package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// withDB runs fn with a verified database handle for an admin command.
// Admin commands report instance-level metadata, so there is no --db
// override. All of them except kill are read-only and pass on readonly
// connections.
func withDB(cmd *cobra.Command, fn func(ctx context.Context, db *sql.DB, name string, start time.Time) error) error {
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
	return fn(ctx, db, name, start)
}

// queryRowMap runs a single-row query and returns the row as a column →
// value map (nil for SQL NULL); ok is false when the result is empty.
func queryRowMap(ctx context.Context, db *sql.DB, query string, args ...any) (row map[string]any, ok bool, err error) {
	rows, _, err := queryRowsMap(ctx, db, query, args...)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	return rows[0], true, nil
}

// queryRowsMap runs a query and returns every row as a column → value map
// (nil for SQL NULL), plus the ordered column names. []byte values from
// the driver convert to strings (pgx stdlib hands text-format values over
// as []byte).
func queryRowsMap(ctx context.Context, db *sql.DB, query string, args ...any) ([]map[string]any, []string, error) {
	rs, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rs.Close() }()
	cols, err := rs.Columns()
	if err != nil {
		return nil, nil, err
	}
	rows := make([]map[string]any, 0)
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, isBytes := vals[i].([]byte); isBytes {
				row[c] = string(b)
			} else {
				row[c] = vals[i]
			}
		}
		rows = append(rows, row)
	}
	if err := rs.Err(); err != nil {
		return nil, nil, err
	}
	return rows, cols, nil
}

func newDatabasesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "databases",
		Short: "List databases on the server",
		Args:  cobra.NoArgs,
		Example: `  muxcat postgres databases
  muxcat postgres databases --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				rs, err := db.QueryContext(ctx,
					`SELECT d.datname, pg_get_userbyid(d.datdba),
					        pg_encoding_to_char(d.encoding), pg_database_size(d.datname)
					 FROM pg_database d WHERE NOT d.datistemplate ORDER BY d.datname`)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				dbs := make([]map[string]any, 0)
				for rs.Next() {
					var dbName, owner, encoding string
					var size int64
					if err := rs.Scan(&dbName, &owner, &encoding, &size); err != nil {
						return classifyErr(err, "failed to read results")
					}
					rows = append(rows, []any{dbName, owner, encoding, size})
					dbs = append(dbs, map[string]any{
						"name": dbName, "owner": owner, "encoding": encoding, "size": size,
					})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"name", "owner", "encoding", "size"},
					Rows:     rows,
					JSONData: map[string]any{"databases": dbs},
				}, meta(name, start, false))
			})
		},
	}
}

func newSchemasCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schemas",
		Short: "List user schemas (pg_* and information_schema excluded)",
		Args:  cobra.NoArgs,
		Example: `  muxcat postgres schemas
  muxcat postgres schemas --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				rs, err := db.QueryContext(ctx,
					`SELECT nspname, pg_get_userbyid(nspowner) FROM pg_namespace
					 WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\' AND nspname <> 'information_schema'
					 ORDER BY nspname`)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				schemas := make([]map[string]any, 0)
				for rs.Next() {
					var sch, owner string
					if err := rs.Scan(&sch, &owner); err != nil {
						return classifyErr(err, "failed to read results")
					}
					rows = append(rows, []any{sch, owner})
					schemas = append(schemas, map[string]any{"name": sch, "owner": owner})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"name", "owner"},
					Rows:     rows,
					JSONData: map[string]any{"schemas": schemas},
				}, meta(name, start, false))
			})
		},
	}
}

func newStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Show server status metrics (curated by default, --all dumps pg_stat_database)",
		Long: `Show server status. The default prints a curated, fixed-order
metric set (version, uptime, connection counts, transaction
commit/rollback totals, buffer cache hit rate, deadlocks) derived
from pg_stat_activity and pg_stat_database; --all dumps the full
pg_stat_database, one row per database.`,
		Args: cobra.NoArgs,
		Example: `  muxcat postgres status
  muxcat postgres status --all --limit 50
  muxcat postgres status --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				if cli.FlagBool(cmd, "all") {
					rows, cols, err := queryRowsMap(ctx, db,
						"SELECT * FROM pg_stat_database ORDER BY datname")
					if err != nil {
						return classifyErr(err, "query failed")
					}
					tableRows := make([][]any, 0, len(rows))
					for _, r := range rows {
						row := make([]any, len(cols))
						for i, cn := range cols {
							row[i] = r[cn]
						}
						tableRows = append(tableRows, row)
					}
					return cli.RenderResult(cmd, &output.Result{
						Columns:  cols,
						Rows:     tableRows,
						JSONData: map[string]any{"databases": rows},
					}, meta(name, start, false))
				}
				metrics, err := collectStatus(ctx, db)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				rows := make([][]any, 0, len(metrics))
				mj := make(map[string]any, len(metrics))
				for _, m := range metrics {
					rows = append(rows, []any{m.name, m.text})
					mj[m.name] = m.value
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"name", "value"},
					Rows:     rows,
					JSONData: map[string]any{"metrics": mj},
				}, meta(name, start, false))
			})
		},
	}
	c.Flags().Bool("all", false, "dump the full pg_stat_database instead of the curated metrics")
	return c
}

// metric is one curated status entry: text is the display form, value the
// JSON form (numeric where derivable).
type metric struct {
	name  string
	text  string
	value any
}

// collectStatus derives the fixed-order status metrics. All catalog
// references are stable across PG 12-18 (pg_stat_bgwriter, renamed in
// PG 17, is deliberately not used).
func collectStatus(ctx context.Context, db *sql.DB) ([]metric, error) {
	var version string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&version); err != nil {
		return nil, err
	}
	var uptime int64
	if err := db.QueryRowContext(ctx,
		"SELECT extract(epoch FROM now() - pg_postmaster_start_time())::bigint").Scan(&uptime); err != nil {
		return nil, err
	}
	var maxConns int64
	if err := db.QueryRowContext(ctx,
		"SELECT setting::bigint FROM pg_settings WHERE name = 'max_connections'").Scan(&maxConns); err != nil {
		return nil, err
	}
	var conns, active, idle int64
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), count(*) FILTER (WHERE state = 'active'), count(*) FILTER (WHERE state = 'idle')
		 FROM pg_stat_activity`).Scan(&conns, &active, &idle); err != nil {
		return nil, err
	}
	var commit, rollback, hit, read, deadlocks int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(xact_commit), 0)::bigint, COALESCE(sum(xact_rollback), 0)::bigint,
		        COALESCE(sum(blks_hit), 0)::bigint, COALESCE(sum(blks_read), 0)::bigint,
		        COALESCE(sum(deadlocks), 0)::bigint
		 FROM pg_stat_database`).Scan(&commit, &rollback, &hit, &read, &deadlocks); err != nil {
		return nil, err
	}

	var out []metric
	add := func(name, text string, value any) {
		out = append(out, metric{name: name, text: text, value: value})
	}
	addInt := func(name string, v int64) {
		add(name, strconv.FormatInt(v, 10), v)
	}
	add("version", version, version)
	addInt("uptime_s", uptime)
	addInt("max_connections", maxConns)
	addInt("connections", conns)
	addInt("active", active)
	addInt("idle", idle)
	addInt("xact_commit", commit)
	addInt("xact_rollback", rollback)
	// No blocks touched yet means no possible misses: 100%.
	rate := 100.0
	if total := hit + read; total > 0 {
		rate = float64(hit) / float64(total) * 100
	}
	rate = math.Round(rate*10) / 10
	add("cache_hit_rate", strconv.FormatFloat(rate, 'f', 1, 64), rate)
	addInt("deadlocks", deadlocks)
	return out, nil
}

func newSettingsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "settings [pattern]",
		Short: "Show server settings, optionally filtered by a case-insensitive substring",
		Long: `Show server settings (pg_settings). [pattern] is a client-side
