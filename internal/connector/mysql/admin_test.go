package mysql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ravenmk2/muxcat/internal/output"
)

// TestExecuteReadonlyRejectsEverything verifies execute is stricter than
// query on readonly connections: even SELECT is rejected before dialing.
func TestExecuteReadonlyRejectsEverything(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--port", "1", "--timeout", "2s", "--readonly", "--set-default")
	for _, sqlText := range []string{"SELECT 1", "SHOW TABLES", "DROP TABLE t"} {
		_, err := runMuxcat(t, "mysql", "execute", sqlText)
		if e := output.ToError(err); err == nil || e.Code != output.CodeReadonlyViolation {
			t.Fatalf("execute %q on readonly conn: err=%v, want READONLY_VIOLATION", sqlText, err)
		}
	}
}

// TestExecuteUnconditionalExec verifies a writable connection reaches the
// dial stage even for SELECT (execute never routes to the Query path), and
// the input-channel mutual exclusion matches query.
func TestExecuteUnconditionalExec(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")

	_, err := runMuxcat(t, "mysql", "execute", "SELECT 1")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("execute SELECT on writable conn: err=%v, want CONNECT_FAILED", err)
	}

	_, err = runMuxcat(t, "mysql", "execute", "SELECT 1", "--file", "x")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("execute arg + --file: err=%v, want MISSING_ARGUMENT", err)
	}
}

func TestFilterNames(t *testing.T) {
	m := map[string]string{
		"Max_connections":    "151",
		"max_allowed_packet": "64M",
		"version":            "8.0.0",
	}
	if got := filterNames(m, "MAX"); !reflect.DeepEqual(got, []string{"Max_connections", "max_allowed_packet"}) {
		t.Fatalf("filterNames MAX = %v", got)
	}
	if got := filterNames(m, ""); !reflect.DeepEqual(got, []string{"Max_connections", "max_allowed_packet", "version"}) {
		t.Fatalf("filterNames empty = %v", got)
	}
	if got := filterNames(m, "nope"); len(got) != 0 {
		t.Fatalf("filterNames nope = %v", got)
	}
}

func TestCuratedMetrics(t *testing.T) {
	status := map[string]string{
		"Uptime": "100", "Queries": "1234",
		"Threads_connected": "5", "Threads_running": "1",
		"Connections": "500", "Aborted_connects": "2", "Questions": "1200",
		"Slow_queries": "3",
		"Com_select":   "800", "Com_insert": "100", "Com_update": "90", "Com_delete": "10",
		"Innodb_buffer_pool_reads": "10", "Innodb_buffer_pool_read_requests": "1000",
	}
	vars := map[string]string{"version": "8.0.36", "max_connections": "151"}
	ms := curatedMetrics(status, vars)

	wantOrder := []string{
		"version", "uptime_s", "qps", "threads_connected", "threads_running",
		"max_connections", "connections", "aborted_connects", "questions",
		"slow_queries", "com_select", "com_insert", "com_update", "com_delete",
		"innodb_buffer_pool_reads", "innodb_buffer_pool_read_requests",
		"innodb_buffer_pool_hit_rate",
	}
	order := make([]string, len(ms))
	byName := make(map[string]metric, len(ms))
	for i, m := range ms {
		order[i] = m.name
		byName[m.name] = m
	}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("metric order = %v", order)
	}
	if byName["qps"].text != "12.3" || byName["qps"].value != 12.3 {
		t.Fatalf("qps = %+v", byName["qps"])
	}
	if byName["innodb_buffer_pool_hit_rate"].text != "99.0" || byName["innodb_buffer_pool_hit_rate"].value != 99.0 {
		t.Fatalf("hit rate = %+v", byName["innodb_buffer_pool_hit_rate"])
	}
	if byName["uptime_s"].value != int64(100) {
		t.Fatalf("uptime_s = %+v", byName["uptime_s"])
	}
	if byName["max_connections"].value != int64(151) {
		t.Fatalf("max_connections = %+v", byName["max_connections"])
	}
	if byName["version"].value != "8.0.36" {
		t.Fatalf("version = %+v", byName["version"])
	}

	// Without InnoDB counters the InnoDB metrics are omitted.
	ms = curatedMetrics(map[string]string{"Uptime": "10", "Queries": "0"}, map[string]string{})
	for _, m := range ms {
		if strings.HasPrefix(m.name, "innodb") {
			t.Fatalf("InnoDB metric %q should be omitted", m.name)
		}
	}

	// Without Uptime there is no qps.
	ms = curatedMetrics(map[string]string{"Queries": "5"}, map[string]string{})
	for _, m := range ms {
		if m.name == "qps" {
			t.Fatal("qps should be omitted without Uptime")
		}
	}
}

func TestParseKillID(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-1", "1x", "01", ""} {
		if _, err := parseKillID(bad); err == nil ||
			output.ToError(err).Code != output.CodeMissingArgument {
			t.Fatalf("parseKillID(%q): err=%v, want MISSING_ARGUMENT", bad, err)
		}
	}
	if id, err := parseKillID("123"); err != nil || id != 123 {
		t.Fatalf("parseKillID(123) = %d, %v", id, err)
	}
}

