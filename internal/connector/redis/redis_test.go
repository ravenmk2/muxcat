package redis

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
	"github.com/ravenmk2/muxcat/schema"
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

func addConn(t *testing.T, name string, extra ...string) string {
	t.Helper()
	args := append([]string{"redis", "conn", "add", name, "--host", "127.0.0.1"}, extra...)
	out, err := runMuxcat(t, args...)
	if err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
	return out
}

func TestGuardCommand(t *testing.T) {
	cases := []struct {
		name           string
		readonly       bool
		allowDangerous bool
		cmd            string
		args           []any
		wantCode       string // "" = allowed
	}{
		{"readonly allows GET", true, false, "GET", nil, ""},
		{"readonly allows SCAN", true, false, "SCAN", nil, ""},
		{"readonly allows CONFIG GET", true, false, "CONFIG", []any{"GET"}, ""},
		{"readonly allows lowercase config get", true, false, "config", []any{"get"}, ""},
		{"readonly allows MEMORY USAGE", true, false, "MEMORY", []any{"USAGE", "k"}, ""},
		{"readonly rejects SET", true, false, "SET", nil, output.CodeReadonlyViolation},
		{"readonly rejects EVAL", true, false, "EVAL", nil, output.CodeReadonlyViolation},
		{"readonly rejects CONFIG SET", true, false, "CONFIG", []any{"SET"}, output.CodeReadonlyViolation},
		{"readonly rejects MEMORY STATS", true, false, "MEMORY", []any{"STATS"}, output.CodeReadonlyViolation},
		{"readonly rejects HSET", true, false, "HSET", nil, output.CodeReadonlyViolation},
		{"readonly violation wins over dangerous", true, false, "FLUSHALL", nil, output.CodeReadonlyViolation},
		{"dangerous FLUSHALL blocked", false, false, "FLUSHALL", nil, output.CodeUnsupportedOperation},
		{"dangerous lowercase flushall blocked", false, false, "flushall", nil, output.CodeUnsupportedOperation},
		{"dangerous KEYS blocked", false, false, "KEYS", nil, output.CodeUnsupportedOperation},
		{"dangerous SCRIPT blocked", false, false, "SCRIPT", []any{"LOAD"}, output.CodeUnsupportedOperation},
		{"dangerous CONFIG SET blocked", false, false, "CONFIG", []any{"SET", "maxmemory", "1gb"}, output.CodeUnsupportedOperation},
		{"dangerous SHUTDOWN blocked", false, false, "SHUTDOWN", nil, output.CodeUnsupportedOperation},
		{"dangerous SWAPDB blocked", false, false, "SWAPDB", nil, output.CodeUnsupportedOperation},
		{"allowDangerous permits FLUSHALL", false, true, "FLUSHALL", nil, ""},
		{"allowDangerous permits CONFIG SET", false, true, "CONFIG", []any{"SET"}, ""},
		{"ordinary writes allowed when writable", false, false, "SET", nil, ""},
		{"CONFIG GET allowed by default", false, false, "CONFIG", []any{"GET"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := Connection{Readonly: tc.readonly, AllowDangerous: tc.allowDangerous}
			err := guardCommand(conn, tc.cmd, tc.args...)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("guardCommand = %v, want allowed", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("guardCommand = nil, want %s", tc.wantCode)
			}
			if e := output.ToError(err); e.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s", e.Code, tc.wantCode)
			}
		})
	}
}

func TestGuardExitCodes(t *testing.T) {
	err := guardCommand(Connection{Readonly: true}, "SET")
	if got := output.ExitCode(err); got != output.ExitExec {
		t.Fatalf("readonly violation exit = %d, want %d", got, output.ExitExec)
	}
	// UNSUPPORTED_OPERATION maps to the usage class by the central exit
	// code table (internal/output), so dangerous-command blocks exit 2.
	err = guardCommand(Connection{}, "FLUSHALL")
	if got := output.ExitCode(err); got != output.ExitUsage {
		t.Fatalf("dangerous block exit = %d, want %d", got, output.ExitUsage)
	}
}

