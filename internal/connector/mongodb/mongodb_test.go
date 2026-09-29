package mongodb

import (
	"bytes"
	"context"
	"errors"
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

// stubStdinTTY replaces the stdin TTY detection for a test.
func stubStdinTTY(t *testing.T, tty bool) {
	t.Helper()
	orig := stdinIsTTY
	stdinIsTTY = func() bool { return tty }
	t.Cleanup(func() { stdinIsTTY = orig })
}

func addConn(t *testing.T, name string, extra ...string) string {
	t.Helper()
	args := append([]string{"mongodb", "conn", "add", name, "--host", "127.0.0.1"}, extra...)
	out, err := runMuxcat(t, args...)
	if err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
	return out
}

func TestConnLifecycle(t *testing.T) {
	setupEnv(t)

	out := addConn(t, "local", "--port", "27018", "--database", "app", "--username", "alice",
		"--password", "s3cret", "--timeout", "5s", "--set-default")
	if !strings.Contains(out, "Warning: --password") {
		t.Fatalf("plaintext password flag should warn on stderr: %q", out)
	}
	if !strings.Contains(out, "added connection local (127.0.0.1:27018, db app)") {
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
	if len(inst.Hosts) != 1 || inst.Hosts[0] != "127.0.0.1:27018" {
		t.Fatalf("instance = %+v", inst)
	}
	conn := cfg.Connections["local"]
	if conn.Instance != "local" || conn.Username != "alice" || conn.Database != "app" ||
		conn.Timeout != "5s" {
		t.Fatalf("connection = %+v", conn)
	}
	// authSource defaults to admin when a username is set.
	if conn.AuthSource != "admin" {
		t.Fatalf("authSource = %q, want admin", conn.AuthSource)
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

	// The config on disk holds the blob, never the plaintext.
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "enc:v1:") || strings.Contains(string(raw), "s3cret") {
		t.Fatalf("mongodb.json on disk leaks the plaintext password: %s", raw)
	}

	// conn ls shows the connection without the password.
	out, err = runMuxcat(t, "mongodb", "conn", "ls")
	if err != nil || !strings.Contains(out, "local") || !strings.Contains(out, "27018") ||
		!strings.Contains(out, "alice") || !strings.Contains(out, "app") || !strings.Contains(out, "admin") {
		t.Fatalf("conn ls: err=%v out=%q", err, out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "enc:v1:") {
		t.Fatalf("conn ls leaked the password: %q", out)
	}

	// conn show never echoes the password.
	out, err = runMuxcat(t, "mongodb", "conn", "show", "local")
	if err != nil || !strings.Contains(out, "authSource") || !strings.Contains(out, "database") {
		t.Fatalf("conn show: err=%v out=%q", err, out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "enc:v1:") {
		t.Fatalf("conn show leaked the password: %q", out)
	}

	// duplicate add is rejected
	if _, err := runMuxcat(t, "mongodb", "conn", "add", "local", "--host", "other"); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("duplicate add: err=%v", err)
	}

	// a second connection becomes the fallback default after rm
	addConn(t, "other")
	if out, err := runMuxcat(t, "mongodb", "conn", "rm", "local", "--yes"); err != nil {
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
	if cfg.DefaultConnection != "other" {
		t.Fatalf("defaultConnection should be reselected to other, got %q", cfg.DefaultConnection)
	}
}

func TestConnAddDefaults(t *testing.T) {
	setupEnv(t)
	addConn(t, "local")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	inst := cfg.Instances["local"]
	if len(inst.Hosts) != 1 || inst.Hosts[0] != "127.0.0.1:27017" {
		t.Fatalf("default port 27017: instance = %+v", inst)
	}
	conn := cfg.Connections["local"]
	if conn.AuthSource != "" {
		t.Fatalf("authSource without username should stay empty, got %q", conn.AuthSource)
	}
	// first connection becomes the default automatically
	if cfg.DefaultConnection != "local" {
		t.Fatalf("defaultConnection = %q, want local", cfg.DefaultConnection)
	}

	// set-default switches the default
	addConn(t, "second", "--set-default")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConnection != "second" {
		t.Fatalf("defaultConnection = %q, want second", cfg.DefaultConnection)
	}
}

func TestConnAddURI(t *testing.T) {
	setupEnv(t)
	out, err := runMuxcat(t, "mongodb", "conn", "add", "rs",
		"--uri", "mongodb://alice:s3cret@db.example.com:27018/shop?authSource=users&tls=true&replicaSet=rs0",
		"--set-default")
	if err != nil {
		t.Fatalf("conn add --uri: %v\n%s", err, out)
	}
	if !strings.Contains(out, "the URI contains a plaintext password") {
		t.Fatalf("URI password should warn on stderr: %q", out)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	inst := cfg.Instances["rs"]
	if len(inst.Hosts) != 1 || inst.Hosts[0] != "db.example.com:27018" || !inst.TLS || inst.ReplicaSet != "rs0" {
		t.Fatalf("instance = %+v", inst)
	}
	conn := cfg.Connections["rs"]
	if conn.Username != "alice" || conn.AuthSource != "users" || conn.Database != "shop" {
		t.Fatalf("connection = %+v", conn)
	}
	if !strings.HasPrefix(conn.Password, secret.Prefix) || strings.Contains(conn.Password, "s3cret") {
		t.Fatalf("password blob = %q", conn.Password)
	}
	if strings.Contains(out, "s3cret") {
		t.Fatalf("output leaked the URI password: %q", out)
	}

	// explicit flags override URI values
	if _, err := runMuxcat(t, "mongodb", "conn", "add", "ovr",
		"--uri", "mongodb://alice@db.example.com:27018/shop",
		"--host", "10.0.0.2", "--port", "27019", "--database", "other"); err != nil {
		t.Fatalf("conn add override: %v", err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Instances["ovr"].Hosts[0]; got != "10.0.0.2:27019" {
		t.Fatalf("override hosts = %q", got)
	}
	if cfg.Connections["ovr"].Database != "other" {
		t.Fatalf("override database = %q", cfg.Connections["ovr"].Database)
	}
}

func TestConnAddValidation(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "mongodb", "conn", "add", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("add without --host in non-TTY: err=%v", err)
	}
	_, err = runMuxcat(t, "mongodb", "conn", "add", "bad", "--host", "h", "--port", "70000")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with invalid --port: err=%v", err)
	}
	_, err = runMuxcat(t, "mongodb", "conn", "add", "bad2", "--host", "h", "--timeout", "banana")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with invalid --timeout: err=%v", err)
	}
	_, err = runMuxcat(t, "mongodb", "conn", "add", "srv", "--uri", "mongodb+srv://db.example.com/app")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with mongodb+srv URI: err=%v", err)
	}
	_, err = runMuxcat(t, "mongodb", "conn", "add", "multi",
		"--uri", "mongodb://10.0.0.1:27017,10.0.0.2:27017/app")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with multi-host URI: err=%v", err)
	}
}

func TestConnRmRequiresYesNonTTY(t *testing.T) {
	setupEnv(t)
	addConn(t, "local")
	_, err := runMuxcat(t, "mongodb", "conn", "rm", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("rm without --yes in non-TTY: err=%v", err)
	}
}

func TestConnNotFound(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "mongodb", "conn", "test", "nope")
	if err == nil || output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("conn test on a nonexistent name: err=%v", err)
	}
}