case-insensitive substring filter on the setting name, not a LIKE
pattern.`,
		Args: cobra.MaximumNArgs(1),
		Example: `  muxcat postgres settings max_connections
  muxcat postgres settings wal
  muxcat postgres settings --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				rs, err := db.QueryContext(ctx,
					"SELECT name, setting, unit, source FROM pg_settings ORDER BY name")
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				pattern := ""
				if len(args) == 1 {
					pattern = strings.ToLower(args[0])
				}
				rows := make([][]any, 0)
				settings := make(map[string]string)
				for rs.Next() {
					var sName, value, source string
					var unit sql.NullString
					if err := rs.Scan(&sName, &value, &unit, &source); err != nil {
						return classifyErr(err, "failed to read results")
					}
					if pattern != "" && !strings.Contains(strings.ToLower(sName), pattern) {
						continue
					}
					rows = append(rows, []any{sName, value, nullStr(unit), source})
					settings[sName] = value
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"name", "value", "unit", "source"},
					Rows:     rows,
					JSONData: map[string]any{"settings": settings},
				}, meta(name, start, false))
			})
		},
	}
}

// activityRow is one row of the activity command (pg_stat_activity).
// Background workers have NULL usename/datname, hence every nullable.
type activityRow struct {
	PID       int64
	User      sql.NullString
	DB        sql.NullString
	Client    sql.NullString
	State     sql.NullString
	DurationS sql.NullInt64
	Wait      sql.NullString
	Query     sql.NullString
}

func queryActivity(ctx context.Context, db *sql.DB) ([]activityRow, error) {
	rs, err := db.QueryContext(ctx,
		`SELECT pid, usename, datname, client_addr::text, state,
		        extract(epoch FROM now() - query_start)::bigint,
		        wait_event_type || '.' || wait_event, query
		 FROM pg_stat_activity ORDER BY pid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := make([]activityRow, 0)
	for rs.Next() {
		var a activityRow
		if err := rs.Scan(&a.PID, &a.User, &a.DB, &a.Client, &a.State, &a.DurationS, &a.Wait, &a.Query); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func newActivityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "activity",
		Short: "Show live backend activity (pg_stat_activity)",
		Args:  cobra.NoArgs,
		Example: `  muxcat postgres activity
  muxcat postgres activity --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				as, err := queryActivity(ctx, db)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				rows := make([][]any, 0, len(as))
				sessions := make([]map[string]any, 0, len(as))
				for _, a := range as {
					queryText := nullStr(a.Query)
					display := queryText
					if s, ok := queryText.(string); ok {
						display = truncateDisplay(s, 80)
					}
					rows = append(rows, []any{
						a.PID, nullStr(a.User), nullStr(a.DB), nullStr(a.Client),
						nullStr(a.State), nullInt(a.DurationS), nullStr(a.Wait), display,
					})
					sessions = append(sessions, map[string]any{
						"pid": a.PID, "user": nullStr(a.User), "db": nullStr(a.DB),
						"client": nullStr(a.Client), "state": nullStr(a.State),
						"duration_s": nullInt(a.DurationS), "wait": nullStr(a.Wait),
						"query": queryText,
					})
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:   []string{"pid", "user", "db", "client", "state", "duration_s", "wait", "query"},
					Rows:      rows,
					JSONData:  map[string]any{"sessions": sessions},
					CellStyle: cellStyle,
				}, meta(name, start, false))
			})
		},
	}
}

