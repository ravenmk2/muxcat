package sqlite

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// withDB resolves the target connection and opens the database, running fn
// with the open handle; used by the admin commands.
func withDB(cmd *cobra.Command, fn func(ctx context.Context, db *sql.DB, path, connName string, start time.Time) error) error {
	start := time.Now()
	cfg, name, conn, err := resolveTarget(cmd)
	if err != nil {
		return err
	}
	timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	db, path, err := openDB(ctx, cfg, conn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return fn(ctx, db, path, name, start)
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show database status (SQLite version, journal mode, page and file sizes)",
		Long: `Show a curated, fixed-order status of the database file:
SQLite version, journal mode, page size/count, freelist pages,
logical size (page_size * page_count), on-disk file size and WAL
presence. File metrics are omitted for :memory: connections.`,
		Args: cobra.NoArgs,
		Example: `  muxcat sqlite status
  muxcat sqlite status -c ro --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, path, name string, start time.Time) error {
				var version, journalMode string
				var pageSize, pageCount, freelist int64
				if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
					return classifyErr(err, "query failed")
				}
				if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
					return classifyErr(err, "query failed")
				}
				if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
					return classifyErr(err, "query failed")
				}
				if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
					return classifyErr(err, "query failed")
				}
				if err := db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freelist); err != nil {
					return classifyErr(err, "query failed")
				}
				sizeBytes := pageSize * pageCount

				metrics := map[string]any{
					"version":        version,
					"journal_mode":   journalMode,
					"page_size":      pageSize,
					"page_count":     pageCount,
					"freelist_count": freelist,
					"size_bytes":     sizeBytes,
				}
				rows := [][]any{
					{"version", version},
					{"journal_mode", journalMode},
					{"page_size", pageSize},
					{"page_count", pageCount},
					{"freelist_count", freelist},
					{"size_bytes", sizeBytes},
				}
				if path != ":memory:" {
					var fileSize int64
					if fi, err := os.Stat(path); err == nil {
						fileSize = fi.Size()
					}
					_, walErr := os.Stat(path + "-wal")
					wal := walErr == nil
					metrics["file_size_bytes"] = fileSize
					metrics["wal"] = wal
					rows = append(rows,
						[]any{"file_size_bytes", fileSize},
						[]any{"wal", wal},
					)
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"name", "value"},
					Rows:     rows,
					JSONData: map[string]any{"metrics": metrics},
				}, meta(name, start, false))
			})
		},
	}
}

func newDescribeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "describe <table>",
		Short: "Show column definitions of a table or view (PRAGMA table_info)",
		Args:  cli.ExactArgs(1, "<table>", "table"),
		Example: `  muxcat sqlite describe users
  muxcat sqlite describe users --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, _, name string, start time.Time) error {
				var exists int
				if err := db.QueryRowContext(ctx,
					`SELECT count(*) FROM sqlite_master
					 WHERE type IN ('table','view') AND name = ?`, args[0]).Scan(&exists); err != nil {
					return classifyErr(err, "query failed")
				}
				if exists == 0 {
					return output.NewError(output.CodeQueryError,
						"table or view not found: "+args[0], "list objects with muxcat sqlite tables")
				}
				rs, err := db.QueryContext(ctx,
					`SELECT cid, name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, args[0])
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				for rs.Next() {
					var cid, notnull, pk int64
					var colName, colType string
					var dflt sql.NullString
					if err := rs.Scan(&cid, &colName, &colType, &notnull, &dflt, &pk); err != nil {
						return classifyErr(err, "failed to read results")
					}
					var dfltVal any
					if dflt.Valid {
						dfltVal = dflt.String
					}
					rows = append(rows, []any{cid, colName, colType, notnull, dfltVal, pk})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns: []string{"cid", "name", "type", "notnull", "default", "pk"},
					Rows:    rows,
				}, meta(name, start, false))
			})
		},
	}
}
