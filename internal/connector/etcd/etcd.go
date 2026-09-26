// Package etcd implements muxcat's etcd connector (v3 API only), backed
// by the pure-Go go.etcd.io/etcd/client/v3 driver (gRPC). Scope: KV
// basics (get/put/del), bounded watch, and cluster inspection (endpoint
// status/health, member list, alarm list/disarm). Lease management,
// compact/defrag and auth management are out of scope; the structure
// leaves room for them.
package etcd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc/grpclog"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

// masterKey is the master key resolver, a package-level variable so tests
// can replace it (keychain-less environments).
var masterKey = secret.MasterKey

func init() {
	// Silence gRPC's internal logging; connection failures surface as
	// structured errors instead of stderr log lines.
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard))
	connector.Register("etcd", New)
}

// New builds the etcd connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "etcd",
		Short: "etcd connector",
		Long: `etcd connector (v3 API only). Scope: KV basics (get/put/del),
bounded watch, and cluster inspection (endpoint status/health,
member list, alarm list/disarm). Lease management, compact/defrag
and auth management are out of scope.

Quickstart:
  1. muxcat etcd conn add local --endpoints 127.0.0.1:2379 --set-default
  2. muxcat etcd put mykey hello
  3. muxcat etcd get mykey

A connection carries credentials (encrypted at rest, never echoed) and
usage policies: readonly allows reads only, and dangerous operations
(del --prefix) are blocked unless the connection sets allowDangerous.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newGetCmd(),
		newPutCmd(),
		newDelCmd(),
		newWatchCmd(),
		newEndpointCmd(),
		newMemberCmd(),
		newAlarmCmd(),
	)
	return c
}

// meta builds the envelope meta for etcd commands.
func meta(_ *Config, _ Connection, connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "etcd",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// clientConfig builds a clientv3 config from an instance + connection,
// decrypting the enc:v1: password blob when present.
func clientConfig(cfg *Config, conn Connection, dialTimeout time.Duration) (*clientv3.Config, error) {
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	cc := &clientv3.Config{
		Endpoints:   inst.Endpoints,
		DialTimeout: dialTimeout,
		Username:    conn.Username,
		// Silence the client's internal zap logging; failures surface as
		// structured errors instead of stderr log lines.
		Logger: zap.NewNop(),
	}
	if encPassword := conn.Password; encPassword != "" {
		key, _, err := masterKey()
		if err != nil {
			return nil, err
		}
		plain, err := secret.Decrypt(key, encPassword)
		if err != nil {
			return nil, err
		}
		cc.Password = string(plain)
	}
	if inst.TLS {
		tlsConfig, err := loadTLSConfig(inst)
		if err != nil {
			return nil, err
		}
		cc.TLS = tlsConfig
	}
	return cc, nil
}

// loadTLSConfig builds the TLS configuration from the instance's CA /
// client certificate file paths.
func loadTLSConfig(inst Instance) (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS12}
	if inst.CACert != "" {
		pem, err := os.ReadFile(inst.CACert)
		if err != nil {
			return nil, output.NewError(output.CodeConfigInvalid,
				"cannot read CA certificate "+inst.CACert+": "+err.Error(), "")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, output.NewError(output.CodeConfigInvalid,
				"CA certificate "+inst.CACert+" contains no PEM certificates", "")
		}
		t.RootCAs = pool
	}
	if (inst.Cert == "") != (inst.Key == "") {
		return nil, output.NewError(output.CodeConfigInvalid,
			"client TLS requires both --cert and --key", "")
	}
	if inst.Cert != "" {
		pair, err := tls.LoadX509KeyPair(inst.Cert, inst.Key)
		if err != nil {
			return nil, output.NewError(output.CodeConfigInvalid,
				"cannot load client certificate: "+err.Error(), "")
		}
		t.Certificates = []tls.Certificate{pair}
	}
	return t, nil
}

// openClient builds a client and verifies it with a maintenance Status
// call against the first endpoint; failures are classified (dial refused
// → CONNECT_FAILED, auth errors → AUTH_FAILED, ...).
func openClient(ctx context.Context, cfg *Config, conn Connection, dialTimeout time.Duration) (*clientv3.Client, error) {
	cc, err := clientConfig(cfg, conn, dialTimeout)
	if err != nil {
		return nil, err
	}
	client, err := clientv3.New(*cc)
	if err != nil {
		return nil, classifyErr(err, "failed to connect")
	}
	if _, err := client.Status(ctx, cc.Endpoints[0]); err != nil {
		_ = client.Close()
		return nil, classifyErr(err, "failed to connect")
	}
	return client, nil
}

// queryTimeout resolves the command timeout: the connection-level timeout
// overrides the global --timeout.
func queryTimeout(conn Connection, flagTimeout time.Duration) (time.Duration, error) {
	if conn.Timeout == "" {
		return flagTimeout, nil
	}
	d, err := time.ParseDuration(conn.Timeout)
	if err != nil || d <= 0 {
		return 0, output.NewError(output.CodeConfigInvalid,
			"invalid connection timeout: "+conn.Timeout, "examples: 5s, 1m (must be > 0)")
	}
	return d, nil
}

// classifyErr classifies driver errors into muxcat error codes.
func classifyErr(err error, prefix string) *output.Error {
	msg := err.Error()
	if prefix != "" {
		msg = prefix + ": " + msg
	}
	switch {
	case isAuthErr(err):
		return output.NewError(output.CodeAuthFailed, msg, "check the username/password of the connection")
	case isPermissionErr(err):
		return output.NewError(output.CodeAuthFailed, msg, "the user lacks permission for this operation")
	case isTimeoutErr(err):
		return output.NewError(output.CodeTimeout, msg, "increase --timeout or check the server")
	case isDialErr(err):
		return output.NewError(output.CodeConnectFailed, msg, "check endpoints and that the server is reachable")
	default:
		return output.NewError(output.CodeQueryError, msg, "")
	}
}

func isAuthErr(err error) bool {
	return errors.Is(err, rpctypes.ErrGRPCAuthFailed) ||
		errors.Is(err, rpctypes.ErrAuthFailed) ||
		errors.Is(err, rpctypes.ErrGRPCInvalidAuthToken) ||
		errors.Is(err, rpctypes.ErrInvalidAuthToken) ||
		strings.Contains(err.Error(), "authentication failed") ||
		strings.Contains(err.Error(), "invalid auth token")
}

func isPermissionErr(err error) bool {
	return errors.Is(err, rpctypes.ErrGRPCPermissionDenied) ||
		errors.Is(err, rpctypes.ErrPermissionDenied) ||
		strings.Contains(err.Error(), "permission denied")
}

func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "context deadline exceeded") {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isDialErr(err error) bool {
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "dial tcp") ||
		strings.Contains(low, "connection refused") ||
		strings.Contains(low, "error while dialing") ||
		strings.Contains(low, "no such host")
}
