package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	gomysql "github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// withDB runs fn with a verified database handle for an admin command.
// Admin commands report instance-level metadata, so there is no --db
// override.
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

// classifyAdminErr classifies an admin query error, adding a privilege
// hint for access-denied errors (1142 ER_TABLEACCESS_DENIED /
// 1227 ER_SPECIFIC_ACCESS_DENIED).
func classifyAdminErr(err error, prefix, privilegeHint string) *output.Error {
	var myErr *gomysql.MySQLError
	if errors.As(err, &myErr) && (myErr.Number == 1142 || myErr.Number == 1227) {
		msg := err.Error()
		if prefix != "" {
			msg = prefix + ": " + msg
		}
		return output.NewError(output.CodeQueryError, msg, privilegeHint)
	}
	return classifyErr(err, prefix)
}

// queryStrings returns the single text column of a query as a list.
func queryStrings(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rs, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := make([]string, 0)
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// showPairs runs a two-column SHOW (Variable_name, Value) and returns the
// rows as a name → value map.
func showPairs(ctx context.Context, db *sql.DB, query string) (map[string]string, error) {
	rs, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	m := make(map[string]string)
	for rs.Next() {
		var name string
		var value sql.NullString
		if err := rs.Scan(&name, &value); err != nil {
			return nil, err
		}
		m[name] = value.String
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// queryRowMap runs a single-row query and returns the row as a column →
// value map (nil for SQL NULL); ok is false when the result is empty.
func queryRowMap(ctx context.Context, db *sql.DB, query string) (row map[string]any, ok bool, err error) {
	rs, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rs.Close() }()
	cols, err := rs.Columns()
	if err != nil {
		return nil, false, err
	}
	if !rs.Next() {
		if err := rs.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rs.Scan(ptrs...); err != nil {
		return nil, false, err
	}
	row = make(map[string]any, len(cols))
	for i, c := range cols {
		if b, isBytes := vals[i].([]byte); isBytes {
			row[c] = string(b)
		} else {
			row[c] = vals[i]
		}
	}
	return row, true, nil
}

// nullStr flattens a sql.NullString to a string or nil.
func nullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

func newDatabasesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "databases",
		Short: "List databases on the server",
		Args:  cobra.NoArgs,
		Example: `  muxcat mysql databases
  muxcat mysql databases --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				names, err := queryStrings(ctx, db, "SHOW DATABASES")
				if err != nil {
					return classifyErr(err, "query failed")
				}
				sort.Strings(names)
				rows := make([][]any, 0, len(names))
				for _, n := range names {
					rows = append(rows, []any{n})
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"database"},
					Rows:     rows,
					JSONData: map[string]any{"databases": names},
				}, meta(name, start, false))
			})
		},
	}
}

func newStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Show server status metrics (curated by default, --all dumps everything)",
		Long: `Show server status. The default prints a curated, fixed-order metric
set (version, uptime, qps, threads, connections, slow queries, InnoDB
buffer pool hit rate, ...) derived from SHOW GLOBAL STATUS plus a few
variables; --all dumps the full SHOW GLOBAL STATUS, sorted by name.`,
		Args: cobra.NoArgs,
		Example: `  muxcat mysql status
  muxcat mysql status --all --limit 50
  muxcat mysql status --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				status, err := showPairs(ctx, db, "SHOW GLOBAL STATUS")
				if err != nil {
					return classifyErr(err, "query failed")
				}
				if cli.FlagBool(cmd, "all") {
					names := filterNames(status, "")
					rows := make([][]any, 0, len(names))
					for _, n := range names {
						rows = append(rows, []any{n, status[n]})
					}
					return cli.RenderResult(cmd, &output.Result{
						Columns:  []string{"name", "value"},
						Rows:     rows,
						JSONData: map[string]any{"status": status},
					}, meta(name, start, false))
				}
				vars, err := showPairs(ctx, db, "SHOW GLOBAL VARIABLES")
				if err != nil {
					return classifyErr(err, "query failed")
				}
				metrics := curatedMetrics(status, vars)
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
	c.Flags().Bool("all", false, "dump the full SHOW GLOBAL STATUS instead of the curated metrics")
	return c
}

// metric is one curated status entry: text is the display form, value the
// JSON form (numeric where derivable).
type metric struct {
	name  string
	text  string
	value any
}

// curatedMetrics derives the fixed-order status metrics from the raw
// SHOW GLOBAL STATUS / SHOW GLOBAL VARIABLES maps.
func curatedMetrics(status, vars map[string]string) []metric {
	var out []metric
	add := func(name, text string, value any) {
		out = append(out, metric{name: name, text: text, value: value})
	}
	addRaw := func(name, raw string) {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			add(name, raw, n)
		} else {
			add(name, raw, raw)
		}
	}
	addStatus := func(name, key string) {
		if raw, ok := status[key]; ok {
			addRaw(name, raw)
		}
	}
	if v, ok := vars["version"]; ok {
		add("version", v, v)
	}
	addStatus("uptime_s", "Uptime")
	if queries, err := strconv.ParseFloat(status["Queries"], 64); err == nil {
		if uptime, err := strconv.ParseFloat(status["Uptime"], 64); err == nil && uptime > 0 {
			qps := math.Round(queries/uptime*10) / 10
			add("qps", strconv.FormatFloat(qps, 'f', 1, 64), qps)
		}
	}
	addStatus("threads_connected", "Threads_connected")
	addStatus("threads_running", "Threads_running")
	if v, ok := vars["max_connections"]; ok {
		addRaw("max_connections", v)
	}
	addStatus("connections", "Connections")
	addStatus("aborted_connects", "Aborted_connects")
	addStatus("questions", "Questions")
	addStatus("slow_queries", "Slow_queries")
	addStatus("com_select", "Com_select")
	addStatus("com_insert", "Com_insert")
	addStatus("com_update", "Com_update")
	addStatus("com_delete", "Com_delete")
	reads, hasReads := status["Innodb_buffer_pool_reads"]
	requests, hasRequests := status["Innodb_buffer_pool_read_requests"]
	if hasReads && hasRequests {
		addStatus("innodb_buffer_pool_reads", "Innodb_buffer_pool_reads")
		addStatus("innodb_buffer_pool_read_requests", "Innodb_buffer_pool_read_requests")
		r, _ := strconv.ParseFloat(reads, 64)
		q, _ := strconv.ParseFloat(requests, 64)
		// No read requests yet means no possible misses: 100%.
		rate := 100.0
		if q > 0 {
			rate = (1 - r/q) * 100
		}
		rate = math.Round(rate*10) / 10
		add("innodb_buffer_pool_hit_rate", strconv.FormatFloat(rate, 'f', 1, 64), rate)
	}
	return out
}

// filterNames returns the sorted subset of map keys containing pattern
// (case-insensitive substring; "" matches everything).
func filterNames(m map[string]string, pattern string) []string {
	p := strings.ToLower(pattern)
	names := make([]string, 0, len(m))
	for name := range m {
		if p == "" || strings.Contains(strings.ToLower(name), p) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func newVariablesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "variables [pattern]",
		Short: "Show server variables, optionally filtered by a case-insensitive substring",
		Long: `Show server variables (SHOW GLOBAL VARIABLES; --session switches to
session variables). [pattern] is a client-side case-insensitive
substring filter, not a LIKE pattern.`,
		Args: cobra.MaximumNArgs(1),
		Example: `  muxcat mysql variables max_connections
  muxcat mysql variables innodb_buffer --session
  muxcat mysql variables --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				query := "SHOW GLOBAL VARIABLES"
				if cli.FlagBool(cmd, "session") {
					query = "SHOW SESSION VARIABLES"
				}
				vars, err := showPairs(ctx, db, query)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				pattern := ""
				if len(args) == 1 {
					pattern = args[0]
				}
				names := filterNames(vars, pattern)
				rows := make([][]any, 0, len(names))
				filtered := make(map[string]string, len(names))
				for _, n := range names {
					rows = append(rows, []any{n, vars[n]})
					filtered[n] = vars[n]
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"name", "value"},
					Rows:     rows,
					JSONData: map[string]any{"variables": filtered},
				}, meta(name, start, false))
			})
		},
	}
	c.Flags().Bool("session", false, "show session variables instead of global")
	return c
}

// process is one row of SHOW FULL PROCESSLIST.
type process struct {
	ID      int64
	User    string
	Host    string
	DB      sql.NullString
	Command string
	Time    int64
	State   sql.NullString
	Info    sql.NullString
}

func queryProcesses(ctx context.Context, db *sql.DB) ([]process, error) {
	rs, err := db.QueryContext(ctx, "SHOW FULL PROCESSLIST")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := make([]process, 0)
	for rs.Next() {
		var p process
		if err := rs.Scan(&p.ID, &p.User, &p.Host, &p.DB, &p.Command, &p.Time, &p.State, &p.Info); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func newProcesslistCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "processlist",
		Short: "Show the full process list",
		Args:  cobra.NoArgs,
		Example: `  muxcat mysql processlist
  muxcat mysql processlist --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				ps, err := queryProcesses(ctx, db)
				if err != nil {
					return classifyAdminErr(err, "query failed", "seeing other accounts' processes requires the PROCESS privilege")
				}
				rows := make([][]any, 0, len(ps))
				procs := make([]map[string]any, 0, len(ps))
				for _, p := range ps {
					rows = append(rows, []any{p.ID, p.User, p.Host, nullStr(p.DB), p.Command, p.Time, nullStr(p.State), nullStr(p.Info)})
					procs = append(procs, map[string]any{
						"id": p.ID, "user": p.User, "host": p.Host,
						"db": nullStr(p.DB), "command": p.Command, "time": p.Time,
						"state": nullStr(p.State), "info": nullStr(p.Info),
					})
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:   []string{"id", "user", "host", "db", "command", "time", "state", "info"},
					Rows:      rows,
					JSONData:  map[string]any{"processes": procs},
					CellStyle: cellStyle,
				}, meta(name, start, false))
			})
		},
	}
}

func newKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <id>",
		Short: "Kill a connection from the process list",
		Long: `Kill a connection by its process id (see processlist). On a TTY the
target is shown before a confirmation prompt; off a TTY --yes is
required. Not allowed on a readonly connection. Killing another
account's connection requires the CONNECTION_ADMIN privilege.`,
		Args:    cli.ExactArgs(1, "<id>", "id"),
		Example: `  muxcat mysql kill 1234 --yes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			id, err := parseKillID(args[0])
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
					"killing a connection requires confirmation", "pass --yes in non-interactive environments")
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
				ps, err := queryProcesses(ctx, db)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				var target *process
				for i := range ps {
					if ps[i].ID == id {
						target = &ps[i]
						break
					}
				}
				if target == nil {
					return output.NewError(output.CodeQueryError,
						fmt.Sprintf("no such process id: %d", id),
						"list processes with muxcat mysql processlist")
				}
				summary := target.Command
				if target.Info.Valid && target.Info.String != "" {
					summary = target.Info.String
					if len(summary) > 60 {
						summary = summary[:57] + "..."
					}
				}
				confirm := false
				form := huh.NewForm(huh.NewGroup(
					huh.NewConfirm().Title(fmt.Sprintf("Kill connection %d (%s@%s, %s)?", id, target.User, target.Host, summary)).Value(&confirm),
				))
				if err := form.Run(); err != nil {
					return err
				}
				if !confirm {
					return output.NewError(output.CodeGeneral, "cancelled", "")
				}
			}

			// id is a validated integer; direct interpolation is safe.
			if _, err := db.ExecContext(ctx, "KILL "+strconv.FormatInt(id, 10)); err != nil {
				return classifyAdminErr(err, "kill failed", "killing other accounts' connections requires the CONNECTION_ADMIN privilege")
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("killed connection %d", id),
			}, meta(name, start, false))
		},
	}
}

// parseKillID validates a process id: digits only, greater than zero.
func parseKillID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != s {
		return 0, output.NewError(output.CodeMissingArgument,
			"invalid process id: "+s, "usage: muxcat mysql kill <id> [--yes]")
	}
	return id, nil
}

func newUsersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "users",
		Short: "List server accounts from mysql.user",
		Args:  cobra.NoArgs,
		Example: `  muxcat mysql users
  muxcat mysql users --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				rs, err := db.QueryContext(ctx,
					"SELECT user, host, plugin, account_locked, password_expired FROM mysql.user ORDER BY user, host")
				if err != nil {
					return classifyAdminErr(err, "query failed", "requires the SELECT privilege on mysql.user")
				}
				defer func() { _ = rs.Close() }()
				rows := make([][]any, 0)
				for rs.Next() {
					var user, host, plugin, locked, expired string
					if err := rs.Scan(&user, &host, &plugin, &locked, &expired); err != nil {
						return classifyErr(err, "failed to read results")
					}
					rows = append(rows, []any{user, host, plugin, locked, expired})
				}
				if err := rs.Err(); err != nil {
					return classifyErr(err, "failed to read results")
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns: []string{"user", "host", "plugin", "locked", "expired"},
					Rows:    rows,
				}, meta(name, start, false))
			})
		},
	}
}

func newGrantsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grants [user@host]",
		Short: "Show grants for the current account or a given user@host",
		Args:  cobra.MaximumNArgs(1),
		Example: `  muxcat mysql grants                    # current account
  muxcat mysql grants 'app'@'%'
  muxcat mysql grants root@localhost --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := "SHOW GRANTS"
			if len(args) == 1 {
				q, err := buildGrantQuery(args[0])
				if err != nil {
					return err
				}
				query = q
			}
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				grants, err := queryStrings(ctx, db, query)
				if err != nil {
					return classifyAdminErr(err, "query failed",
						"inspecting other accounts' grants requires privileges on the mysql system database")
				}
				rows := make([][]any, 0, len(grants))
				for _, g := range grants {
					rows = append(rows, []any{g})
				}
				return cli.RenderResult(cmd, &output.Result{
					Columns:  []string{"grants"},
					Rows:     rows,
					JSONData: map[string]any{"grants": grants},
				}, meta(name, start, false))
			})
		},
	}
}

// buildGrantQuery builds SHOW GRANTS FOR 'user'@'host', splitting on the
// first '@' and escaping quotes and backslashes in both parts.
func buildGrantQuery(arg string) (string, error) {
	i := strings.Index(arg, "@")
	if i < 0 {
		return "", output.NewError(output.CodeMissingArgument,
			"invalid grant target: "+arg, "usage: muxcat mysql grants [user@host]")
	}
	escape := func(s string) string {
		return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
	}
	return fmt.Sprintf("SHOW GRANTS FOR '%s'@'%s'", escape(arg[:i]), escape(arg[i+1:])), nil
}

func newEngineCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "engine",
		Short: "Storage engine inspection",
		Long: `Storage engine inspection. Currently covers InnoDB only: engine
