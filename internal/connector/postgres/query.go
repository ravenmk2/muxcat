package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// queryVerbs are the leading keywords that route a statement to the Query
// path (returning a result set); everything else goes to the Exec path and
// reports rows_affected.
var queryVerbs = map[string]bool{
	"SELECT": true, "SHOW": true, "EXPLAIN": true,
	"WITH": true, "VALUES": true, "TABLE": true,
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
			"choose one: muxcat postgres query \"SQL\" or muxcat postgres query --file <path>")
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
			"usage: muxcat postgres query \"SQL\", muxcat postgres query --file <path>, or echo \"SQL\" | muxcat postgres query")
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
		Long: `Execute one SQL statement. The statement comes from the positional
argument, --file <path>, --file - (stdin), or a piped stdin. A
statement leading with SELECT/WITH/TABLE/VALUES/EXPLAIN/SHOW
returns a result set; anything else runs via Exec and reports
rows_affected.

Result sets are capped by --limit (meta.truncated reports an early
stop); NULL renders as NULL, bytea columns as 0x hex. On a readonly
connection only read statements pass the client-side guard.
Multi-statement scripts are not supported.`,
		Args: cobra.MaximumNArgs(1),
		Example: `  muxcat postgres query "SELECT id, email FROM users LIMIT 5"
  muxcat postgres query "UPDATE users SET active = true WHERE id = 42"
  muxcat postgres query --file report.sql --json
  echo "SELECT * FROM pg_stat_activity" | muxcat postgres query --db analytics`,
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
	c.Flags().String("db", "", "override the connection's database for this invocation (reconnects to that database)")
	c.Flags().String("file", "", "read SQL from a file (- reads from stdin)")
	return c
}

// newExecuteCmd builds the explicit execution entry: every statement goes
// through Exec unconditionally (result sets are discarded), complementing
// query's first-keyword auto-routing. It is stricter than query on
// readonly connections: everything is rejected, even SELECT.
func newExecuteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   `execute ["SQL"]`,
		Short: "Execute a statement unconditionally via Exec (result sets are discarded)",
		Long: `Execute one statement unconditionally via Exec: even SELECT goes
through Exec, the result set is discarded, and only rows_affected is
reported. The input channels are the same as query (positional
argument, --file <path>, --file -, piped stdin), and --db overrides
the connection's database for this invocation.

On a readonly connection execute is refused outright — SELECT
included; use query for read-only statements.`,
		Args: cobra.MaximumNArgs(1),
		Example: `  muxcat postgres execute "INSERT INTO logs (msg) VALUES ('hello')"
  muxcat postgres execute --file add_index.sql --db app
  echo "TRUNCATE TABLE staging" | muxcat postgres execute`,
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
			if conn.Readonly {
				return output.NewError(output.CodeReadonlyViolation,
					"execute is not allowed on a readonly connection",
					"use query for read-only statements, or use a writable connection (-c)")
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

			return runExec(cmd, ctx, db, name, sqlText, start)
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation (reconnects to that database)")
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
		// Type-aware []byte handling: binary-typed columns (bytea) keep
		// their raw bytes (text renderers hex them; JSON gets the hex form
		// below), everything else converts to string.
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
// dialing, and by the server-side read-only runtime parameter as a
// fallback.
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

// userSchemaFilter excludes the system schemas from information_schema
// queries (pg_catalog carries the catalog tables/views).
const userSchemaFilter = `table_schema NOT IN ('pg_catalog', 'information_schema')`

func newTablesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "tables",
		Short: "List tables and views, optionally filtered to one schema",
		Long: `List tables and views across all user schemas (pg_catalog and
information_schema excluded), from information_schema.tables.
--schema filters to a single schema and drops the schema column.`,
		Args: cobra.NoArgs,
		Example: `  muxcat postgres tables
  muxcat postgres tables --schema public --json`,
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
			db, err := openDB(ctx, cfg, conn, "")
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			schemaName := cli.FlagString(cmd, "schema")
			query := `SELECT table_schema, table_name, table_type FROM information_schema.tables
WHERE ` + userSchemaFilter
			args := []any{}
			if schemaName != "" {
				query += ` AND table_schema = $1`
				args = append(args, schemaName)
			}
			query += ` ORDER BY table_schema, table_type, table_name`
			rs, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				return classifyErr(err, "query failed")
			}
			defer func() { _ = rs.Close() }()
			rows := make([][]any, 0)
			for rs.Next() {
				var sch, tbl, typ string
				if err := rs.Scan(&sch, &tbl, &typ); err != nil {
					return classifyErr(err, "failed to read results")
				}
				if schemaName != "" {
					rows = append(rows, []any{tbl, typ})
				} else {
					rows = append(rows, []any{sch, tbl, typ})
				}
			}
			if err := rs.Err(); err != nil {
				return classifyErr(err, "failed to read results")
			}
			columns := []string{"schema", "name", "type"}
			if schemaName != "" {
				columns = []string{"name", "type"}
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: columns,
				Rows:    rows,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("schema", "", "list only this schema (drops the schema column)")
	return c
}

func newSchemaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "schema [--schema name] [table]",
		Short: "Describe tables: column summary without arguments, full definition of a single table otherwise",
		Long: `Describe table structure (PostgreSQL has no SHOW CREATE TABLE).
Without arguments it prints a column-definition summary of every
user BASE TABLE. With a table name it prints a \d+-style structured
description: columns (name/type/nullable/default), indexes, and
constraints (including foreign keys); JSON is fully structured
({table, schema, columns, indexes, constraints}). --schema selects
the schema, otherwise the search_path applies.`,
		Args: cobra.MaximumNArgs(1),
		Example: `  muxcat postgres schema              # column summary of every user table
  muxcat postgres schema users
  muxcat postgres schema --schema app orders --json`,
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
			db, err := openDB(ctx, cfg, conn, "")
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			schemaName := cli.FlagString(cmd, "schema")
			if len(args) == 1 {
				desc, err := describeTable(ctx, db, schemaName, args[0])
				if err != nil {
					var pgErr *pgconn.PgError
					if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
						return output.NewError(output.CodeQueryError,
							"table not found: "+args[0], "list tables with muxcat postgres tables")
					}
					return classifyErr(err, "query failed")
				}
				return renderTableDescription(cmd, name, desc, start)
			}

			rs, err := db.QueryContext(ctx,
				`SELECT c.table_schema, c.table_name, c.column_name,
				        CASE
				          WHEN c.data_type = 'ARRAY' THEN substring(c.udt_name from 2) || '[]'
				          WHEN c.character_maximum_length IS NOT NULL THEN c.data_type || '(' || c.character_maximum_length || ')'
				          WHEN c.data_type IN ('numeric', 'decimal') AND c.numeric_precision IS NOT NULL THEN c.data_type || '(' || c.numeric_precision || ',' || c.numeric_scale || ')'
				          ELSE c.data_type
				        END,
				        c.is_nullable, c.column_default
				 FROM information_schema.columns c
				 JOIN information_schema.tables t
				   ON t.table_schema = c.table_schema AND t.table_name = c.table_name
				 WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema') AND t.table_type = 'BASE TABLE'
				 ORDER BY c.table_schema, c.table_name, c.ordinal_position`)
			if err != nil {
				return classifyErr(err, "query failed")
			}
			defer func() { _ = rs.Close() }()
			rows := make([][]any, 0)
			tables := make([]map[string]any, 0)
			var curSchema, curTable string
			var curCols []map[string]any
			flush := func() {
				if curTable == "" {
					return
				}
				tables = append(tables, map[string]any{
					"schema": curSchema, "table": curTable, "columns": curCols,
				})
				curCols = nil
			}
			for rs.Next() {
				var sch, tbl, col, typ, nullable string
				var def sql.NullString
				if err := rs.Scan(&sch, &tbl, &col, &typ, &nullable, &def); err != nil {
					return classifyErr(err, "failed to read results")
				}
				rows = append(rows, []any{sch, tbl, col, typ, nullable, nullStr(def)})
				if sch != curSchema || tbl != curTable {
					flush()
					curSchema, curTable = sch, tbl
				}
				curCols = append(curCols, map[string]any{
					"name": col, "type": typ,
					"nullable": nullable == "YES", "default": nullStr(def),
				})
			}
			flush()
			if err := rs.Err(); err != nil {
				return classifyErr(err, "failed to read results")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns:   []string{"schema", "table", "column", "type", "nullable", "default"},
				Rows:      rows,
				JSONData:  map[string]any{"tables": tables},
				CellStyle: cellStyle,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("schema", "", "schema of the table (default: search_path resolution)")
	return c
}

// describeColumn is one column of a single-table description.
type describeColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  any    `json:"default"`
}

// describeIndex is one index of a single-table description.
type describeIndex struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

// describeConstraint is one constraint of a single-table description.
type describeConstraint struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // primary key / unique / foreign key / check / exclude
	Definition string `json:"definition"`
}

// tableDescription is the structured \d+-style description of one table.
type tableDescription struct {
	Schema      string               `json:"schema"`
	Table       string               `json:"table"`
	Columns     []describeColumn     `json:"columns"`
	Indexes     []describeIndex      `json:"indexes"`
	Constraints []describeConstraint `json:"constraints"`
}

