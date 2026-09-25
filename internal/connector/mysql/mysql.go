// Package mysql implements muxcat's MySQL connector (MySQL 5.7+ / 8.x),
// backed by the pure-Go github.com/go-sql-driver/mysql driver via
// database/sql.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

// masterKey is the master key resolver, a package-level variable so tests
// can replace it (keychain-less environments).
var masterKey = secret.MasterKey

func init() {
	// Silence the driver's stderr logging; connection failures surface as
	// structured errors instead of stderr log lines.
	_ = gomysql.SetLogger(new(gomysql.NopLogger))
	connector.Register("mysql", New)
}

// New builds the mysql connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "mysql",
		Short: "MySQL connector",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newQueryCmd(),
		newExecuteCmd(),
		newTablesCmd(),
		newSchemaCmd(),
		newDatabasesCmd(),
		newStatusCmd(),
		newVariablesCmd(),
		newProcesslistCmd(),
		newKillCmd(),
		newUsersCmd(),
		newGrantsCmd(),
		newEngineCmd(),
		newReplicationCmd(),
	)
	return c
}

// meta builds the envelope meta for mysql commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "mysql",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// dsn builds the driver DSN from an instance + connection, decrypting the
// enc:v1: password blob when present. dbOverride replaces the connection's
// database for this invocation only.
func dsn(inst Instance, conn Connection, dbOverride string) (string, error) {
	cfg := gomysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = addr(inst)
	cfg.User = conn.Username
	cfg.DBName = conn.Database
	if dbOverride != "" {
		cfg.DBName = dbOverride
	}
	cfg.ParseTime = true
	cfg.MultiStatements = false
	if inst.TLS {
		cfg.TLSConfig = "true"
	} else {
		cfg.TLSConfig = "false"
	}
	if conn.Password != "" {
		key, _, err := masterKey()
		if err != nil {
			return "", err
		}
		plain, err := secret.Decrypt(key, conn.Password)
		if err != nil {
			return "", err
		}
		cfg.Passwd = string(plain)
	}
	return cfg.FormatDSN(), nil
}

// openDB opens a connection and verifies it with PING; failures are
// classified (dial refused → CONNECT_FAILED, 1045 → AUTH_FAILED, ...). On
// readonly connections the session is then forced read-only server-side.
func openDB(ctx context.Context, cfg *Config, conn Connection, dbOverride string) (*sql.DB, error) {
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	d, err := dsn(inst, conn, dbOverride)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", d)
	if err != nil {
		return nil, output.NewError(output.CodeConnectFailed,
			"failed to open database: "+err.Error(), "")
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, classifyErr(err, "failed to connect to "+addr(inst))
	}
	if conn.Readonly {
		if err := enforceReadonly(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

// enforceReadonly forces the session read-only server-side. MySQL 5.7 and
// MariaDB name the variable tx_read_only (MySQL 8.0 renamed it to
// transaction_read_only), so an ER_UNKNOWN_SYSTEM_VARIABLE (1193) retries
// with the legacy name; any other failure is classified.
func enforceReadonly(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "SET SESSION transaction_read_only = 1")
	if err == nil {
		return nil
	}
	var myErr *gomysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == 1193 {
		_, err = db.ExecContext(ctx, "SET SESSION tx_read_only = 1")
	}
	if err != nil {
		return classifyErr(err, "failed to enforce the read-only session")
	}
	return nil
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
	var myErr *gomysql.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1045: // ER_ACCESS_DENIED_ERROR
			return output.NewError(output.CodeAuthFailed, msg, "check the username/password of the connection")
		case 1290, 1792, 1836: // read-only server / session / running statements
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