func TestParseURI(t *testing.T) {
	parts, err := parseURI("mongodb://alice:s3cret@db.example.com:27018/shop?authSource=users&tls=true&replicaSet=rs0")
	if err != nil {
		t.Fatal(err)
	}
	if parts.host != "db.example.com" || !parts.portSet || parts.port != 27018 ||
		parts.username != "alice" || parts.password != "s3cret" || !parts.passwordSet ||
		parts.authSource != "users" || parts.database != "shop" ||
		!parts.tls || !parts.tlsSet || parts.replicaSet != "rs0" {
		t.Fatalf("full URI: %+v", parts)
	}

	parts, err = parseURI("mongodb://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if parts.host != "127.0.0.1" || parts.portSet {
		t.Fatalf("bare host: %+v", parts)
	}

	// srv scheme is rejected
	if _, err := parseURI("mongodb+srv://db.example.com/app"); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("srv scheme: err=%v", err)
	}

	// multiple hosts are rejected
	if _, err := parseURI("mongodb://10.0.0.1:27017,10.0.0.2:27017/app"); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("multiple hosts: err=%v", err)
	}

	// invalid URIs never echo the password
	_, err = parseURI("mongodb://alice:topsecret@db.example.com:notaport/shop")
	if err == nil {
		t.Fatal("invalid port should be rejected")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("error echoes the password: %v", err)
	}
	if e := output.ToError(err); e.Hint != "" && strings.Contains(e.Hint, "topsecret") {
		t.Fatalf("hint echoes the password: %v", e.Hint)
	}
}