// TestKillGuards covers the pre-dial rejections: readonly connection,
// non-TTY without --yes, and the dial stage with --yes.
func TestKillGuards(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--port", "1", "--timeout", "2s", "--readonly", "--set-default")

	_, err := runMuxcat(t, "mysql", "kill", "1", "--yes")
	if e := output.ToError(err); err == nil || e.Code != output.CodeReadonlyViolation {
		t.Fatalf("kill on readonly conn: err=%v, want READONLY_VIOLATION", err)
	}

	addConn(t, "down", "--port", "1", "--timeout", "2s")
	_, err = runMuxcat(t, "mysql", "kill", "1", "-c", "down")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("kill without --yes in non-TTY: err=%v, want MISSING_ARGUMENT", err)
	}
	_, err = runMuxcat(t, "mysql", "kill", "abc", "--yes", "-c", "down")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("kill with non-numeric id: err=%v, want MISSING_ARGUMENT", err)
	}
	_, err = runMuxcat(t, "mysql", "kill", "1", "--yes", "-c", "down")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("kill --yes against closed port: err=%v, want CONNECT_FAILED", err)
	}
}

func TestBuildGrantQuery(t *testing.T) {
	cases := []struct {
		arg  string
		want string
	}{
		{"root@localhost", "SHOW GRANTS FOR 'root'@'localhost'"},
		{"us'er@ho\\st", `SHOW GRANTS FOR 'us\'er'@'ho\\st'`},
		{"u@h@extra", "SHOW GRANTS FOR 'u'@'h@extra'"}, // split on the first @
		{"@%", "SHOW GRANTS FOR ''@'%'"},
	}
	for _, tc := range cases {
		q, err := buildGrantQuery(tc.arg)
		if err != nil {
			t.Fatalf("buildGrantQuery(%q): %v", tc.arg, err)
		}
		if q != tc.want {
			t.Fatalf("buildGrantQuery(%q) = %q, want %q", tc.arg, q, tc.want)
		}
	}
	if _, err := buildGrantQuery("noat"); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("buildGrantQuery(noat): err=%v, want MISSING_ARGUMENT", err)
	}
}

func TestCurateReplica(t *testing.T) {
	modern := map[string]any{
		"Source_Host": "db1", "Source_Port": "3306", "Source_User": "repl",
		"Replica_IO_Running": "Yes", "Replica_SQL_Running": "Yes",
		"Seconds_Behind_Source": "0",
		"Retrieved_Gtid_Set":    "uuid:1-9", "Executed_Gtid_Set": "uuid:1-8",
		"Last_Error": "",
	}
	got := curateReplica(modern)
	if got["source_host"] != "db1" || got["source_port"] != "3306" ||
		got["replica_io_running"] != "Yes" || got["replica_sql_running"] != "Yes" ||
		got["seconds_behind_source"] != "0" || got["retrieved_gtid_set"] != "uuid:1-9" ||
		got["last_error"] != "" {
		t.Fatalf("modern mapping = %v", got)
	}

	legacy := map[string]any{
		"Master_Host": "db2", "Master_Port": "3307", "Master_User": "repl",
		"Slave_IO_Running": "No", "Slave_SQL_Running": "Yes",
		"Seconds_Behind_Master": nil,
		"Retrieved_Gtid_Set":    "", "Executed_Gtid_Set": "",
		"Last_Error": "deadlock",
	}
	got = curateReplica(legacy)
	if got["source_host"] != "db2" || got["source_port"] != "3307" ||
		got["replica_io_running"] != "No" || got["last_error"] != "deadlock" {
		t.Fatalf("legacy mapping = %v", got)
	}
	if got["seconds_behind_source"] != nil {
		t.Fatalf("NULL seconds_behind must stay nil, got %v", got["seconds_behind_source"])
	}
}

// TestAdminCommandsClosedPort verifies every admin command reaches the
// dial stage and classifies the failure.
func TestAdminCommandsClosedPort(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")
	cases := [][]string{
		{"mysql", "databases"},
		{"mysql", "status"},
		{"mysql", "status", "--all"},
		{"mysql", "variables"},
		{"mysql", "variables", "max", "--session"},
		{"mysql", "processlist"},
		{"mysql", "users"},
		{"mysql", "grants"},
		{"mysql", "grants", "root@localhost"},
		{"mysql", "engine", "innodb", "status"},
		{"mysql", "replication"},
	}
	for _, args := range cases {
		if _, err := runMuxcat(t, args...); err == nil ||
			output.ToError(err).Code != output.CodeConnectFailed {
			t.Fatalf("%v: err=%v, want CONNECT_FAILED", args, err)
		}
	}

	// An invalid grant target is rejected before dialing.
	if _, err := runMuxcat(t, "mysql", "grants", "noat"); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("grants noat: err=%v, want MISSING_ARGUMENT", err)
	}
}

// TestAdminCommandsMounted verifies the new commands appear in mysql help.
func TestAdminCommandsMounted(t *testing.T) {
	setupEnv(t)
	out, err := runMuxcat(t, "mysql", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"execute", "databases", "status", "variables", "processlist",
		"kill", "users", "grants", "engine", "replication",
	} {
		if !strings.Contains(out, name) {
			t.Fatalf("mysql --help missing %q:\n%s", name, out)
		}
	}
}
