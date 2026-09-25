package mysql

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// queryVerbs are the leading keywords that route a statement to the Query
// path (returning a result set); everything else goes to the Exec path and
// reports rows_affected.
var queryVerbs = map[string]bool{
	"SELECT": true, "SHOW": true, "DESC": true, "DESCRIBE": true,
	"EXPLAIN": true, "WITH": true, "VALUES": true, "TABLE": true,
}

func isQuery(sqlText string) bool {
	return queryVerbs[firstKeyword(sqlText)]
}

// stdinIsTTY reports whether stdin is a terminal; a variable so tests can
// stub it.
var stdinIsTTY = func() bool {
	return isatty.IsTerminal(os.Stdin.Fd())
}

// resolveTarget loads the config and resolves a connection from
// -c/--conn (falling back to defaultConnection).
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

// resolveSQLInput resolves the SQL text: positional argument > --file
// <path> > --file - (stdin) > implicit stdin when it is not a TTY.
func resolveSQLInput(cmd *cobra.Command, args []string) (string, error) {
	file := cli.FlagString(cmd, "file")
	if len(args) == 1 && file != "" {
		return "", output.NewError(output.CodeMissingArgument,
			"SQL given both as an argument and via --file",
			"choose one: muxcat mysql query \"SQL\" or muxcat mysql query --file <path>")
	}
	if len(args) == 1 {
		return args[0], nil
	}
	if file != "" {
		if file == "-" {
			return readStdin(cmd)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", output.NewError(output.CodeMissingArgument,
				"cannot read --file "+file+": "+err.Error(), "")
		}
		return string(data), nil
	}
	if stdinIsTTY() {
		return "", output.NewError(output.CodeMissingArgument,
			"no SQL provided",
			"usage: muxcat mysql query \"SQL\", muxcat mysql query --file <path>, or echo \"SQL\" | muxcat mysql query")
	}
	return readStdin(cmd)
}

func readStdin(cmd *cobra.Command) (string, error) {
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", output.NewError(output.CodeMissingArgument,
			"cannot read SQL from stdin: "+err.Error(), "")
	}
	return string(data), nil
}

func newQueryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   `query ["SQL"]`,
		Short: "Execute SQL (SELECT-like statements return a result set, others report rows_affected)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			sqlText, err := resolveSQLInput(cmd, args)
			if err != nil {
				return err
			}
			// The readonly guard intercepts writes before any dialing.
			if err := guardQuery(conn, sqlText); err != nil {
				return err
			}
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			db, err := openDB(ctx, cfg, conn, cli.FlagString(cmd, "db"))
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			if isQuery(sqlText) {
				return runRows(cmd, ctx, db, name, sqlText, start)
			}
			return runExec(cmd, ctx, db, name, sqlText, start)
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().String("file", "", "read SQL from a file (- reads from stdin)")
	return c
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
		// Type-aware []byte handling: binary-typed columns keep their raw
		// bytes (text renderers hex them; JSON gets the hex form below),
		// everything else converts to string.
		for i, v := range vals {
			if b, ok := v.([]byte); ok && !output.IsBinaryType(cols[i].Type) {
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
		"rows":          output.BinaryCellsToHex(rows),
		"row_count":     len(rows),
		"rows_affected": 0,
	}
	return cli.RenderResult(cmd, &output.Result{
		Columns:     names,
		ColumnTypes: dbTypes(cols),
		Rows:        rows,
		JSONData:    data,
		CellStyle:   cellStyle,
	}, meta(connName, start, truncated))
}

// dbTypes extracts the type names of query columns.
func dbTypes(cols []columnInfo) []string {
	types := make([]string, len(cols))
	for i, c := range cols {
		types[i] = c.Type
	}
	return types
}

// runExec executes a non-query statement and reports rows_affected. Writes
// on readonly connections are rejected by the client-side guard before
// dialing, and by the server-side read-only session as a fallback.
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
	c := &cobra.Command{
		Use:   "tables",
		Short: "List tables and views in the database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			db, err := openDB(ctx, cfg, conn, cli.FlagString(cmd, "db"))
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			rs, err := db.QueryContext(ctx,
				`SELECT TABLE_NAME, TABLE_TYPE FROM information_schema.TABLES
				 WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_TYPE, TABLE_NAME`)
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
	c.Flags().String("db", "", "override the connection's database for this invocation")
	return c
}

func newSchemaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "schema [table]",
		Short: "Print DDL: all tables without arguments, a single table otherwise",
		Args:  cobra.MaximumNArgs(1),
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
			db, err := openDB(ctx, cfg, conn, cli.FlagString(cmd, "db"))
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			if len(args) == 1 {
				ddl, err := showCreateTable(ctx, db, args[0])
				if err != nil {
					var myErr *gomysql.MySQLError
					if errors.As(err, &myErr) && myErr.Number == 1146 {
						return output.NewError(output.CodeQueryError,
							"table not found: "+args[0], "list tables with muxcat mysql tables")
					}
					return classifyErr(err, "query failed")
				}
				return renderDDL(cmd, name, ddl, start)
			}

			rs, err := db.QueryContext(ctx,
				`SELECT TABLE_NAME FROM information_schema.TABLES
				 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME`)
			if err != nil {
				return classifyErr(err, "query failed")
			}
			tables := make([]string, 0)
			for rs.Next() {
				var tbl string
				if err := rs.Scan(&tbl); err != nil {
					_ = rs.Close()
					return classifyErr(err, "failed to read results")
				}
				tables = append(tables, tbl)
			}
			_ = rs.Close()
			if err := rs.Err(); err != nil {
				return classifyErr(err, "failed to read results")
			}

			ddls := make([]string, 0, len(tables))
			for _, tbl := range tables {
				ddl, err := showCreateTable(ctx, db, tbl)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				ddls = append(ddls, ddl)
			}
			return renderDDL(cmd, name, strings.Join(ddls, ";\n\n"), start)
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	return c
}

// showCreateTable returns the CREATE TABLE DDL of one table.
func showCreateTable(ctx context.Context, db *sql.DB, table string) (string, error) {
	var tbl, ddl string
	err := db.QueryRowContext(ctx, "SHOW CREATE TABLE "+quoteIdent(table)).Scan(&tbl, &ddl)
	if err != nil {
		return "", err
	}
	return ddl, nil
}

// quoteIdent quotes a MySQL identifier with backticks.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func renderDDL(cmd *cobra.Command, connName, ddl string, start time.Time) error {
	if ddl != "" {
		ddl += ";"
	}
	return cli.RenderResult(cmd, &output.Result{
		Value:    ddl,
		JSONData: map[string]any{"ddl": ddl},
		Syntax:   "sql",
	}, meta(connName, start, false))
}
