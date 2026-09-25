package mysql

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
	"github.com/ravenmk2/muxcat/schema"
)

// runMuxcat executes through the full root command (including persistent flags and the registry mounts) and returns stdout and the error.
func runMuxcat(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runMuxcatIn(t, nil, args...)
}

// runMuxcatIn is runMuxcat with a stubbed stdin.
func runMuxcatIn(t *testing.T, stdin *strings.Reader, args ...string) (string, error) {
	t.Helper()
	root := cli.NewRoot("test")
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if stdin != nil {
		root.SetIn(stdin)
	}
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// testMasterKey is the fixed master key used by tests.
var testMasterKey = bytes.Repeat([]byte{42}, secret.KeySize)

// setupEnv isolates the config directory and stubs the master key
// resolution so encryption never touches the OS keychain.
func setupEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MUXCAT_HOME", t.TempDir())
	t.Setenv("NO_COLOR", "1")
	orig := masterKey
	masterKey = func() ([]byte, secret.Source, error) {
		return testMasterKey, secret.SourceEnv, nil
	}
	t.Cleanup(func() { masterKey = orig })
}

// stubStdinTTY replaces the stdin TTY detection for a test.
func stubStdinTTY(t *testing.T, tty bool) {
	t.Helper()
	orig := stdinIsTTY
	stdinIsTTY = func() bool { return tty }
	t.Cleanup(func() { stdinIsTTY = orig })
}

func addConn(t *testing.T, name string, extra ...string) string {
	t.Helper()
	args := append([]string{"mysql", "conn", "add", name, "--host", "127.0.0.1"}, extra...)
	out, err := runMuxcat(t, args...)
	if err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
	return out
}

func TestDSN(t *testing.T) {
	setupEnv(t)
	enc, err := secret.Encrypt(testMasterKey, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	inst := Instance{Host: "db.local", Port: 3307}
	conn := Connection{Instance: "x", Username: "alice", Password: enc, Database: "app"}

	d, err := dsn(inst, conn, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice:s3cret@tcp(db.local:3307)/app", "parseTime=true", "tls=false"} {
		if !strings.Contains(d, want) {
			t.Fatalf("dsn %q missing %q", d, want)
		}
	}

	// TLS instance flips the tls param.
	d, err = dsn(Instance{Host: "db.local", Port: 3306, TLS: true}, conn, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "tls=true") {
		t.Fatalf("tls dsn = %q", d)
	}

	// --db overrides the connection database for the invocation only.
	d, err = dsn(inst, conn, "other")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "/other?") {
		t.Fatalf("dbOverride dsn = %q", d)
	}
	if conn.Database != "app" {
		t.Fatalf("dbOverride must not mutate the connection, database = %q", conn.Database)
	}

	// Empty database selects no default schema.
	d, err = dsn(inst, Connection{Instance: "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "tcp(db.local:3307)/?") {
		t.Fatalf("no-database dsn = %q", d)
	}
}

func TestGuardQuery(t *testing.T) {
	cases := []struct {
		name     string
		readonly bool
		sql      string
		wantCode string // "" = allowed
	}{
		{"select", true, "SELECT 1", ""},
		{"lowercase select", true, "select * from t", ""},
		{"show", true, "SHOW TABLES", ""},
		{"desc", true, "DESC t", ""},
		{"describe", true, "DESCRIBE t", ""},
		{"explain", true, "EXPLAIN SELECT 1", ""},
		{"with", true, "WITH x AS (SELECT 1) SELECT * FROM x", ""},
		{"use", true, "USE app", ""},
		{"values", true, "VALUES ROW(1, 2)", ""},
		{"table", true, "TABLE t", ""},
		{"leading whitespace", true, "  \n\t SELECT 1", ""},
		{"leading parens", true, "(SELECT 1)", ""},
		{"leading dash comment", true, "-- hello\nSELECT 1", ""},
		{"leading hash comment", true, "# hello\nSELECT 1", ""},
		{"leading block comment", true, "/* hello */ SELECT 1", ""},
		{"leading comment then write", true, "-- hello\nDROP TABLE t", output.CodeReadonlyViolation},
		{"insert", true, "INSERT INTO t VALUES (1)", output.CodeReadonlyViolation},
		{"update", true, "UPDATE t SET a = 1", output.CodeReadonlyViolation},
		{"drop", true, "DROP TABLE t", output.CodeReadonlyViolation},
		{"set rejected", true, "SET SESSION transaction_read_only = 0", output.CodeReadonlyViolation},
		{"empty rejected", true, "  ", output.CodeReadonlyViolation},
		{"unterminated comment rejected", true, "/* never ends", output.CodeReadonlyViolation},
		{"writable allows insert", false, "INSERT INTO t VALUES (1)", ""},
		{"writable allows drop", false, "DROP TABLE t", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guardQuery(Connection{Readonly: tc.readonly}, tc.sql)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("guardQuery = %v, want allowed", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("guardQuery = nil, want %s", tc.wantCode)
			}
			if e := output.ToError(err); e.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s", e.Code, tc.wantCode)
			}
			if got := output.ExitCode(err); got != output.ExitExec {
				t.Fatalf("exit = %d, want %d", got, output.ExitExec)
			}
		})
	}
}

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantExit int
	}{
		{"access denied", &gomysql.MySQLError{Number: 1045, Message: "Access denied for user"}, output.CodeAuthFailed, output.ExitAuth},
		{"read-only option", &gomysql.MySQLError{Number: 1290, Message: "read-only"}, output.CodeReadonlyViolation, output.ExitExec},
		{"read-only transaction", &gomysql.MySQLError{Number: 1792, Message: "read only transaction"}, output.CodeReadonlyViolation, output.ExitExec},
		{"read-only session", &gomysql.MySQLError{Number: 1836, Message: "read only session"}, output.CodeReadonlyViolation, output.ExitExec},
		{"syntax error", &gomysql.MySQLError{Number: 1064, Message: "syntax error"}, output.CodeQueryError, output.ExitExec},
		{"table missing", &gomysql.MySQLError{Number: 1146, Message: "table doesn't exist"}, output.CodeQueryError, output.ExitExec},
		{"dial refused", errors.New("dial tcp 127.0.0.1:3306: connectex: No connection could be made because the target machine actively refused it."), output.CodeConnectFailed, output.ExitConnect},
		{"no such host", errors.New("dial tcp: lookup nope: no such host"), output.CodeConnectFailed, output.ExitConnect},
		{"deadline", context.DeadlineExceeded, output.CodeTimeout, output.ExitConnect},
		{"wrapped mysql error", wrapped1213(), output.CodeQueryError, output.ExitExec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classifyErr(tc.err, "op failed")
			if e.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s (msg: %s)", e.Code, tc.wantCode, e.Message)
			}
			if got := output.ExitCode(e); got != tc.wantExit {
				t.Fatalf("exit = %d, want %d", got, tc.wantExit)
			}
			if !strings.HasPrefix(e.Message, "op failed: ") {
				t.Fatalf("message lacks prefix: %q", e.Message)
			}
		})
	}
}