innodb status prints the InnoDB monitor report.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newEngineInnodbCmd())
	return c
}

func newEngineInnodbCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "innodb",
		Short: "InnoDB inspection",
		Long: `InnoDB inspection: the engine status monitor report (SHOW ENGINE
INNODB STATUS) with its transaction, deadlock, and buffer pool
sections.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newEngineInnodbStatusCmd())
	return c
}

func newEngineInnodbStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print the InnoDB monitor report (SHOW ENGINE INNODB STATUS)",
		Args:  cobra.NoArgs,
		Example: `  muxcat mysql engine innodb status
  muxcat mysql engine innodb status --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				var typ, engineName, status string
				err := db.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&typ, &engineName, &status)
				if err != nil {
					return classifyErr(err, "query failed")
				}
				return cli.RenderResult(cmd, &output.Result{
					Value:    status,
					JSONData: map[string]any{"status": status},
				}, meta(name, start, false))
			})
		},
	}
}

func newReplicationCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "replication",
		Short: "Show replica status (curated fields, 8.0 naming)",
		Long: `Show replica status as curated fields with MySQL 8.0 naming (on 5.7
the Master_*/Slave_* columns are mapped automatically). A server that
is not replicating reports "not a replica". GTID sets are truncated
in text output; JSON keeps the full values.`,
		Args: cobra.NoArgs,
		Example: `  muxcat mysql replication
  muxcat mysql replication --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd, func(ctx context.Context, db *sql.DB, name string, start time.Time) error {
				row, ok, err := queryRowMap(ctx, db, "SHOW REPLICA STATUS")
				if err != nil {
					// MySQL 5.7 / older MariaDB only know SHOW SLAVE STATUS
					// (same style as the enforceReadonly fallback).
					var myErr *gomysql.MySQLError
					if errors.As(err, &myErr) && myErr.Number == 1064 {
						row, ok, err = queryRowMap(ctx, db, "SHOW SLAVE STATUS")
					}
				}
				if err != nil {
					return classifyErr(err, "query failed")
				}
				if !ok {
					return cli.RenderResult(cmd, &output.Result{
						Message:  "not a replica",
						JSONData: map[string]any{"replica": nil},
					}, meta(name, start, false))
				}
				curated := curateReplica(row)
				display := make(map[string]any, len(curated))
				for _, f := range replicaFields {
					v := curated[f.out]
					if v == nil {
						display[f.out] = "NULL"
					} else if s, isStr := v.(string); isStr && (f.out == "retrieved_gtid_set" || f.out == "executed_gtid_set") {
						display[f.out] = truncateDisplay(s, 80)
					} else {
						display[f.out] = v
					}
				}
				return cli.RenderResult(cmd, &output.Result{
					Value:    display,
					JSONData: curated,
				}, meta(name, start, false))
			})
		},
	}
}

// replicaFields pairs each curated output field (MySQL 8.0 naming) with
// the column names MySQL 8.0 and 5.7 use in SHOW REPLICA/SLAVE STATUS.
var replicaFields = []struct {
	out    string
	modern string
	legacy string
}{
	{"source_host", "Source_Host", "Master_Host"},
	{"source_port", "Source_Port", "Master_Port"},
	{"source_user", "Source_User", "Master_User"},
	{"replica_io_running", "Replica_IO_Running", "Slave_IO_Running"},
	{"replica_sql_running", "Replica_SQL_Running", "Slave_SQL_Running"},
	{"seconds_behind_source", "Seconds_Behind_Source", "Seconds_Behind_Master"},
	{"retrieved_gtid_set", "Retrieved_Gtid_Set", "Retrieved_Gtid_Set"},
	{"executed_gtid_set", "Executed_Gtid_Set", "Executed_Gtid_Set"},
	{"last_error", "Last_Error", "Last_Error"},
}

// curateReplica maps a SHOW REPLICA/SLAVE STATUS row to the curated
// fields, normalizing 5.7 Master_*/Slave_* column names to the 8.0 names.
// NULL values are preserved as nil.
func curateReplica(row map[string]any) map[string]any {
	out := make(map[string]any, len(replicaFields))
	for _, f := range replicaFields {
		v, ok := row[f.modern]
		if !ok {
			v = row[f.legacy]
		}
		out[f.out] = v
	}
	return out
}

// truncateDisplay shortens a long single-line value (e.g. a GTID set) for
// display, keeping the head.
func truncateDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
