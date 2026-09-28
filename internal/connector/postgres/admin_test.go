package postgres

import (
	"strings"
	"testing"

	"github.com/ravenmk2/muxcat/internal/output"
)

// TestExecuteReadonlyRejectsEverything verifies execute is stricter than
// query on readonly connections: even SELECT is rejected before dialing.
func TestExecuteReadonlyRejectsEverything(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--port", "1", "--timeout", "2s", "--readonly", "--set-default")
	for _, sqlText := range []string{"SELECT 1", "SHOW ALL", "DROP TABLE t"} {
		_, err := runMuxcat(t, "postgres", "execute", sqlText)
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

	_, err := runMuxcat(t, "postgres", "execute", "SELECT 1")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("execute SELECT on writable conn: err=%v, want CONNECT_FAILED", err)
	}

	_, err = runMuxcat(t, "postgres", "execute", "SELECT 1", "--file", "x")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("execute arg + --file: err=%v, want MISSING_ARGUMENT", err)
	}
}

func TestParseKillPID(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-1", "1x", "01", ""} {
		if _, err := parseKillPID(bad); err == nil ||
			output.ToError(err).Code != output.CodeMissingArgument {
			t.Fatalf("parseKillPID(%q): err=%v, want MISSING_ARGUMENT", bad, err)
		}
	}
	if id, err := parseKillPID("123"); err != nil || id != 123 {
		t.Fatalf("parseKillPID(123) = %d, %v", id, err)
	}
}

// TestKillGuards covers the pre-dial rejections: readonly connection,
// non-TTY without --yes, and the dial stage with --yes.
func TestKillGuards(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--port", "1", "--timeout", "2s", "--readonly", "--set-default")

	_, err := runMuxcat(t, "postgres", "kill", "1", "--yes")
	if e := output.ToError(err); err == nil || e.Code != output.CodeReadonlyViolation {
		t.Fatalf("kill on readonly conn: err=%v, want READONLY_VIOLATION", err)
	}

	addConn(t, "down", "--port", "1", "--timeout", "2s")
	_, err = runMuxcat(t, "postgres", "kill", "1", "-c", "down")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("kill without --yes in non-TTY: err=%v, want MISSING_ARGUMENT", err)
	}
	_, err = runMuxcat(t, "postgres", "kill", "abc", "--yes", "-c", "down")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("kill with non-numeric pid: err=%v, want MISSING_ARGUMENT", err)
	}
	_, err = runMuxcat(t, "postgres", "kill", "1", "--yes", "-c", "down")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("kill --yes against closed port: err=%v, want CONNECT_FAILED", err)
	}
	_, err = runMuxcat(t, "postgres", "kill", "1", "--cancel", "--yes", "-c", "down")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("kill --cancel --yes against closed port: err=%v, want CONNECT_FAILED", err)
	}
}

func TestTruncateDisplay(t *testing.T) {
	if got := truncateDisplay("short", 80); got != "short" {
		t.Fatalf("short string = %q", got)
	}
	long := strings.Repeat("x", 100)
	got := truncateDisplay(long, 80)
	if len([]rune(got)) != 80 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated = %q (len %d)", got, len(got))
	}
}

func TestConstraintTypeName(t *testing.T) {
	for in, want := range map[string]string{
		"p": "primary key", "u": "unique", "f": "foreign key",
		"c": "check", "x": "exclude", "z": "z",
	} {
		if got := constraintTypeName(in); got != want {
			t.Fatalf("constraintTypeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("users"); got != `"users"` {
		t.Fatalf("quoteIdent(users) = %q", got)
	}
	if got := quoteIdent(`we"ird`); got != `"we""ird"` {
		t.Fatalf("quoteIdent escaping = %q", got)
	}
}

// TestAdminCommandsClosedPort verifies every admin command reaches the
// dial stage and classifies the failure.
func TestAdminCommandsClosedPort(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")
	cases := [][]string{
		{"postgres", "databases"},
		{"postgres", "schemas"},
		{"postgres", "status"},
		{"postgres", "status", "--all"},
		{"postgres", "settings"},
		{"postgres", "settings", "max"},
		{"postgres", "activity"},
		{"postgres", "roles"},
		{"postgres", "grants"},
		{"postgres", "grants", "app"},
		{"postgres", "extensions"},
		{"postgres", "locks"},
		{"postgres", "replication"},
	}
	for _, args := range cases {
		if _, err := runMuxcat(t, args...); err == nil ||
			output.ToError(err).Code != output.CodeConnectFailed {
			t.Fatalf("%v: err=%v, want CONNECT_FAILED", args, err)
		}
	}
}

// TestAdminCommandsMounted verifies the new commands appear in postgres
// help.
func TestAdminCommandsMounted(t *testing.T) {
	setupEnv(t)
	out, err := runMuxcat(t, "postgres", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"execute", "databases", "schemas", "tables", "schema", "status",
		"settings", "activity", "kill", "roles", "grants", "extensions",
		"locks", "replication",
	} {
		if !strings.Contains(out, name) {
			t.Fatalf("postgres --help missing %q:\n%s", name, out)
		}
	}
}