func TestRenderString(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		binary   string
		maxBytes int
		want     string
		trunc    bool
	}{
		{"utf8 as-is", "héllo wörld", "hex", 4096, "héllo wörld", false},
		{"invalid utf8 hex", "\xff\xfe\x00", "hex", 4096, "fffe00", false},
		{"invalid utf8 base64", "\xff\xfe\x00", "base64", 4096, base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00}), false},
		{"control chars hex", "a\x01\x02b", "hex", 4096, "61010262", false},
		{"DEL is not printable", "a\x7fb", "hex", 4096, "617f62", false},
		{"newline tab cr as-is", "line1\nline2\t\r", "hex", 4096, "line1\nline2\t\r", false},
		{"control chars base64", "a\x01\x02b", "base64", 4096, base64.StdEncoding.EncodeToString([]byte{'a', 0x01, 0x02, 'b'}), false},
		{"truncate utf8", "hello", "hex", 3, "hel", true},
		{"truncate at rune boundary", "héllo", "hex", 2, "h", true},
		{"truncate binary then hex", "\xff\xfe\x00\x01", "hex", 2, "fffe", true},
		{"max-bytes 0 disables truncation", strings.Repeat("a", 100), "hex", 0, strings.Repeat("a", 100), false},
		{"binary max-bytes 0", "\xff\xfe", "hex", 0, "fffe", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, trunc := renderString(tc.in, tc.binary, tc.maxBytes)
			if got != tc.want || trunc != tc.trunc {
				t.Fatalf("renderString(%q, %s, %d) = (%q, %v), want (%q, %v)",
					tc.in, tc.binary, tc.maxBytes, got, trunc, tc.want, tc.trunc)
			}
		})
	}
}

