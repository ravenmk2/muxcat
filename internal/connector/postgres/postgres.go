// Package postgres implements muxcat's PostgreSQL connector (PG 12+),
// backed by the pure-Go github.com/jackc/pgx/v5 driver in stdlib mode
// (database/sql).
package postgres

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

// masterKey is the master key resolver, a package-level variable so tests
// can replace it (keychain-less environments).
var masterKey = secret.MasterKey

func init() {
	connector.Register("postgres", New)
}

// New builds the postgres connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "postgres",
		Short: "PostgreSQL connector",
		Long: `PostgreSQL connector for PG 12+, backed by the pure-Go
jackc/pgx/v5 driver in database/sql (stdlib) mode.

Quickstart:
  1. muxcat postgres conn add local --host 127.0.0.1 --username postgres --set-default
  2. muxcat postgres query "SELECT version()"
  3. muxcat postgres tables

A connection carries credentials (encrypted at rest, never echoed), an
optional default database (empty falls back to "postgres"), and usage
policies: a readonly connection passes read statements only (a
client-side guard plus the server-side default_transaction_read_only
runtime parameter applied to every physical connection), and
execute/kill are refused on it. Every invocation opens its own
connection; statements are single-shot (no multi-statement scripts, no
transactions).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newQueryCmd(),
		newExecuteCmd(),
		newDatabasesCmd(),
		newSchemasCmd(),
		newTablesCmd(),
		newSchemaCmd(),
		newStatusCmd(),
		newSettingsCmd(),
		newActivityCmd(),
		newKillCmd(),
		newRolesCmd(),
		newGrantsCmd(),
		newExtensionsCmd(),
		newLocksCmd(),
		newReplicationCmd(),
	)
	return c
}

// meta builds the envelope meta for postgres commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "postgres",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// pgConfig builds the driver config from an instance + connection,
// decrypting the enc:v1: password blob when present. The config is built
// programmatically (no hand-assembled DSN), so a password with special
// characters needs no escaping; the plaintext password lives in memory
// only and never enters an error message or log. dbOverride replaces the
// connection's database for this invocation only; an empty database falls
// back to "postgres".
func pgConfig(inst Instance, conn Connection, dbOverride string) (*pgx.ConnConfig, error) {
	// Only the constant sslmode token goes through the conn-string parser;
	// every user-controlled field is assigned structurally afterwards.
	cfg, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid,
			"failed to build the driver config: "+err.Error(), "")
	}
	cfg.Host = inst.Host
	cfg.Port = uint16(inst.Port) // validated to 1-65535 at conn add / schema
	if inst.TLS {
		// sslmode=require semantics (libpq): encrypt without CA
		// verification, matching mysql's tls=true. Assigned structurally
		// because ParseConfig resolves TLS against the parse-time default
		// host, which is a unix socket directory on macOS/Linux — and TLS
		// settings are silently ignored for unix sockets there.
		cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // intentional: sslmode=require encrypts without verification
	}
	cfg.User = conn.Username
	cfg.Database = conn.Database
	if cfg.Database == "" {
		cfg.Database = "postgres"
	}
	if dbOverride != "" {
		cfg.Database = dbOverride
	}
	if conn.Readonly {
		// A runtime parameter is applied by the server to every physical
		// connection of the pool, immune to the database/sql pool-vs-SET
		// SESSION pitfall.
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	if conn.Password != "" {
		key, _, err := masterKey()
		if err != nil {
			return nil, err
		}
		plain, err := secret.Decrypt(key, conn.Password)
		if err != nil {
			return nil, err
		}
		cfg.Password = string(plain)
	}
	return cfg, nil
}

// openDB opens a connection and verifies it with PING; failures are
// classified (dial refused → CONNECT_FAILED, 28P01 → AUTH_FAILED, ...).
// On readonly connections the read-only runtime parameter was already
// attached to the driver config (see pgConfig).
func openDB(ctx context.Context, cfg *Config, conn Connection, dbOverride string) (*sql.DB, error) {
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	pgc, err := pgConfig(inst, conn, dbOverride)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*pgc)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, classifyErr(err, "failed to connect to "+addr(inst))
	}
	return db, nil
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

// classifyErr classifies driver errors into muxcat error codes. The error
// text of a pgconn.ConnectError carries host/user/database but never the
// password (it is only ever assigned structurally), so it is safe to
// surface.
func classifyErr(err error, prefix string) *output.Error {
	msg := err.Error()
	if prefix != "" {
		msg = prefix + ": " + msg
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000": // invalid_password / invalid_authorization_specification
			return output.NewError(output.CodeAuthFailed, msg, "check the username/password of the connection")
		case "25006": // read_only_sql_transaction
			return output.NewError(output.CodeReadonlyViolation, msg,
				"the server rejected a write on a read-only session; use a writable connection (-c)")
		default:
			return output.NewError(output.CodeQueryError, msg, "")
		}
	}
	switch {
	case isTimeoutErr(err):
		return output.NewError(output.CodeTimeout, msg, "increase --timeout or check the server")
	case isDialErr(err):
		return output.NewError(output.CodeConnectFailed, msg, "check host/port and that the server is reachable")
	default:
		return output.NewError(output.CodeQueryError, msg, "")
	}
}

func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isDialErr(err error) bool {
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "dial tcp") ||
		strings.Contains(low, "connection refused") ||
		strings.Contains(low, "no such host")
}