// describeTable builds the structured description of one table. The
// relation is resolved through the $1::regclass cast (schema-qualified
// when schemaName is given, search_path otherwise); a missing relation
// surfaces as SQLSTATE 42P01.
func describeTable(ctx context.Context, db *sql.DB, schemaName, table string) (*tableDescription, error) {
	ref := quoteIdent(table)
	if schemaName != "" {
		ref = quoteIdent(schemaName) + "." + ref
	}
	desc := &tableDescription{
		Columns:     []describeColumn{},
		Indexes:     []describeIndex{},
		Constraints: []describeConstraint{},
	}
	var relkind string
	err := db.QueryRowContext(ctx,
		`SELECT n.nspname, c.relname, c.relkind
		 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.oid = $1::regclass`, ref).Scan(&desc.Schema, &desc.Table, &relkind)
	if err != nil {
		return nil, err
	}

	attRef := quoteIdent(desc.Schema) + "." + quoteIdent(desc.Table)
	rs, err := db.QueryContext(ctx,
		`SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod),
		        NOT a.attnotnull, pg_get_expr(d.adbin, d.adrelid)
		 FROM pg_attribute a
		 LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
		 ORDER BY a.attnum`, attRef)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var col describeColumn
		var def sql.NullString
		if err := rs.Scan(&col.Name, &col.Type, &col.Nullable, &def); err != nil {
			_ = rs.Close()
			return nil, err
		}
		col.Default = nullStr(def)
		desc.Columns = append(desc.Columns, col)
	}
	_ = rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}

	rs, err = db.QueryContext(ctx,
		`SELECT indexname, indexdef FROM pg_indexes
		 WHERE schemaname = $1 AND tablename = $2 ORDER BY indexname`,
		desc.Schema, desc.Table)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var idx describeIndex
		if err := rs.Scan(&idx.Name, &idx.Definition); err != nil {
			_ = rs.Close()
			return nil, err
		}
		desc.Indexes = append(desc.Indexes, idx)
	}
	_ = rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}

	rs, err = db.QueryContext(ctx,
		`SELECT conname, contype, pg_get_constraintdef(oid)
		 FROM pg_constraint WHERE conrelid = $1::regclass ORDER BY conname`, attRef)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var con describeConstraint
		var contype string
		if err := rs.Scan(&con.Name, &contype, &con.Definition); err != nil {
			_ = rs.Close()
			return nil, err
		}
		con.Type = constraintTypeName(contype)
		desc.Constraints = append(desc.Constraints, con)
	}
	_ = rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return desc, nil
}

// constraintTypeName maps pg_constraint.contype letters to names.
func constraintTypeName(contype string) string {
	switch contype {
	case "p":
		return "primary key"
	case "u":
		return "unique"
	case "f":
		return "foreign key"
	case "c":
		return "check"
	case "x":
		return "exclude"
	case "n": // PG 18+ records NOT NULL as a constraint
		return "not null"
	default:
		return contype
	}
}

// renderTableDescription renders a single-table description: aligned text
// in text modes, the structured JSON shape in --json.
func renderTableDescription(cmd *cobra.Command, connName string, desc *tableDescription, start time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Table \"%s.%s\"\n", desc.Schema, desc.Table)
	nameW, typeW := len("column"), len("type")
	for _, c := range desc.Columns {
		if len(c.Name) > nameW {
			nameW = len(c.Name)
		}
		if len(c.Type) > typeW {
			typeW = len(c.Type)
		}
	}
	pad := func(s string, w int) string { return s + strings.Repeat(" ", w-len(s)) }
	fmt.Fprintf(&b, "  %s  %s  nullable  default\n", pad("column", nameW), pad("type", typeW))
	for _, c := range desc.Columns {
		def := ""
		if s, ok := c.Default.(string); ok {
			def = s
		}
		fmt.Fprintf(&b, "  %s  %s  %-8v  %s\n", pad(c.Name, nameW), pad(c.Type, typeW), c.Nullable, def)
	}
	if len(desc.Indexes) > 0 {
		b.WriteString("Indexes:\n")
		for _, i := range desc.Indexes {
			fmt.Fprintf(&b, "  %s: %s\n", i.Name, i.Definition)
		}
	}
	if len(desc.Constraints) > 0 {
		b.WriteString("Constraints:\n")
		for _, c := range desc.Constraints {
			fmt.Fprintf(&b, "  %s: %s\n", c.Name, c.Definition)
		}
	}
	return cli.RenderResult(cmd, &output.Result{
		Value:    strings.TrimRight(b.String(), "\n"),
		JSONData: desc,
	}, meta(connName, start, false))
}

// quoteIdent quotes a PostgreSQL identifier with double quotes.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// nullStr flattens a sql.NullString to a string or nil.
func nullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}