// wrapped1213 wraps a driver error so errors.As must unwrap it.
func wrapped1213() error {
	return errors.Join(errors.New("io"), &gomysql.MySQLError{Number: 1213, Message: "deadlock"})
}

func TestQueryTimeout(t *testing.T) {
	if d, err := queryTimeout(Connection{}, 30*time.Second); err != nil || d != 30*time.Second {
		t.Fatalf("no connection timeout: d=%v err=%v", d, err)
	}
	if d, err := queryTimeout(Connection{Timeout: "5s"}, 30*time.Second); err != nil || d != 5*time.Second {
		t.Fatalf("connection timeout overrides: d=%v err=%v", d, err)
	}
	for _, bad := range []string{"banana", "0s", "-5s"} {
		if _, err := queryTimeout(Connection{Timeout: bad}, 30*time.Second); err == nil ||
			output.ToError(err).Code != output.CodeConfigInvalid {
			t.Fatalf("timeout %q: err=%v, want CONFIG_INVALID", bad, err)
		}
	}
}

func TestIsQuery(t *testing.T) {
	for _, q := range []string{"SELECT 1", "show tables", "  /* c */ WITH x AS (SELECT 1) SELECT * FROM x", "DESC t"} {
		if !isQuery(q) {
			t.Fatalf("isQuery(%q) = false, want true", q)
		}
	}
	for _, q := range []string{"INSERT INTO t VALUES (1)", "update t set a=1", "", "USE app"} {
		if isQuery(q) {
			t.Fatalf("isQuery(%q) = true, want false", q)
		}
	}
}

