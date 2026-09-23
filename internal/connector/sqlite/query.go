package sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// queryVerbs are the leading keywords that route a statement to the Query
// path (returning a result set); everything else goes to the Exec path and
// reports rows_affected.
var queryVerbs = map[string]bool{
	"SELECT": true, "PRAGMA": true, "WITH": true,
	"EXPLAIN": true, "VALUES": true, "TABLE": true,
}

func isQuery(sqlText string) bool {
	fields := strings.Fields(strings.TrimSpace(sqlText))
	if len(fields) == 0 {
		return false
	}
	return queryVerbs[strings.ToUpper(fields[0])]
}

// resolveTarget loads the config and resolves a connection from
// -c/--conn (falling back to default_connection).
func resolveTarget(cmd *cobra.Command) (*Config, string, Connection, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, "", Connection{}, err
	}
	name, conn, err := resolve(cfg, cli.FlagString(cmd, "conn"))
	if err != nil {
		return nil, "", Connection{}, err
	}
	return cfg, name, conn, nil
}

// columnInfo is an element of columns in query data.
type columnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func newQueryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `query "SQL"`,
		Short: "Execute SQL (SELECT-like statements return a result set, others report rows_affected)",
		Args:  cli.ExactArgs(1, `"SQL"`, "sql"),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			db, _, err := openDB(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			if isQuery(args[0]) {
				return runRows(cmd, ctx, db, name, args[0], start)
			}
			return runExec(cmd, ctx, db, name, args[0], start)
		},
	}
}

// runRows executes a query statement, truncating at --limit and setting
// meta.truncated.
func runRows(cmd *cobra.Command, ctx context.Context, db *sql.DB, connName, sqlText string, start time.Time) error {
	rs, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		return classifyErr(err, "query failed")
	}
	defer func() { _ = rs.Close() }()

	colTypes, err := rs.ColumnTypes()
	if err != nil {
		return classifyErr(err, "query failed")
	}
	cols := make([]columnInfo, len(colTypes))
	names := make([]string, len(colTypes))
	for i, ct := range colTypes {
		cols[i] = columnInfo{Name: ct.Name(), Type: ct.DatabaseTypeName()}
		names[i] = ct.Name()
	}

	limit := cli.FlagLimit(cmd)
	rows := make([][]any, 0)
	truncated := false
	for rs.Next() {
		// Read one extra row to detect truncation.
		if limit > 0 && len(rows) >= limit {
			truncated = true
			break
		}
		vals := make([]any, len(colTypes))
		ptrs := make([]any, len(colTypes))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return classifyErr(err, "failed to read results")
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		rows = append(rows, vals)
	}
	if err := rs.Err(); err != nil {
		return classifyErr(err, "failed to read results")
	}

	data := map[string]any{
		"columns":       cols,
		"rows":          rows,
		"row_count":     len(rows),
		"rows_affected": 0,
	}
	return cli.RenderResult(cmd, &output.Result{
		Columns:  names,
		Rows:     rows,
		JSONData: data,
	}, meta(connName, start, truncated))
}

// runExec executes a non-query statement and reports rows_affected. Writes
// on readonly connections are rejected by SQLite via mode=ro, and the
// error is classified as READONLY_VIOLATION.
func runExec(cmd *cobra.Command, ctx context.Context, db *sql.DB, connName, sqlText string, start time.Time) error {
	res, err := db.ExecContext(ctx, sqlText)
	if err != nil {
		return classifyErr(err, "execution failed")
	}
	affected, _ := res.RowsAffected()
	data := map[string]any{
		"columns":       []columnInfo{},
		"rows":          [][]any{},
		"row_count":     0,
		"rows_affected": affected,
	}
	return cli.RenderResult(cmd, &output.Result{
		Message:  "OK, rows affected: " + strconv.FormatInt(affected, 10),
		JSONData: data,
	}, meta(connName, start, false))
}

func newTablesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tables",
		Short: "List tables and views in the database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), cli.FlagTimeout(cmd))
			defer cancel()
			db, _, err := openDB(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			rs, err := db.QueryContext(ctx,
				`SELECT name, type FROM sqlite_master
				 WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
				 ORDER BY type, name`)
			if err != nil {
				return classifyErr(err, "query failed")
			}
			defer func() { _ = rs.Close() }()
			rows := make([][]any, 0)
			for rs.Next() {
				var tbl, typ string
				if err := rs.Scan(&tbl, &typ); err != nil {
					return classifyErr(err, "failed to read results")
				}
				rows = append(rows, []any{tbl, typ})
			}
			if err := rs.Err(); err != nil {
				return classifyErr(err, "failed to read results")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "type"},
				Rows:    rows,
			}, meta(name, start, false))
		},
	}
}

func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema [table]",
		Short: "Print DDL: the whole database without arguments, a single table otherwise",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), cli.FlagTimeout(cmd))
			defer cancel()
			db, _, err := openDB(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			query := `SELECT sql FROM sqlite_master WHERE sql IS NOT NULL`
			params := []any{}
			if len(args) == 1 {
				query += ` AND name = ?`
				params = append(params, args[0])
			}
			query += ` ORDER BY rowid`
			rs, err := db.QueryContext(ctx, query, params...)
			if err != nil {
				return classifyErr(err, "query failed")
			}
			defer func() { _ = rs.Close() }()
			var ddls []string
			for rs.Next() {
				var ddl string
				if err := rs.Scan(&ddl); err != nil {
					return classifyErr(err, "failed to read results")
				}
				ddls = append(ddls, ddl)
			}
			if err := rs.Err(); err != nil {
				return classifyErr(err, "failed to read results")
			}
			if len(args) == 1 && len(ddls) == 0 {
				return output.NewError(output.CodeQueryError,
					"table or view not found: "+args[0], "list objects with muxcat sqlite tables")
			}
			ddl := strings.Join(ddls, ";\n\n")
			if ddl != "" {
				ddl += ";"
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    ddl,
				JSONData: map[string]any{"ddl": ddl},
			}, meta(name, start, false))
		},
	}
}
