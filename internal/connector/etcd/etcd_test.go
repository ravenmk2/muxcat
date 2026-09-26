package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"

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
	args := append([]string{"etcd", "conn", "add", name, "--endpoints", "127.0.0.1:2379"}, extra...)
	out, err := runMuxcat(t, args...)
	if err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
	return out
}

func TestGuardWrite(t *testing.T) {
	cases := []struct {
		name     string
		conn     Connection
		op       string
		wantCode string // "" = allowed
	}{
		{"readonly rejects put", Connection{Readonly: true}, "put", output.CodeReadonlyViolation},
		{"readonly rejects del", Connection{Readonly: true}, "del", output.CodeReadonlyViolation},
		{"writable allows put", Connection{}, "put", ""},
		{"writable allows del", Connection{}, "del", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guardWrite(tc.conn, tc.op)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("guardWrite = %v, want allowed", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("guardWrite = nil, want %s", tc.wantCode)
			}
			if e := output.ToError(err); e.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s", e.Code, tc.wantCode)
			}
		})
	}

	if err := guardDangerous(Connection{}, "del --prefix"); err == nil ||
		output.ToError(err).Code != output.CodeUnsupportedOperation {
		t.Fatalf("del --prefix without allowDangerous: %v", err)
	}
	if err := guardDangerous(Connection{AllowDangerous: true}, "del --prefix"); err != nil {
		t.Fatalf("del --prefix with allowDangerous: %v", err)
	}
	if got := output.ExitCode(guardWrite(Connection{Readonly: true}, "put")); got != output.ExitExec {
		t.Fatalf("readonly violation exit = %d, want %d", got, output.ExitExec)
	}
	if got := output.ExitCode(guardDangerous(Connection{}, "del --prefix")); got != output.ExitUsage {
		t.Fatalf("dangerous block exit = %d, want %d", got, output.ExitUsage)
	}
}

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantExit int
	}{
		{"auth failed", rpctypes.ErrGRPCAuthFailed, output.CodeAuthFailed, output.ExitAuth},
		{"invalid auth token", rpctypes.ErrGRPCInvalidAuthToken, output.CodeAuthFailed, output.ExitAuth},
		{"permission denied", rpctypes.ErrGRPCPermissionDenied, output.CodeAuthFailed, output.ExitAuth},
		{"dial refused", errors.New("rpc error: code = Unavailable desc = connection error: desc = \"transport: Error while dialing: dial tcp 127.0.0.1:2379: connectex: No connection could be made because the target machine actively refused it.\""), output.CodeConnectFailed, output.ExitConnect},
		{"no such host", errors.New("dial tcp: lookup nope: no such host"), output.CodeConnectFailed, output.ExitConnect},
		{"deadline", context.DeadlineExceeded, output.CodeTimeout, output.ExitConnect},
		{"grpc deadline text", errors.New("rpc error: code = DeadlineExceeded desc = context deadline exceeded"), output.CodeTimeout, output.ExitConnect},
		{"compaction", rpctypes.ErrGRPCCompacted, output.CodeQueryError, output.ExitExec},
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

	out := addConn(t, "local", "--username", "root", "--password", "s3cret",
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
	if len(inst.Endpoints) != 1 || inst.Endpoints[0] != "127.0.0.1:2379" {
		t.Fatalf("instance = %+v", inst)
	}
	conn := cfg.Connections["local"]
	if conn.Instance != "local" || !conn.Readonly || conn.Timeout != "5s" || conn.AllowDangerous {
		t.Fatalf("connection = %+v", conn)
	}
	if conn.Username != "root" {
		t.Fatalf("username = %q, want root", conn.Username)
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
	out, err = runMuxcat(t, "etcd", "conn", "ls")
	if err != nil || !strings.Contains(out, "local") || !strings.Contains(out, "127.0.0.1:2379") {
		t.Fatalf("conn ls: err=%v out=%q", err, out)
	}
	if strings.Contains(out, "enc:v1:") || strings.Contains(out, "s3cret") {
		t.Fatalf("conn ls leaked the password: %q", out)
	}

	// conn show never echoes the password.
	out, err = runMuxcat(t, "etcd", "conn", "show", "local")
	if err != nil || !strings.Contains(out, "allowDangerous") {
		t.Fatalf("conn show: err=%v out=%q", err, out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "enc:v1:") {
		t.Fatalf("conn show leaked the password: %q", out)
	}

	// switching the default to a nonexistent connection should report CONN_NOT_FOUND
	if _, err := runMuxcat(t, "etcd", "conn", "default", "nope"); err == nil ||
		output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("conn default nope: err=%v", err)
	}

	if out, err := runMuxcat(t, "etcd", "conn", "rm", "local", "--yes"); err != nil {
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
	_, err := runMuxcat(t, "etcd", "conn", "add", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("add without --endpoints in non-TTY: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "conn", "add", "blank", "--endpoints", "   ")
	if err == nil {
		t.Fatal("add with blank --endpoints should fail")
	}
	_, err = runMuxcat(t, "etcd", "conn", "add", "noport", "--endpoints", "127.0.0.1")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with endpoint missing port: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "conn", "add", "badport", "--endpoints", "127.0.0.1:70000")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with out-of-range port: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "conn", "add", "bad", "--endpoints", "127.0.0.1:2379", "--timeout", "banana")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with invalid --timeout: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "conn", "add", "badtls", "--endpoints", "127.0.0.1:2379", "--cacert", "ca.pem")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with --cacert but no --tls: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "conn", "add", "badpair", "--endpoints", "127.0.0.1:2379", "--tls", "--cert", "c.pem")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("add with --cert but no --key: err=%v", err)
	}
}

