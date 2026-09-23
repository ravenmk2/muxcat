package sqlite

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// runMuxcat executes through the full root command (including persistent flags and the registry mounts) and returns stdout and the error.
func runMuxcat(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := cli.NewRoot("test")
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// setupEnv isolates the config directory and returns a database file path.
func setupEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("MUXCAT_HOME", t.TempDir())
	t.Setenv("NO_COLOR", "1")
	return filepath.Join(t.TempDir(), "test.db")
}

func addConn(t *testing.T, name, dbPath string, extra ...string) {
	t.Helper()
	args := append([]string{"sqlite", "conn", "add", name, "--path", dbPath}, extra...)
	if out, err := runMuxcat(t, args...); err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
}

func queryOK(t *testing.T, sql string, extra ...string) string {
	t.Helper()
	args := append([]string{"sqlite", "query", sql}, extra...)
	out, err := runMuxcat(t, args...)
	if err != nil {
		t.Fatalf("query %q failed: %v\n%s", sql, err, out)
	}
	return out
}

func TestDSN(t *testing.T) {
	d := dsn("app.db", false)
	if !strings.HasPrefix(d, "file:app.db?") ||
		!strings.Contains(d, "_pragma=busy_timeout(5000)") ||
		!strings.Contains(d, "_pragma=foreign_keys(1)") {
		t.Fatalf("unexpected dsn: %s", d)
	}
	if strings.Contains(d, "mode=ro") {
		t.Fatalf("non-readonly dsn should not carry mode=ro: %s", d)
	}
	if ro := dsn("app.db", true); !strings.Contains(ro, "mode=ro") {
		t.Fatalf("readonly dsn should carry mode=ro: %s", ro)
	}
	if m := dsn(":memory:", false); m != ":memory:" {
		t.Fatalf("memory dsn = %q, want :memory:", m)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := ExpandHome("~/x.db"); got != filepath.Join(home, "x.db") {
		t.Fatalf("ExpandHome(~/x.db) = %q", got)
	}
	if got := ExpandHome("plain.db"); got != "plain.db" {
		t.Fatalf("ExpandHome(plain.db) = %q", got)
	}
}

func TestConnLifecycle(t *testing.T) {
	dbPath := setupEnv(t)

	addConn(t, "local", dbPath, "--set-default")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConnection != "local" {
		t.Fatalf("default_connection = %q, want local", cfg.DefaultConnection)
	}
	conn := cfg.Connections["local"]
	if conn.Instance != "local" || conn.Readonly {
		t.Fatalf("connection = %+v", conn)
	}
	if cfg.Instances["local"].Path != dbPath {
		t.Fatalf("instance path = %q, want %q", cfg.Instances["local"].Path, dbPath)
	}

	out, err := runMuxcat(t, "sqlite", "conn", "ls")
	if err != nil || !strings.Contains(out, "local") {
		t.Fatalf("conn ls: err=%v out=%q", err, out)
	}

	// switching the default to a nonexistent connection should report CONN_NOT_FOUND
	if _, err := runMuxcat(t, "sqlite", "conn", "default", "nope"); err == nil ||
		output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("conn default nope: err=%v", err)
	}

	if out, err := runMuxcat(t, "sqlite", "conn", "rm", "local", "--yes"); err != nil {
		t.Fatalf("conn rm: %v\n%s", err, out)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Connections["local"]; ok {
		t.Fatal("connection should be removed")
	}
	if _, ok := cfg.Instances["local"]; ok {
		t.Fatal("unreferenced instance should be removed")
	}
	if cfg.DefaultConnection != "" {
		t.Fatalf("default_connection should be cleared, got %q", cfg.DefaultConnection)
	}
}

func TestConnRmRequiresYesNonTTY(t *testing.T) {
	dbPath := setupEnv(t)
	addConn(t, "local", dbPath)
	_, err := runMuxcat(t, "sqlite", "conn", "rm", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("rm without --yes in non-TTY: err=%v", err)
	}
}

func TestConnAddMissingPathNonTTY(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "sqlite", "conn", "add", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("add without --path in non-TTY: err=%v", err)
	}
	if hint := output.ToError(err).Hint; hint == "" {
		t.Fatal("MISSING_ARGUMENT should carry a hint")
	}
}