// nullInt flattens a sql.NullInt64 to an int64 or nil.
func nullInt(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func newKillCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "kill <pid> [--cancel] [--yes]",
		Short: "Terminate (or with --cancel, interrupt) a backend by pid",
		Long: `Terminate a backend by its pid (see activity): the default runs
pg_terminate_backend (closes the connection); --cancel runs
pg_cancel_backend instead (interrupts the running query, keeps the
connection). On a TTY the target session is shown before a
confirmation prompt; off a TTY --yes is required. Not allowed on a
readonly connection.`,
		Args: cli.ExactArgs(1, "<pid>", "pid"),
		Example: `  muxcat postgres kill 1234 --yes
  muxcat postgres kill 1234 --cancel --yes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			pid, err := parseKillPID(args[0])
			if err != nil {
				return err
			}
			if conn.Readonly {
				return output.NewError(output.CodeReadonlyViolation,
					"kill is not allowed on a readonly connection",
					"use a writable connection (-c)")
			}
			yes := cli.FlagBool(cmd, "yes")
			if !yes && !cli.RuntimeFrom(cmd.Context()).Interactive {
				return output.NewError(output.CodeMissingArgument,
					"killing a backend requires confirmation", "pass --yes in non-interactive environments")
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

			if !yes {
				// On a TTY, show what is about to be killed before asking.
				as, err := queryActivity(ctx, db)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				var target *activityRow
				for i := range as {
					if as[i].PID == pid {
						target = &as[i]
						break
					}
				}
				if target == nil {
					return output.NewError(output.CodeQueryError,
						fmt.Sprintf("no such backend pid: %d", pid),
						"list backends with muxcat postgres activity")
				}
				summary := nullStr(target.State)
				if target.Query.Valid && target.Query.String != "" {
					summary = truncateDisplay(target.Query.String, 60)
				}
				action := "Terminate"
				if cli.FlagBool(cmd, "cancel") {
					action = "Cancel the query of"
				}
				confirm := false
				form := huh.NewForm(huh.NewGroup(
					huh.NewConfirm().Title(fmt.Sprintf("%s backend %d (%v, %v)?", action, pid, nullStr(target.User), summary)).Value(&confirm),
				))
				if err := form.Run(); err != nil {
					return err
				}
				if !confirm {
					return output.NewError(output.CodeGeneral, "cancelled", "")
				}
			}

			fn := "pg_terminate_backend"
			verb := "terminated"
			if cli.FlagBool(cmd, "cancel") {
				fn = "pg_cancel_backend"
				verb = "cancelled"
			}
			// pid is a validated integer; direct interpolation is safe.
			var ok bool
			if err := db.QueryRowContext(ctx,
				"SELECT "+fn+"("+strconv.FormatInt(pid, 10)+")").Scan(&ok); err != nil {
				return classifyErr(err, "kill failed")
			}
			if !ok {
				return output.NewError(output.CodeQueryError,
					fmt.Sprintf("no such backend pid: %d", pid),
					"list backends with muxcat postgres activity")
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("%s backend %d", verb, pid),
			}, meta(name, start, false))
		},
	}
	c.Flags().Bool("cancel", false, "cancel the backend's running query instead of terminating the connection")
	c.Flags().Bool("yes", false, "skip the confirmation prompt (required off a TTY)")
	return c
}

// parseKillPID validates a backend pid: digits only, greater than zero.
func parseKillPID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != s {
		return 0, output.NewError(output.CodeMissingArgument,
			"invalid backend pid: "+s, "usage: muxcat postgres kill <pid> [--cancel] [--yes]")
	}
	return id, nil
}

func newRolesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "roles",
		Short: "List roles (pg_roles)",
		Args:  cobra.NoArgs,
		Example: `  muxcat postgres roles
  muxcat postgres roles --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				rs, err := db.QueryContext(ctx,
					`SELECT rolname, rolsuper, rolcreatedb, rolcreaterole, rolcanlogin, rolreplication, rolconnlimit
					 FROM pg_roles ORDER BY rolname`)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				roles := make([]map[string]any, 0)
				for rs.Next() {
					var role string
					var super, createdb, createrole, login, replication bool
					var connLimit int64
					if err := rs.Scan(&role, &super, &createdb, &createrole, &login, &replication, &connLimit); err != nil {
						return classifyErr(err, "failed to read results")
					}
					rows = append(rows, []any{role, super, createdb, createrole, login, replication, connLimit})
					roles = append(roles, map[string]any{
						"role": role, "superuser": super, "createdb": createdb,
						"createrole": createrole, "login": login,
						"replication": replication, "conn_limit": connLimit,
					})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"role", "superuser", "createdb", "createrole", "login", "replication", "conn_limit"},
					Rows:     rows,
					JSONData: map[string]any{"roles": roles},
				}, meta(name, start, false))
			})
		},
	}
}

func newGrantsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grants [role]",
		Short: "Show table privileges of a role (default: the current user)",
		Args:  cobra.MaximumNArgs(1),
		Example: `  muxcat postgres grants             # current user
  muxcat postgres grants app --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				role := ""
				if len(args) == 1 {
					role = args[0]
				} else if err := db.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil {
					return classifyErr(err, "query failed")
				}
				rs, err := db.QueryContext(ctx,
					`SELECT table_schema, table_name, privilege_type, is_grantable
					 FROM information_schema.role_table_grants
					 WHERE grantee = $1
					 ORDER BY table_schema, table_name, privilege_type`, role)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				grants := make([]map[string]any, 0)
				for rs.Next() {
					var sch, tbl, privilege, grantable string
					if err := rs.Scan(&sch, &tbl, &privilege, &grantable); err != nil {
						return classifyErr(err, "failed to read results")
					}
					rows = append(rows, []any{sch, tbl, privilege, grantable})
					grants = append(grants, map[string]any{
						"schema": sch, "table": tbl,
						"privilege": privilege, "grantable": grantable == "YES",
					})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"schema", "table", "privilege", "grantable"},
					Rows:     rows,
					JSONData: map[string]any{"role": role, "grants": grants},
				}, meta(name, start, false))
			})
		},
	}
}

func newExtensionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "extensions",
		Short: "List installed extensions of the current database",
		Args:  cobra.NoArgs,
		Example: `  muxcat postgres extensions
  muxcat postgres extensions --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				rs, err := db.QueryContext(ctx,
					`SELECT e.extname, e.extversion, n.nspname
					 FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
					 ORDER BY e.extname`)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				exts := make([]map[string]any, 0)
				for rs.Next() {
					var ext, version, sch string
					if err := rs.Scan(&ext, &version, &sch); err != nil {
						return classifyErr(err, "failed to read results")
					}
					rows = append(rows, []any{ext, version, sch})
					exts = append(exts, map[string]any{"name": ext, "version": version, "schema": sch})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"name", "version", "schema"},
					Rows:     rows,
					JSONData: map[string]any{"extensions": exts},
				}, meta(name, start, false))
			})
		},
	}
}

func newLocksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "locks",
		Short: "Show current locks and their blockers (pg_locks + pg_stat_activity)",
		Args:  cobra.NoArgs,
		Example: `  muxcat postgres locks
  muxcat postgres locks --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				rs, err := db.QueryContext(ctx,
					`SELECT l.pid, l.locktype, COALESCE(c.relname, l.relation::text), l.mode, l.granted,
					        pg_blocking_pids(l.pid)::text, a.usename, a.query
					 FROM pg_locks l
					 LEFT JOIN pg_stat_activity a ON a.pid = l.pid
					 LEFT JOIN pg_class c ON c.oid = l.relation
					 ORDER BY l.granted, l.pid, l.locktype`)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				locks := make([]map[string]any, 0)
				for rs.Next() {
					var pid sql.NullInt64
					var locktype, mode string
					var relation, blocking sql.NullString
					var granted bool
					var user, queryText sql.NullString
					if err := rs.Scan(&pid, &locktype, &relation, &mode, &granted, &blocking, &user, &queryText); err != nil {
						return classifyErr(err, "failed to read results")
					}
					display := nullStr(queryText)
					if s, ok := display.(string); ok {
						display = truncateDisplay(s, 60)
					}
					rows = append(rows, []any{
						nullInt(pid), locktype, nullStr(relation), mode, granted,
						nullStr(blocking), nullStr(user), display,
					})
					locks = append(locks, map[string]any{
						"pid": nullInt(pid), "locktype": locktype, "relation": nullStr(relation),
						"mode": mode, "granted": granted,
						"blocking_pids": nullStr(blocking),
						"user":          nullStr(user), "query": nullStr(queryText),
					})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:   []string{"pid", "locktype", "relation", "mode", "granted", "blocking_pids", "user", "query"},
					Rows:      rows,
					JSONData:  map[string]any{"locks": locks},
					CellStyle: cellStyle,
				}, meta(name, start, false))
			})
		},
	}
}

func newReplicationCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "replication",
		Short: "Show replication status (pg_stat_replication on a primary, pg_stat_wal_receiver on a standby)",
		Long: `Show replication status. On a primary (pg_is_in_recovery() =
false) it lists pg_stat_replication rows, one per replica; with no
replicas it reports "no replicas". On a standby it shows the
selected pg_stat_wal_receiver fields. The wal_receiver conninfo
column (which may carry credentials) is never selected.`,
		Args: cobra.NoArgs,
		Example: `  muxcat postgres replication
  muxcat postgres replication --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				var inRecovery bool
				if err := db.QueryRowContext(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
					return classifyErr(err, "query failed")
				}
				if inRecovery {
					return renderWalReceiver(cmd, ctx, db, name, start)
				}
				rs, err := db.QueryContext(ctx,
					`SELECT pid, usename, client_addr::text, state,
					        sent_lsn::text, write_lsn::text, flush_lsn::text, replay_lsn::text, sync_state
					 FROM pg_stat_replication ORDER BY pid`)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				replicas := make([]map[string]any, 0)
				for rs.Next() {
					var pid int64
					var state, syncState string
					var user, client, sent, write, flush, replay sql.NullString
					if err := rs.Scan(&pid, &user, &client, &state, &sent, &write, &flush, &replay, &syncState); err != nil {
						return classifyErr(err, "failed to read results")
					}
					rows = append(rows, []any{
						pid, nullStr(user), nullStr(client), state,
						nullStr(sent), nullStr(write), nullStr(flush), nullStr(replay), syncState,
					})
					replicas = append(replicas, map[string]any{
						"pid": pid, "user": nullStr(user), "client_addr": nullStr(client), "state": state,
						"sent_lsn": nullStr(sent), "write_lsn": nullStr(write),
						"flush_lsn": nullStr(flush), "replay_lsn": nullStr(replay),
						"sync_state": syncState,
					})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				if len(replicas) == 0 {
					return cli.RenderResult(cmd, &output.Result{
						Message:  "no replicas",
						JSONData: map[string]any{"replicas": []any{}},
					}, meta(name, start, false))
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:   []string{"pid", "user", "client_addr", "state", "sent_lsn", "write_lsn", "flush_lsn", "replay_lsn", "sync_state"},
					Rows:      rows,
					JSONData:  map[string]any{"replicas": replicas},
					CellStyle: cellStyle,
				}, meta(name, start, false))
			})
		},
	}
}

// renderWalReceiver renders the standby side of replication. PG 13+
// renamed the old received_lsn column; written_lsn (the LSN received and
// written to disk) takes its place under the received_lsn output name.
func renderWalReceiver(cmd *cobra.Command, ctx context.Context, db *sql.DB, name string, start time.Time) error {
	row, ok, err := queryRowMap(ctx, db,
		`SELECT status, sender_host, sender_port, written_lsn::text AS received_lsn,
		        latest_end_lsn::text, last_msg_send_time, last_msg_receipt_time
		 FROM pg_stat_wal_receiver`)
	if err != nil {
		return classifyErr(err, "query failed")
	}
	if !ok {
		return cli.RenderResult(cmd, &output.Result{
			Message:  "no wal receiver",
			JSONData: map[string]any{"receiver": nil},
		}, meta(name, start, false))
	}
	display := make(map[string]any, len(row))
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := row[k]
		if v == nil {
			display[k] = "NULL"
		} else {
			display[k] = v
		}
	}
	return cli.RenderResult(cmd, &output.Result{
		Value:    display,
		JSONData: map[string]any{"receiver": row},
	}, meta(name, start, false))
}

// truncateDisplay shortens a long single-line value (e.g. a query text)
// for display, keeping the head. Truncation is by runes so multi-byte
// UTF-8 text is never cut mid-character.
func truncateDisplay(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}