func TestRenderReply(t *testing.T) {
	r, trunc := renderReply(nil, "hex", 4096)
	if r.Type != "null" || r.Value != nil || trunc {
		t.Fatalf("nil reply = %+v, %v", r, trunc)
	}

	r, trunc = renderReply(int64(42), "hex", 4096)
	if r.Type != "integer" || r.Value != int64(42) || trunc {
		t.Fatalf("integer reply = %+v, %v", r, trunc)
	}

	// RESP3 types returned by go-redis ReadReply (internal/proto/reader.go).
	r, trunc = renderReply(float64(1.5), "hex", 4096)
	if r.Type != "double" || r.Value != float64(1.5) || trunc {
		t.Fatalf("double reply = %+v, %v", r, trunc)
	}
	r, trunc = renderReply(true, "hex", 4096)
	if r.Type != "boolean" || r.Value != true || trunc {
		t.Fatalf("boolean reply = %+v, %v", r, trunc)
	}
	bigInt, _ := new(big.Int).SetString("349289032840923850932485994385934991243", 10)
	r, trunc = renderReply(bigInt, "hex", 4096)
	if r.Type != "integer" || r.Value != "349289032840923850932485994385934991243" || trunc {
		t.Fatalf("big int reply = %+v, %v", r, trunc)
	}

	// Recursion: every string element is rendered independently; a binary
	// element is encoded, a long element is truncated, and truncation
	// propagates up.
	r, trunc = renderReply([]any{"ok", "\xff", "abcdef", int64(1)}, "hex", 3)
	if r.Type != "array" || !trunc {
		t.Fatalf("array reply = %+v, trunc=%v", r, trunc)
	}
	items := r.Value.([]any)
	if items[0].(reply).Value != "ok" || items[0].(reply).Type != "string" {
		t.Fatalf("item[0] = %+v", items[0])
	}
	if items[1].(reply).Value != "ff" {
		t.Fatalf("item[1] = %+v, want hex-encoded ff", items[1])
	}
	if items[2].(reply).Value != "abc" {
		t.Fatalf("item[2] = %+v, want truncated abc", items[2])
	}
	if items[3].(reply).Type != "integer" {
		t.Fatalf("item[3] = %+v", items[3])
	}

	r, _ = renderReply(map[any]any{"k": "v"}, "hex", 4096)
	if r.Type != "map" {
		t.Fatalf("map reply = %+v", r)
	}
	if r.Value.(map[string]any)["k"].(reply).Value != "v" {
		t.Fatalf("map value = %+v", r.Value)
	}
}

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantExit int
	}{
		{"NOAUTH", errors.New("NOAUTH Authentication required."), output.CodeAuthFailed, output.ExitAuth},
		{"WRONGPASS", errors.New("WRONGPASS invalid username-password pair"), output.CodeAuthFailed, output.ExitAuth},
		{"NOPERM", errors.New("NOPERM this user has no permissions"), output.CodeAuthFailed, output.ExitAuth},
		{"dial refused", errors.New("dial tcp 127.0.0.1:6379: connectex: No connection could be made because the target machine actively refused it."), output.CodeConnectFailed, output.ExitConnect},
		{"no such host", errors.New("dial tcp: lookup nope: no such host"), output.CodeConnectFailed, output.ExitConnect},
		{"deadline", context.DeadlineExceeded, output.CodeTimeout, output.ExitConnect},
		{"server READONLY", errors.New("READONLY You can't write against a read only replica."), output.CodeReadonlyViolation, output.ExitExec},
		{"WRONGTYPE", errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"), output.CodeQueryError, output.ExitExec},
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

func TestConnLifecycle(t *testing.T) {
	setupEnv(t)

	out := addConn(t, "local", "--port", "6380", "--db", "2", "--password", "s3cret",
		"--readonly", "--timeout", "5s", "--set-default")
	if !strings.Contains(out, "Warning: --password") {
		t.Fatalf("plaintext password flag should warn on stderr: %q", out)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConnection != "local" {
		t.Fatalf("defaultConnection = %q, want local", cfg.DefaultConnection)
	}
	inst := cfg.Instances["local"]
	if inst.Host != "127.0.0.1" || inst.Port != 6380 {
		t.Fatalf("instance = %+v", inst)
	}
	if inst.Username != "" || inst.Password != "" || inst.DB != 0 {
		t.Fatalf("credentials/db must live on the connection, instance = %+v", inst)
	}
	conn := cfg.Connections["local"]
	if conn.Instance != "local" || !conn.Readonly || conn.Timeout != "5s" || conn.AllowDangerous {
		t.Fatalf("connection = %+v", conn)
	}
	if conn.DB == nil || *conn.DB != 2 {
		t.Fatalf("connection db = %v, want 2", conn.DB)
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
	out, err = runMuxcat(t, "redis", "conn", "ls")
	if err != nil || !strings.Contains(out, "local") || !strings.Contains(out, "6380") {
		t.Fatalf("conn ls: err=%v out=%q", err, out)
	}
	if strings.Contains(out, "enc:v1:") {
		t.Fatalf("conn ls leaked the password blob: %q", out)
	}

	// conn show never echoes the password.
	out, err = runMuxcat(t, "redis", "conn", "show", "local")
	if err != nil || !strings.Contains(out, "allowDangerous") {
		t.Fatalf("conn show: err=%v out=%q", err, out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "enc:v1:") {
		t.Fatalf("conn show leaked the password: %q", out)
	}

	// switching the default to a nonexistent connection should report CONN_NOT_FOUND
	if _, err := runMuxcat(t, "redis", "conn", "default", "nope"); err == nil ||
		output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("conn default nope: err=%v", err)
	}

	if out, err := runMuxcat(t, "redis", "conn", "rm", "local", "--yes"); err != nil {
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
	_, err := runMuxcat(t, "redis", "conn", "add", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("add without --host in non-TTY: err=%v", err)
	}
	_, err = runMuxcat(t, "redis", "conn", "add", "blank", "--host", "   ")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("add with blank --host in non-TTY: err=%v", err)
	}
	_, err = runMuxcat(t, "redis", "conn", "add", "bad", "--host", "h", "--timeout", "banana")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with invalid --timeout: err=%v", err)
	}
	_, err = runMuxcat(t, "redis", "conn", "add", "bad2", "--host", "h", "--timeout", "0s")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with --timeout 0s: err=%v", err)
	}
}

func TestConnAddInstanceReferenced(t *testing.T) {
	setupEnv(t)
	// Connection "b" references instance "a" directly in the config file.
	cfg := &Config{
		Version:           1,
		Instances:         map[string]Instance{"a": {Host: "127.0.0.1", Port: 6379}},
		Connections:       map[string]Connection{"b": {Instance: "a"}},
		DefaultConnection: "b",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	_, err := runMuxcat(t, "redis", "conn", "add", "a", "--host", "other")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add over a referenced instance: err=%v", err)
	}
	if hint := output.ToError(err).Hint; !strings.Contains(hint, "b") {
		t.Fatalf("hint should name the referencing connection, got %q", hint)
	}

	// With the referencing connection gone, the orphan instance may be
	// overwritten (rebuild).
	delete(cfg.Connections, "b")
	cfg.DefaultConnection = ""
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	addConn(t, "a", "--host", "other", "--port", "6400")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if inst := cfg.Instances["a"]; inst.Host != "other" || inst.Port != 6400 {
		t.Fatalf("orphan instance should be rebuilt, got %+v", inst)
	}
}

func TestConnRmRequiresYesNonTTY(t *testing.T) {
	setupEnv(t)
	addConn(t, "local")
	_, err := runMuxcat(t, "redis", "conn", "rm", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("rm without --yes in non-TTY: err=%v", err)
	}
}

func TestConnNotFound(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "redis", "get", "k")
	if err == nil || output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("get without any connection: err=%v", err)
	}
}