func TestQueryRoundTrip(t *testing.T) {
	dbPath := setupEnv(t)
	addConn(t, "local", dbPath, "--set-default")

	queryOK(t, "CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT)")
	out := queryOK(t, "INSERT INTO t(name) VALUES('a'),('b')")
	if !strings.Contains(out, "2") {
		t.Fatalf("insert output = %q, want rows affected 2", out)
	}

	out = queryOK(t, "SELECT id, name FROM t ORDER BY id")
	if !strings.Contains(out, "alice") && !strings.Contains(out, "name") {
		t.Fatalf("select output = %q", out)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Fatalf("select output missing rows: %q", out)
	}

	// --json: verify the envelope + columns/rows shape
	out = queryOK(t, "SELECT id, name FROM t ORDER BY id", "--json")
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Columns []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"columns"`
			Rows         [][]any `json:"rows"`
			RowCount     int     `json:"row_count"`
			RowsAffected int     `json:"rows_affected"`
		} `json:"data"`
		Meta struct {
			Connector  string `json:"connector"`
			Connection string `json:"connection"`
			Truncated  bool   `json:"truncated"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("invalid envelope JSON: %v\n%s", err, out)
	}
	if !env.OK || env.Meta.Connector != "sqlite" || env.Meta.Connection != "local" {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Data.RowCount != 2 || len(env.Data.Rows) != 2 || len(env.Data.Columns) != 2 {
		t.Fatalf("data = %+v", env.Data)
	}
	if env.Data.Columns[0].Name != "id" || env.Data.Columns[0].Type == "" {
		t.Fatalf("columns = %+v", env.Data.Columns)
	}
}

func TestQueryLimitTruncation(t *testing.T) {
	dbPath := setupEnv(t)
	addConn(t, "local", dbPath, "--set-default")
	queryOK(t, "CREATE TABLE t(n INTEGER)")
	queryOK(t, "INSERT INTO t(n) VALUES(1),(2),(3),(4),(5)")

	out := queryOK(t, "SELECT n FROM t", "--limit", "2", "--json")
	var env struct {
		Data struct {
			RowCount int `json:"row_count"`
		} `json:"data"`
		Meta struct {
			Truncated bool `json:"truncated"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("invalid envelope: %v\n%s", err, out)
	}
	if env.Data.RowCount != 2 || !env.Meta.Truncated {
		t.Fatalf("limit=2: row_count=%d truncated=%v, want 2/true",
			env.Data.RowCount, env.Meta.Truncated)
	}

	out = queryOK(t, "SELECT n FROM t", "--limit", "100", "--json")
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.RowCount != 5 || env.Meta.Truncated {
		t.Fatalf("limit=100: row_count=%d truncated=%v, want 5/false",
			env.Data.RowCount, env.Meta.Truncated)
	}
}

func TestReadonlyViolation(t *testing.T) {
	dbPath := setupEnv(t)
	addConn(t, "rw", dbPath, "--set-default")
	queryOK(t, "CREATE TABLE t(id INTEGER PRIMARY KEY)")
	queryOK(t, "INSERT INTO t(id) VALUES(1)")

	addConn(t, "ro", dbPath, "--readonly")
	_, err := runMuxcat(t, "sqlite", "query", "INSERT INTO t(id) VALUES(2)", "-c", "ro")
	if err == nil {
		t.Fatal("readonly insert should fail")
	}
	e := output.ToError(err)
	if e.Code != output.CodeReadonlyViolation {
		t.Fatalf("code = %s, want %s\nmessage: %s", e.Code, output.CodeReadonlyViolation, e.Message)
	}
	if output.ExitCode(e) != output.ExitExec {
		t.Fatalf("exit = %d, want %d", output.ExitCode(e), output.ExitExec)
	}

	// queries on a readonly connection are unaffected
	out := queryOK(t, "SELECT id FROM t", "-c", "ro")
	if !strings.Contains(out, "1") {
		t.Fatalf("readonly select output = %q", out)
	}
}

func TestConnNotFound(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "sqlite", "query", "SELECT 1")
	if err == nil || output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("query without any connection: err=%v", err)
	}
}

func TestTablesAndSchema(t *testing.T) {
	dbPath := setupEnv(t)
	addConn(t, "local", dbPath, "--set-default")
	queryOK(t, "CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT)")
	queryOK(t, "CREATE VIEW v AS SELECT name FROM t")

	out, err := runMuxcat(t, "sqlite", "tables")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "t") || !strings.Contains(out, "v") || !strings.Contains(out, "view") {
		t.Fatalf("tables output = %q", out)
	}

	out, err = runMuxcat(t, "sqlite", "schema", "t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "CREATE TABLE t") {
		t.Fatalf("schema t output = %q", out)
	}
	if strings.Contains(out, "CREATE VIEW") {
		t.Fatalf("schema t should not contain view DDL: %q", out)
	}

	if _, err := runMuxcat(t, "sqlite", "schema", "nope"); err == nil ||
		output.ToError(err).Code != output.CodeQueryError {
		t.Fatalf("schema nope: err=%v", err)
	}

	out, err = runMuxcat(t, "sqlite", "conn", "test", "local")
	if err != nil || !strings.Contains(out, "connection ok") {
		t.Fatalf("conn test: err=%v out=%q", err, out)
	}
}