func TestConnRmRequiresYesNonTTY(t *testing.T) {
	setupEnv(t)
	addConn(t, "local")
	_, err := runMuxcat(t, "etcd", "conn", "rm", "local")
	if err == nil || output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("rm without --yes in non-TTY: err=%v", err)
	}
}

func TestConnNotFound(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "etcd", "get", "k")
	if err == nil || output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("get without any connection: err=%v", err)
	}
}

// TestGuardBlocksWithoutServer verifies interception happens before any
// network access: rejections fire against a connection pointing at a
// closed port.
func TestGuardBlocksWithoutServer(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--endpoints", "127.0.0.1:1", "--readonly", "--set-default")
	_, err := runMuxcat(t, "etcd", "put", "k", "v")
	if err == nil || output.ToError(err).Code != output.CodeReadonlyViolation {
		t.Fatalf("put on readonly conn: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "del", "k")
	if err == nil || output.ToError(err).Code != output.CodeReadonlyViolation {
		t.Fatalf("del on readonly conn: err=%v", err)
	}
	// readonly violation wins over the dangerous check
	_, err = runMuxcat(t, "etcd", "del", "/a/", "--prefix")
	if err == nil || output.ToError(err).Code != output.CodeReadonlyViolation {
		t.Fatalf("del --prefix on readonly conn: err=%v", err)
	}

	addConn(t, "rw", "--endpoints", "127.0.0.1:1")
	_, err = runMuxcat(t, "etcd", "del", "/a/", "--prefix", "-c", "rw")
	if err == nil || output.ToError(err).Code != output.CodeUnsupportedOperation {
		t.Fatalf("del --prefix without allowDangerous: err=%v", err)
	}
	// with allowDangerous the guard passes and the dial fails instead
	addConn(t, "adm", "--endpoints", "127.0.0.1:1", "--allow-dangerous", "--timeout", "2s")
	_, err = runMuxcat(t, "etcd", "del", "/a/", "--prefix", "-c", "adm")
	if err == nil {
		t.Fatal("del --prefix against a closed port should fail at dial")
	}
	if e := output.ToError(err); e.Code == output.CodeUnsupportedOperation || e.Code == output.CodeReadonlyViolation {
		t.Fatalf("guard should not fire for allowDangerous conn, got %s", e.Code)
	}
}

// TestUnreachableClassified checks that get/watch against a closed port
// surface a connect-class error (CONNECT_FAILED or TIMEOUT, both exit 3).
func TestUnreachableClassified(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--endpoints", "127.0.0.1:1", "--timeout", "2s", "--set-default")
	for _, args := range [][]string{
		{"etcd", "get", "k"},
		{"etcd", "get", "/a/", "--prefix"},
		{"etcd", "put", "k", "v"},
		{"etcd", "del", "k"},
		{"etcd", "watch", "k", "--timeout", "2s"},
		{"etcd", "conn", "test", "down"},
	} {
		_, err := runMuxcat(t, args...)
		if err == nil {
			t.Fatalf("%v: should fail against a closed port", args)
		}
		e := output.ToError(err)
		if e.Code != output.CodeConnectFailed && e.Code != output.CodeTimeout {
			t.Fatalf("%v: code = %s, want CONNECT_FAILED or TIMEOUT (msg: %s)", args, e.Code, e.Message)
		}
		if output.ExitCode(err) != output.ExitConnect {
			t.Fatalf("%v: exit = %d, want %d", args, output.ExitCode(err), output.ExitConnect)
		}
		if strings.Contains(e.Message, "s3cret") || strings.Contains(e.Message, "enc:v1:") {
			t.Fatalf("%v: error leaked credential material: %s", args, e.Message)
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

func TestParseEndpoints(t *testing.T) {
	eps, err := parseEndpoints("127.0.0.1:2379, etcd.example.com:2379")
	if err != nil || len(eps) != 2 || eps[1] != "etcd.example.com:2379" {
		t.Fatalf("valid endpoints: eps=%v err=%v", eps, err)
	}
	for _, bad := range []string{"", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:abc", ":2379"} {
		if _, err := parseEndpoints(bad); err == nil {
			t.Fatalf("endpoint %q should be rejected", bad)
		}
	}
}

// TestClientConfigCredentials covers credential decryption and TLS config
// validation (no network access).
func TestClientConfigCredentials(t *testing.T) {
	setupEnv(t)
	enc, err := secret.Encrypt(testMasterKey, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Instances: map[string]Instance{"i": {Endpoints: []string{"127.0.0.1:2379"}}},
		Connections: map[string]Connection{
			"c": {Instance: "i", Username: "root", Password: enc},
		},
	}
	cc, err := clientConfig(cfg, cfg.Connections["c"], time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cc.Username != "root" || cc.Password != "s3cret" {
		t.Fatalf("credentials not resolved: %+v", cc)
	}
	if cc.TLS != nil {
		t.Fatal("TLS should be off")
	}

	cfg.Instances["i"] = Instance{Endpoints: []string{"127.0.0.1:2379"}, TLS: true, Cert: "c.pem"}
	if _, err := clientConfig(cfg, cfg.Connections["c"], time.Second); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("cert without key: err=%v", err)
	}

	cfg.Instances["i"] = Instance{Endpoints: []string{"127.0.0.1:2379"}, TLS: true, CACert: "nope.pem"}
	if _, err := clientConfig(cfg, cfg.Connections["c"], time.Second); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("unreadable CA: err=%v", err)
	}
}

func TestConfigJSONShape(t *testing.T) {
	// The config model must serialize with camelCase field names.
	c := Config{
		Version:           1,
		Instances:         map[string]Instance{"local": {Endpoints: []string{"127.0.0.1:2379"}}},
		Connections:       map[string]Connection{"local": {Instance: "local", AllowDangerous: true}},
		DefaultConnection: "local",
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"defaultConnection"`, `"allowDangerous"`, `"endpoints"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("config JSON missing %s: %s", want, s)
		}
	}
}

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{
	  "version": 1,
	  "instances": {
	    "local": {"endpoints": ["127.0.0.1:2379"], "tls": true, "cacert": "ca.pem", "cert": "c.pem", "key": "k.pem"}
	  },
	  "connections": {
	    "local": {"instance": "local", "username": "root", "password": "enc:v1:abc", "readonly": false, "allowDangerous": false, "timeout": "5s"}
	  },
	  "defaultConnection": "local"
	}`)
	if err := schema.Validate("etcd.json", valid); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}

	missingEndpoints := []byte(`{"version": 1, "instances": {"local": {"tls": false}}}`)
	if err := schema.Validate("etcd.json", missingEndpoints); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("doc without endpoints: err=%v", err)
	}

	emptyEndpoints := []byte(`{"version": 1, "instances": {"local": {"endpoints": []}}}`)
	if err := schema.Validate("etcd.json", emptyEndpoints); err == nil {
		t.Fatal("doc with an empty endpoint list should be rejected")
	}

	connNoInstance := []byte(`{"version": 1, "connections": {"local": {"readonly": true}}}`)
	if err := schema.Validate("etcd.json", connNoInstance); err == nil {
		t.Fatal("connection without instance should be rejected")
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
	if !strings.Contains(out, "etcd") {
		t.Fatalf("connector ls should list etcd: %q", out)
	}
}

// TestWatchFlagValidation covers watch's flag validation before dialing.
func TestWatchFlagValidation(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--endpoints", "127.0.0.1:1", "--timeout", "2s", "--set-default")

	_, err := runMuxcat(t, "etcd", "watch", "k", "--max-events", "0")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("--max-events 0: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "watch", "k", "--rev", "-1")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("--rev -1: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "get", "k", "--keys-only")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("--keys-only without --prefix: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "get", "/a/", "--prefix", "--bare")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("--bare with --prefix: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "get", "k", "--rev", "-1")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("get --rev -1: err=%v", err)
	}
	_, err = runMuxcat(t, "etcd", "put", "k", "v", "--lease-id", "-1")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("put --lease-id -1: err=%v", err)
	}
}

// TestAlarmDisarmGuardedWithoutServer verifies alarm disarm counts as a
// write: a readonly connection is intercepted before any network access.
func TestAlarmDisarmGuardedWithoutServer(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--endpoints", "127.0.0.1:1", "--readonly", "--set-default")
	_, err := runMuxcat(t, "etcd", "alarm", "disarm")
	if err == nil || output.ToError(err).Code != output.CodeReadonlyViolation {
		t.Fatalf("alarm disarm on readonly conn: err=%v", err)
	}
	// allowDangerous is not required: a writable connection passes the guard
	// and fails at dial instead.
	addConn(t, "rw", "--endpoints", "127.0.0.1:1", "--timeout", "2s")
	_, err = runMuxcat(t, "etcd", "alarm", "disarm", "-c", "rw")
	if err == nil {
		t.Fatal("alarm disarm against a closed port should fail at dial")
	}
	e := output.ToError(err)
	if e.Code == output.CodeReadonlyViolation || e.Code == output.CodeUnsupportedOperation {
		t.Fatalf("guard should not fire on a writable conn, got %s", e.Code)
	}
	if e.Code != output.CodeConnectFailed && e.Code != output.CodeTimeout {
		t.Fatalf("code = %s, want CONNECT_FAILED or TIMEOUT (msg: %s)", e.Code, e.Message)
	}
}

// TestClusterCommandsUnreachable checks the cluster-inspection commands
// against a closed port: endpoint status/health fail only when every
// endpoint fails; member list / alarm list surface classifyErr directly.
func TestClusterCommandsUnreachable(t *testing.T) {
	setupEnv(t)
	addConn(t, "down", "--endpoints", "127.0.0.1:1", "--timeout", "2s", "--set-default")
	for _, args := range [][]string{
		{"etcd", "endpoint", "status"},
		{"etcd", "endpoint", "health"},
		{"etcd", "member", "list"},
		{"etcd", "alarm", "list"},
		{"etcd", "alarm", "disarm"},
	} {
		_, err := runMuxcat(t, args...)
		if err == nil {
			t.Fatalf("%v: should fail against a closed port", args)
		}
		e := output.ToError(err)
		if e.Code != output.CodeConnectFailed && e.Code != output.CodeTimeout {
			t.Fatalf("%v: code = %s, want CONNECT_FAILED or TIMEOUT (msg: %s)", args, e.Code, e.Message)
		}
		if output.ExitCode(err) != output.ExitConnect {
			t.Fatalf("%v: exit = %d, want %d", args, output.ExitCode(err), output.ExitConnect)
		}
	}
}