// TestGuardBlocksWithoutServer verifies interception happens before any
// network access: a readonly/dangerous rejection fires against a
// connection pointing at a closed port.
func TestGuardBlocksWithoutServer(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--port", "1", "--readonly", "--set-default")
	_, err := runMuxcat(t, "redis", "set", "k", "v")
	if err == nil || output.ToError(err).Code != output.CodeReadonlyViolation {
		t.Fatalf("set on readonly conn: err=%v", err)
	}
	_, err = runMuxcat(t, "redis", "exec", "FLUSHALL")
	if err == nil || output.ToError(err).Code != output.CodeReadonlyViolation {
		t.Fatalf("exec FLUSHALL on readonly conn: err=%v", err)
	}
}

func TestConnectFailedUnreachable(t *testing.T) {
	setupEnv(t)
	// Port 1 is closed; the failure should classify as CONNECT_FAILED (3).
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")
	_, err := runMuxcat(t, "redis", "conn", "test", "down")
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
}

// TestSetFromFile covers the --file value source of redis set: conflict with
// the positional value, a missing value, an unreadable file (fails before
// dialing), and a readable file (reaches the dial stage on a closed port).
func TestSetFromFile(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")

	_, err := runMuxcat(t, "redis", "set", "k", "v", "--file", "x")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("positional value + --file: err=%v", err)
	}

	_, err = runMuxcat(t, "redis", "set", "k")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("no value at all: err=%v", err)
	}

	_, err = runMuxcat(t, "redis", "set", "k", "--file", filepath.Join(t.TempDir(), "nope"))
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("unreadable file: err=%v", err)
	}

	f := filepath.Join(t.TempDir(), "v.bin")
	if err := os.WriteFile(f, []byte{'a', 0, 1, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runMuxcat(t, "redis", "set", "k", "--file", f)
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("readable file should reach the dial stage: err=%v", err)
	}
}