func TestNormalizeHost(t *testing.T) {
	if got := normalizeHost("db.example.com"); got != "db.example.com:27017" {
		t.Fatalf("normalizeHost = %q", got)
	}
	if got := normalizeHost("db.example.com:27018"); got != "db.example.com:27018" {
		t.Fatalf("normalizeHost with port = %q", got)
	}
	if got := addr(Instance{Hosts: []string{"127.0.0.1"}}); got != "127.0.0.1:27017" {
		t.Fatalf("addr = %q", got)
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

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"unauthorized", errors.New("connection() error occurred during connection handshake: auth error: sasl conversation error: Authentication failed."), output.CodeAuthFailed},
		{"dial refused", errors.New("server selection error: context deadline exceeded, current topology: { Servers: [{ Addr: 127.0.0.1:27017, Last error: dial tcp 127.0.0.1:27017: connectex: No connection could be made because the target machine actively refused it. }] }"), output.CodeConnectFailed},
		{"no such host", errors.New("server selection error: dial tcp: lookup nope: no such host"), output.CodeConnectFailed},
		{"deadline", context.DeadlineExceeded, output.CodeTimeout},
		{"other", errors.New("something broke"), output.CodeQueryError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classifyErr(tc.err, "op failed")
			if e.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s (msg: %s)", e.Code, tc.wantCode, e.Message)
			}
			if !strings.HasPrefix(e.Message, "op failed: ") {
				t.Fatalf("message lacks prefix: %q", e.Message)
			}
		})
	}
}

// TestConnectFailedUnreachable verifies conn test against a closed port
// classifies as CONNECT_FAILED.
func TestConnectFailedUnreachable(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--port", "1", "--timeout", "2s", "--set-default")
	_, err := runMuxcat(t, "mongodb", "conn", "test", "down")
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

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{
	  "version": 1,
	  "instances": {
	    "local": {"hosts": ["127.0.0.1:27017"], "tls": true, "replicaSet": "rs0"}
	  },
	  "connections": {
	    "local": {"instance": "local", "username": "root", "password": "enc:v1:abc", "authSource": "admin", "database": "app", "timeout": "5s"}
	  },
	  "defaultConnection": "local"
	}`)
	if err := schema.Validate("mongodb.json", valid); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}

	missingHosts := []byte(`{"version": 1, "instances": {"local": {"tls": true}}}`)
	if err := schema.Validate("mongodb.json", missingHosts); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("doc without hosts: err=%v", err)
	}

	emptyHosts := []byte(`{"version": 1, "instances": {"local": {"hosts": []}}}`)
	if err := schema.Validate("mongodb.json", emptyHosts); err == nil {
		t.Fatal("doc with an empty hosts array should be rejected")
	}

	extra := []byte(`{"version": 1, "connections": {"local": {"instance": "local", "db": 0}}}`)
	if err := schema.Validate("mongodb.json", extra); err == nil {
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
	if !strings.Contains(out, "mongodb") {
		t.Fatalf("connector ls should list mongodb: %q", out)
	}
}
