// Package redis implements muxcat's Redis connector for standalone
// instances (Redis 6+, including Redis 8), backed by the pure-Go
// github.com/redis/go-redis/v9 driver. Cluster / Sentinel / Pub-Sub /
// Monitor are out of scope for the first iteration.
package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

// masterKey is the master key resolver, a package-level variable so tests
// can replace it (keychain-less environments).
var masterKey = secret.MasterKey

func init() {
	// Silence the driver's internal pool/dial logging; connection failures
	// surface as structured errors instead of stderr log lines.
	goredis.SetLogger(discardLogger{})
	connector.Register("redis", New)
}

// discardLogger implements go-redis's internal.Logging by dropping messages.
type discardLogger struct{}

func (discardLogger) Printf(context.Context, string, ...any) {}

// New builds the redis connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "redis",
		Short: "Redis connector",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newExecCmd(),
		newGetCmd(),
		newSetCmd(),
		newDelCmd(),
		newScanCmd(),
		newTypeCmd(),
		newTTLCmd(),
		newInfoCmd(),
		newDBSizeCmd(),
		newHGetCmd(),
		newHGetAllCmd(),
		newLRangeCmd(),
		newSMembersCmd(),
		newZRangeCmd(),
		newEvalCmd(),
		newConfigGroupCmd(),
	)
	return c
}

// meta builds the envelope meta for redis commands, reporting the effective
// logical database (after --db override and legacy fallbacks) when the
// connection's instance resolves; conn-management commands that have no
// single instance in scope (conn ls) omit the db field.
func meta(cfg *Config, conn Connection, connName string, start time.Time, truncated bool) output.Meta {
	m := output.Meta{
		Connector:  "redis",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
	if inst, err := cfg.instanceOf(conn); err == nil {
		db := effectiveDB(inst, conn)
		m.DB = &db
	}
	return m
}

// clientOptions builds go-redis options from an instance + connection,
// decrypting the enc:v1: password blob when present.
func clientOptions(cfg *Config, conn Connection) (*goredis.Options, error) {
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	opts := &goredis.Options{
		Addr:     addr(inst),
		Username: effectiveUsername(inst, conn),
		DB:       effectiveDB(inst, conn),
	}
	if encPassword := effectivePassword(inst, conn); encPassword != "" {
		key, _, err := masterKey()
		if err != nil {
			return nil, err
		}
		plain, err := secret.Decrypt(key, encPassword)
		if err != nil {
			return nil, err
		}
		opts.Password = string(plain)
	}
	if inst.TLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return opts, nil
}

// openClient builds a client and verifies it with PING; failures are
// classified (dial refused → CONNECT_FAILED, NOAUTH → AUTH_FAILED, ...).
func openClient(ctx context.Context, cfg *Config, conn Connection) (*goredis.Client, error) {
	opts, err := clientOptions(cfg, conn)
	if err != nil {
		return nil, err
	}
	client := goredis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		if isNoPerm(err) {
			// A NOPERM PING still proves TCP + AUTH succeeded;
			// least-privilege ACL users (e.g. +@read only) cannot PING
			// but can run their permitted commands.
			return client, nil
		}
		_ = client.Close()
		return nil, classifyErr(err, "failed to connect to "+opts.Addr)
	}
	return client, nil
}

// isNoPerm reports whether err is a Redis NOPERM ACL error.
func isNoPerm(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "NOPERM")
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
	case isTimeoutErr(err):
		return output.NewError(output.CodeTimeout, msg, "increase --timeout or check the server")
	case isReadonlyErr(err):
		return output.NewError(output.CodeReadonlyViolation, msg, "the server rejected a write (e.g. replica); use a writable connection")
	case isDialErr(err):
		return output.NewError(output.CodeConnectFailed, msg, "check host/port and that the server is reachable")
	default:
		return output.NewError(output.CodeQueryError, msg, "")
	}
}

func isAuthErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "NOAUTH") ||
		strings.Contains(msg, "WRONGPASS") ||
		strings.Contains(msg, "NOPERM")
}

func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isReadonlyErr matches the server-side READONLY error (writes against a
// replica); distinct from the connector-side readonly interception.
func isReadonlyErr(err error) bool {
	return strings.Contains(err.Error(), "READONLY")
}

func isDialErr(err error) bool {
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "dial tcp") ||
		strings.Contains(low, "connection refused") ||
		strings.Contains(low, "no such host")
}