func TestResolveSQLInput(t *testing.T) {
	// Positional argument wins.
	cmd := newQueryCmd()
	got, err := resolveSQLInput(cmd, []string{"SELECT 1"})
	if err != nil || got != "SELECT 1" {
		t.Fatalf("positional: got=%q err=%v", got, err)
	}

	// --file reads a file.
	f := filepath.Join(t.TempDir(), "q.sql")
	if err := os.WriteFile(f, []byte("SELECT 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = newQueryCmd()
	if err := cmd.Flags().Set("file", f); err != nil {
		t.Fatal(err)
	}
	got, err = resolveSQLInput(cmd, nil)
	if err != nil || got != "SELECT 2;\n" {
		t.Fatalf("--file: got=%q err=%v", got, err)
	}

	// Unreadable --file is MISSING_ARGUMENT.
	cmd = newQueryCmd()
	if err := cmd.Flags().Set("file", filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSQLInput(cmd, nil); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("unreadable --file: err=%v", err)
	}

	// Positional argument and --file together are rejected.
	cmd = newQueryCmd()
	if err := cmd.Flags().Set("file", f); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSQLInput(cmd, []string{"SELECT 1"}); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("arg + --file: err=%v", err)
	}

	// --file - reads stdin.
	stubStdinTTY(t, true) // must not matter; - forces stdin
	cmd = newQueryCmd()
	if err := cmd.Flags().Set("file", "-"); err != nil {
		t.Fatal(err)
	}
	cmd.SetIn(strings.NewReader("SELECT 3"))
	got, err = resolveSQLInput(cmd, nil)
	if err != nil || got != "SELECT 3" {
		t.Fatalf("--file -: got=%q err=%v", got, err)
	}

	// No argument on a TTY is MISSING_ARGUMENT with a usage hint.
	stubStdinTTY(t, true)
	cmd = newQueryCmd()
	if _, err := resolveSQLInput(cmd, nil); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("TTY without input: err=%v", err)
	} else if output.ToError(err).Hint == "" {
		t.Fatal("MISSING_ARGUMENT should carry a hint")
	}

	// No argument with piped stdin reads it.
	stubStdinTTY(t, false)
	cmd = newQueryCmd()
	cmd.SetIn(strings.NewReader("SELECT 4"))
	got, err = resolveSQLInput(cmd, nil)
	if err != nil || got != "SELECT 4" {
		t.Fatalf("piped stdin: got=%q err=%v", got, err)
	}
}

func TestConnLifecycle(t *testing.T) {
	setupEnv(t)

	out := addConn(t, "local", "--port", "3307", "--database", "app", "--username", "alice",
		"--password", "s3cret", "--readonly", "--timeout", "5s", "--set-default")
	if !strings.Contains(out, "Warning: --password") {
		t.Fatalf("plaintext password flag should warn on stderr: %q", out)
	}
	if !strings.Contains(out, "added connection local (127.0.0.1:3307, db app)") {
		t.Fatalf("add message = %q", out)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConnection != "local" {
		t.Fatalf("defaultConnection = %q, want local", cfg.DefaultConnection)
	}
	inst := cfg.Instances["local"]
	if inst.Host != "127.0.0.1" || inst.Port != 3307 {
		t.Fatalf("instance = %+v", inst)
	}
	conn := cfg.Connections["local"]
	if conn.Instance != "local" || conn.Username != "alice" || conn.Database != "app" ||
		!conn.Readonly || conn.Timeout != "5s" {
		t.Fatalf("connection = %+v", conn)
	}
	if !strings.HasPrefix(conn.Password, secret.Prefix) {
		t.Fatalf("password should be an enc:v1: blob, got %q", conn.Password)
	}
	if strings.Contains(conn.Password, "s3cret") {
		t.Fatal("password blob contains plaintext")
	}
	plain, err := secret.Decrypt(testMasterKey, conn.Password)
	if err != nil || string(plain) != "s3cret" {
		t.Fatalf("decrypt roundtrip = %q, %v", plain, err)
	}

	// conn ls shows the connection without the password.
	out, err = runMuxcat(t, "mysql", "conn", "ls")
	if err != nil || !strings.Contains(out, "local") || !strings.Contains(out, "3307") ||
		!strings.Contains(out, "alice") || !strings.Contains(out, "app") {
		t.Fatalf("conn ls: err=%v out=%q", err, out)
	}
	if strings.Contains(out, "enc:v1:") {
		t.Fatalf("conn ls leaked the password blob: %q", out)
	}

	// conn show never echoes the password.
	out, err = runMuxcat(t, "mysql", "conn", "show", "local")
	if err != nil || !strings.Contains(out, "readonly") || !strings.Contains(out, "database") {
		t.Fatalf("conn show: err=%v out=%q", err, out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "enc:v1:") {
		t.Fatalf("conn show leaked the password: %q", out)
	}

	// duplicate add is rejected
	if _, err := runMuxcat(t, "mysql", "conn", "add", "local", "--host", "other"); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("duplicate add: err=%v", err)
	}

	// switching the default to a nonexistent connection should report CONN_NOT_FOUND
	if _, err := runMuxcat(t, "mysql", "conn", "default", "nope"); err == nil ||
		output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("conn default nope: err=%v", err)
	}

	if out, err := runMuxcat(t, "mysql", "conn", "rm", "local", "--yes"); err != nil {
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
		t.Fatalf("defaultConnection should be cleared, got %q", cfg.DefaultConnection)
	}
}

