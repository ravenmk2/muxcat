// Package mongodb implements muxcat's MongoDB connector (MongoDB 6 / 7 /
// 8), backed by the official go.mongodb.org/mongo-driver/v2 driver.
package mongodb

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

// masterKey is the master key resolver, a package-level variable so tests
// can replace it (keychain-less environments).
var masterKey = secret.MasterKey

func init() {
	connector.Register("mongodb", New)
}

// New builds the mongodb connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "mongodb",
		Short: "MongoDB connector",
		Long: `MongoDB connector for MongoDB 6 / 7 / 8 (prioritizing 8), backed
by the official go.mongodb.org/mongo-driver/v2 driver.

Quickstart:
  1. muxcat mongodb conn add local --host 127.0.0.1 --set-default
  2. muxcat mongodb conn test local

A connection carries credentials (encrypted at rest, never echoed), an
auth source, and an optional default database. Phase 1 covers connection
management; query commands land in a later phase.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
	)
	return c
}

// meta builds the envelope meta for mongodb commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "mongodb",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// connect builds a client from an instance + connection and verifies it
// with a ping. The enc:v1: password blob is decrypted in memory only; no
// URI string carrying credentials is ever built.
func connect(ctx context.Context, cfg *Config, conn Connection) (*mongo.Client, error) {
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(inst.Hosts))
	for _, h := range inst.Hosts {
		hosts = append(hosts, normalizeHost(h))
	}
	opts := options.Client().SetHosts(hosts)
	if conn.Username != "" || conn.Password != "" {
		cred := options.Credential{Username: conn.Username, AuthSource: conn.AuthSource}
		if conn.Password != "" {
			key, _, err := masterKey()
			if err != nil {
				return nil, err
			}
			plain, err := secret.Decrypt(key, conn.Password)
			if err != nil {
				return nil, err
			}
			cred.Password = string(plain)
			cred.PasswordSet = true
		}
		opts.SetAuth(cred)
	}
	if inst.TLS {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	if inst.ReplicaSet != "" {
		opts.SetReplicaSet(inst.ReplicaSet)
	}
	if conn.Timeout != "" {
		if d, err := time.ParseDuration(conn.Timeout); err == nil && d > 0 {
			opts.SetConnectTimeout(d)
			opts.SetTimeout(d)
			opts.SetServerSelectionTimeout(d)
		}
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, output.NewError(output.CodeConnectFailed,
			"failed to create the mongo client: "+err.Error(), "")
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, classifyErr(err, "failed to connect to "+addr(inst))
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
	var srvErr mongo.ServerError
	if errors.As(err, &srvErr) && (srvErr.HasErrorCode(13) || srvErr.HasErrorCode(18)) {
		return output.NewError(output.CodeAuthFailed, msg,
			"check the username/password/authSource of the connection")
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "auth"):
		return output.NewError(output.CodeAuthFailed, msg,
			"check the username/password/authSource of the connection")
	case isDialErr(low):
		return output.NewError(output.CodeConnectFailed, msg,
			"check host/port and that the server is reachable")
	case mongo.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded):
		return output.NewError(output.CodeTimeout, msg, "increase --timeout or check the server")
	case strings.Contains(low, "server selection"):
		return output.NewError(output.CodeConnectFailed, msg,
			"check host/port and that the server is reachable")
	default:
		return output.NewError(output.CodeQueryError, msg, "")
	}
}

func isDialErr(low string) bool {
	return strings.Contains(low, "dial tcp") ||
		strings.Contains(low, "connection refused") ||
		strings.Contains(low, "no such host")
}