// TestNegativeArgsNotParsedAsFlags verifies that negative positional
// arguments (the Redis 0 -1 idiom) reach the dial stage instead of being
// rejected as unknown flags, on lrange/zrange/exec. The connection points
// at a closed port, so a parsed command surfaces as CONNECT_FAILED.
func TestNegativeArgsNotParsedAsFlags(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")
	cases := [][]string{
		{"redis", "lrange", "queue", "0", "-1"},
		{"redis", "zrange", "board", "0", "-1"},
		{"redis", "exec", "ZRANGE", "board", "0", "-1"},
		// flags still work when placed before the positional arguments
		{"redis", "exec", "--binary", "base64", "GET", "k"},
		{"redis", "lrange", "--max-bytes", "8", "queue", "0", "-1"},
	}
	for _, args := range cases {
		_, err := runMuxcat(t, args...)
		if err == nil {
			t.Fatalf("%v: should fail against a closed port", args)
		}
		if e := output.ToError(err); e.Code != output.CodeConnectFailed {
			t.Fatalf("%v: code = %s, want %s (msg: %s)", args, e.Code, output.CodeConnectFailed, e.Message)
		}
	}
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

func TestEffectiveDB(t *testing.T) {
	inst := Instance{DB: 2}
	if got := effectiveDB(inst, Connection{}); got != 2 {
		t.Fatalf("effectiveDB without override = %d, want 2", got)
	}
	over := 5
	if got := effectiveDB(inst, Connection{DB: &over}); got != 5 {
		t.Fatalf("effectiveDB with override = %d, want 5", got)
	}
}

// TestCredentialResolution covers the credential layering: connection-level
// username/password win; instance-level fields are a legacy fallback, and
// clientOptions decrypts whichever blob is effective.
func TestCredentialResolution(t *testing.T) {
	setupEnv(t)
	enc, err := secret.Encrypt(testMasterKey, []byte("legacypass"))
	if err != nil {
		t.Fatal(err)
	}
	inst := Instance{Host: "h", Port: 6379, Username: "legacy", Password: enc, DB: 3}

	if got := effectiveUsername(inst, Connection{}); got != "legacy" {
		t.Fatalf("username fallback = %q, want legacy", got)
	}
	if got := effectiveUsername(inst, Connection{Username: "alice"}); got != "alice" {
		t.Fatalf("username override = %q, want alice", got)
	}
	if got := effectivePassword(inst, Connection{}); got != enc {
		t.Fatal("password should fall back to the instance blob")
	}
	if got := effectivePassword(inst, Connection{Password: "enc:v1:other"}); got != "enc:v1:other" {
		t.Fatal("connection password should win")
	}

	cfg := &Config{
		Instances:   map[string]Instance{"i": inst},
		Connections: map[string]Connection{"c": {Instance: "i"}},
	}
	opts, err := clientOptions(cfg, cfg.Connections["c"])
	if err != nil {
		t.Fatal(err)
	}
	if opts.Username != "legacy" || opts.Password != "legacypass" || opts.DB != 3 {
		t.Fatalf("legacy fallback: opts = %+v", opts)
	}
}

func TestParseInfo(t *testing.T) {
	text := "# Server\r\nredis_version:8.0.0\r\nrun_id:abc\r\n\r\n# Clients\r\nconnected_clients:3\r\n"
	sections := parseInfo(text)
	server, ok := sections["server"].(map[string]any)
	if !ok || server["redis_version"] != "8.0.0" || server["run_id"] != "abc" {
		t.Fatalf("server section = %+v", sections["server"])
	}
	clients, ok := sections["clients"].(map[string]any)
	if !ok || clients["connected_clients"] != "3" {
		t.Fatalf("clients section = %+v", sections["clients"])
	}
	if _, ok := sections["default"]; ok {
		t.Fatalf("unexpected default section: %+v", sections)
	}
}

func TestMaskConfigValue(t *testing.T) {
	cases := []struct {
		field, value, want string
	}{
		{"requirepass", "s3cret", "***"},
		{"masterauth", "s3cret", "***"},
		{"requirepass", "", ""}, // empty stays empty: credential not set
		{"maxmemory", "1073741824", "1073741824"},
		{"tls-key-file", "/etc/redis/key.pem", "/etc/redis/key.pem"}, // path, not a secret
	}
	for _, c := range cases {
		if got := maskConfigValue(c.field, c.value); got != c.want {
			t.Errorf("maskConfigValue(%q, %q) = %q, want %q", c.field, c.value, got, c.want)
		}
	}
}

func TestConfigJSONShape(t *testing.T) {
	// The config model must serialize with camelCase field names.
	c := Config{
		Version:           1,
		Instances:         map[string]Instance{"local": {Host: "127.0.0.1", Port: 6379}},
		Connections:       map[string]Connection{"local": {Instance: "local", AllowDangerous: true}},
		DefaultConnection: "local",
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"defaultConnection"`, `"allowDangerous"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("config JSON missing %s: %s", want, s)
		}
	}
}

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{
	  "version": 1,
	  "instances": {
	    "local": {"host": "127.0.0.1", "port": 6379, "username": "", "password": "enc:v1:abc", "db": 0, "tls": false}
	  },
	  "connections": {
	    "local": {"instance": "local", "db": 0, "readonly": false, "allowDangerous": false, "timeout": "5s"}
	  },
	  "defaultConnection": "local"
	}`)
	if err := schema.Validate("redis.json", valid); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}

	missingHost := []byte(`{"version": 1, "instances": {"local": {"port": 6379}}}`)
	if err := schema.Validate("redis.json", missingHost); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("doc without host: err=%v", err)
	}

	badPort := []byte(`{"version": 1, "instances": {"local": {"host": "h", "port": 70000}}}`)
	if err := schema.Validate("redis.json", badPort); err == nil {
		t.Fatal("doc with out-of-range port should be rejected")
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
	if !strings.Contains(out, "redis") {
		t.Fatalf("connector ls should list redis: %q", out)
	}
}

func TestResolveSyntax(t *testing.T) {
	cases := []struct {
		name  string
		value string
		flag  string
		want  string
	}{
		{"auto detects a json object", `{"a":1}`, "", "json"},
		{"auto detects a json array", `[1,2]`, "auto", "json"},
		{"leading whitespace is fine", "  \n {\"a\":1}", "", "json"},
		{"brace but invalid json", `{not json}`, "", ""},
		{"plain string", "hello", "", ""},
		{"none disables", `{"a":1}`, "none", ""},
		{"explicit yaml", "a: 1", "yaml", "yaml"},
		{"explicit toml", "a = 1", "toml", "toml"},
		{"explicit json without sniffing", "not json at all", "json", "json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSyntax(tc.value, tc.flag)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("resolveSyntax(%q, %q) = %q, want %q", tc.value, tc.flag, got, tc.want)
			}
		})
	}
	if _, err := resolveSyntax("", "xml"); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("invalid --syntax should be MISSING_ARGUMENT, got %v", err)
	}
}

// TestHighlightFlagAlias verifies --highlight and its --hl alias both reach
// validation before dialing (invalid value fails without a server).
func TestHighlightFlagAlias(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")
	for _, flag := range []string{"--highlight", "--hl"} {
		_, err := runMuxcat(t, "redis", "get", "k", flag, "bad")
		if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
			t.Fatalf("%s bad: err=%v", flag, err)
		}
	}
}

func TestIsNoPerm(t *testing.T) {
	if !isNoPerm(errors.New("NOPERM User alice has no permissions to run the 'ping' command")) {
		t.Fatal("NOPERM prefix should be detected")
	}
	if isNoPerm(errors.New("WRONGPASS invalid username-password pair")) {
		t.Fatal("WRONGPASS is not NOPERM")
	}
	if isNoPerm(nil) {
		t.Fatal("nil is not NOPERM")
	}
}
