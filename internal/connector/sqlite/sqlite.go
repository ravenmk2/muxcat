// Package sqlite implements muxcat's SQLite connector.
// Dual drivers: CGO builds use mattn/go-sqlite3, pure-Go builds use
// modernc.org/sqlite, switched automatically by build constraint; business
// code only ever sees driverName.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
)

func init() {
	connector.Register("sqlite", New)
}

// New builds the sqlite connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "sqlite",
		Short: "SQLite connector",
		Long: `SQLite connector for local database files. CGO builds use
mattn/go-sqlite3, pure-Go builds use modernc.org/sqlite; the
switch is automatic.

Quickstart:
  1. muxcat sqlite conn add local --path ./app.db --set-default
  2. muxcat sqlite tables
  3. muxcat sqlite query "SELECT * FROM users LIMIT 5"

A connection points at a database file (the path supports ~
expansion) and carries usage policies: a readonly connection
opens the file with mode=ro, so SQLite itself rejects writes.
Every invocation opens the file fresh; statements are
single-shot (no multi-statement scripts, no transactions).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newConnCmd(), newQueryCmd(), newTablesCmd(), newSchemaCmd())
	return c
}

// meta builds the envelope meta for sqlite commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "sqlite",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// openDB opens a connection and verifies it with Ping; failures are
// classified as CONNECT_FAILED (e.g. opening a nonexistent file read-only).
func openDB(ctx context.Context, cfg *Config, conn Connection) (*sql.DB, string, error) {
	path, err := cfg.instancePath(conn)
	if err != nil {
		return nil, "", err
	}
	db, err := sql.Open(driverName, dsn(path, conn.Readonly))
	if err != nil {
		return nil, "", output.NewError(output.CodeConnectFailed,
			"failed to open database: "+err.Error(), "")
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, "", classifyErr(err, "failed to connect to database")
	}
	return db, path, nil
}

// queryTimeout resolves the query timeout: the connection-level timeout
// overrides the global --timeout.
func queryTimeout(conn Connection, flagTimeout time.Duration) (time.Duration, error) {
	if conn.Timeout == "" {
		return flagTimeout, nil
	}
	d, err := time.ParseDuration(conn.Timeout)
	if err != nil {
		return 0, output.NewError(output.CodeConfigInvalid,
			"invalid connection timeout: "+conn.Timeout, "examples: 5s, 1m")
	}
	return d, nil
}

// classifyErr classifies driver errors into muxcat error codes.
func classifyErr(err error, prefix string) *output.Error {
	msg := err.Error()
	if prefix != "" {
		msg = prefix + ": " + msg
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "readonly") || strings.Contains(low, "read-only"):
		return output.NewError(output.CodeReadonlyViolation, msg, "this connection is read-only (mode=ro); writes are rejected")
	case errors.Is(err, context.DeadlineExceeded):
		return output.NewError(output.CodeTimeout, msg, "increase --timeout or check for database file locks")
	default:
		return output.NewError(output.CodeQueryError, msg, "")
	}
}