func TestConnAddValidation(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "mysql", "conn", "add", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("add without --host in non-TTY: err=%v", err)
	}
	_, err = runMuxcat(t, "mysql", "conn", "add", "blank", "--host", "   ")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("add with blank --host in non-TTY: err=%v", err)
	}
	_, err = runMuxcat(t, "mysql", "conn", "add", "bad", "--host", "h", "--port", "70000")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with invalid --port: err=%v", err)
	}
	_, err = runMuxcat(t, "mysql", "conn", "add", "bad2", "--host", "h", "--timeout", "banana")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with invalid --timeout: err=%v", err)
	}
	_, err = runMuxcat(t, "mysql", "conn", "add", "bad3", "--host", "h", "--timeout", "0s")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with --timeout 0s: err=%v", err)
	}
}

func TestConnRmRequiresYesNonTTY(t *testing.T) {
	setupEnv(t)
	addConn(t, "local")
	_, err := runMuxcat(t, "mysql", "conn", "rm", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("rm without --yes in non-TTY: err=%v", err)
	}
}

func TestConnNotFound(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "mysql", "query", "SELECT 1")
	if err == nil || output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("query without any connection: err=%v", err)
	}
}

func TestConnectFailedUnreachable(t *testing.T) {
	setupEnv(t)
	// Port 1 is closed; the failure should classify as CONNECT_FAILED (3).
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")
	_, err := runMuxcat(t, "mysql", "conn", "test", "down")
	if err == nil {
		t.Fatal("conn test against a closed port should fail")
	}
	e := output.ToError(err)
	if e.Code != output.CodeConnectFailed {
		t.Fatalf("code = %s, want %s (msg: %s)", e.Code, output.CodeConnectFailed, e.Message)
	}
	if output.ExitCode(e) != output.ExitConnect {
		t.Fatalf("exit = %d, want %d", output.ExitCode(e), output.ExitConnect)
	}

	// query/tables/schema reach the dial stage too.
	for _, args := range [][]string{
		{"mysql", "query", "SELECT 1"},
		{"mysql", "query", "INSERT INTO t VALUES (1)"},
		{"mysql", "tables"},
		{"mysql", "schema"},
		{"mysql", "schema", "t"},
	} {
		if _, err := runMuxcat(t, args...); err == nil ||
			output.ToError(err).Code != output.CodeConnectFailed {
			t.Fatalf("%v: err=%v, want CONNECT_FAILED", args, err)
		}
	}
}

// TestGuardBlocksWithoutServer verifies the readonly guard fires before any
// network access: write statements against a connection pointing at a
// closed port surface as READONLY_VIOLATION, reads as CONNECT_FAILED.
func TestGuardBlocksWithoutServer(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--port", "1", "--timeout", "2s", "--readonly", "--set-default")

	_, err := runMuxcat(t, "mysql", "query", "DROP TABLE t")
	if e := output.ToError(err); err == nil || e.Code != output.CodeReadonlyViolation {
		t.Fatalf("DROP on readonly conn: err=%v", err)
	}
	_, err = runMuxcat(t, "mysql", "query", "SET SESSION transaction_read_only = 0")
	if e := output.ToError(err); err == nil || e.Code != output.CodeReadonlyViolation {
		t.Fatalf("SET on readonly conn: err=%v", err)
	}

	// A read passes the guard and fails later, at the dial stage.
	_, err = runMuxcat(t, "mysql", "query", "SELECT 1")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("SELECT on readonly conn: err=%v, want CONNECT_FAILED", err)
	}
}

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{
	  "version": 1,
	  "instances": {
	    "local": {"host": "127.0.0.1", "port": 3306, "tls": true}
	  },
	  "connections": {
	    "local": {"instance": "local", "username": "root", "password": "enc:v1:abc", "database": "app", "readonly": true, "timeout": "5s"}
	  },
	  "defaultConnection": "local"
	}`)
	if err := schema.Validate("mysql.json", valid); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}

	missingHost := []byte(`{"version": 1, "instances": {"local": {"port": 3306}}}`)
	if err := schema.Validate("mysql.json", missingHost); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("doc without host: err=%v", err)
	}

	badPort := []byte(`{"version": 1, "instances": {"local": {"host": "h", "port": 70000}}}`)
	if err := schema.Validate("mysql.json", badPort); err == nil {
		t.Fatal("doc with out-of-range port should be rejected")
	}

	extra := []byte(`{"version": 1, "connections": {"local": {"instance": "local", "db": 0}}}`)
	if err := schema.Validate("mysql.json", extra); err == nil {
		t.Fatal("doc with an unknown connection field should be rejected")
	}
}

// TestConnectorMounted verifies the connector registers on the root
// command via the registry.
func TestConnectorMounted(t *testing.T) {
	setupEnv(t)
	out, err := runMuxcat(t, "connector", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mysql") {
		t.Fatalf("connector ls should list mysql: %q", out)
	}
}
