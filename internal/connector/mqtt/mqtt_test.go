package mqtt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/paho"
	packetsv3 "github.com/eclipse/paho.mqtt.golang/packets"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
	"github.com/ravenmk2/muxcat/schema"
)

// runMuxcat executes through the full root command (including persistent
// flags and the registry mounts) and returns stdout and the error.
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

// runJSON runs a command with --json and decodes the envelope.
func runJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out, err := runMuxcat(t, append(args, "--json")...)
	if err != nil {
		t.Fatalf("%v failed: %v\n%s", args, err, out)
	}
	return decodeEnvelope(t, out)
}

// decodeEnvelope unmarshals a JSON envelope.
func decodeEnvelope(t *testing.T, out string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	return env
}

// runMuxcatSE is runMuxcat with separate stdout/stderr buffers, for
// asserting on warning output.
func runMuxcatSE(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := cli.NewRoot("test")
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(outBuf)
	root.SetErr(errBuf)
	root.SetArgs(args)
	err := root.Execute()
	return outBuf.String(), errBuf.String(), err
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

// addTestConn runs conn add (the plaintext-password warning goes to
// stderr, which must not pollute JSON assertions).
func addTestConn(t *testing.T, args ...string) {
	t.Helper()
	if _, _, err := runMuxcatSE(t, args...); err != nil {
		t.Fatalf("%v failed: %v", args, err)
	}
}

const testPassword = "s3cr3t-pw"

// readConfigFile returns the raw mqtt.json content.
func readConfigFile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatalf("cannot read %s: %v", FileName, err)
	}
	return string(raw)
}

// startBroker starts an in-process mochi-mqtt broker (anonymous allowed)
// on a random port and returns its address.
func startBroker(t *testing.T) string {
	t.Helper()
	return startBrokerWithHook(t, nil)
}

// startBrokerWithHook starts a broker, optionally with an auth hook
// config; nil means anonymous allow-all.
func startBrokerWithHook(t *testing.T, ledger *auth.Ledger) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	server := mochi.New(&mochi.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if ledger != nil {
		if err := server.AddHook(new(auth.Hook), &auth.Options{Ledger: ledger}); err != nil {
			t.Fatalf("add auth hook: %v", err)
		}
	} else if err := server.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatalf("add allow hook: %v", err)
	}
	if err := server.AddListener(listeners.NewTCP(listeners.Config{ID: "t1", Address: addr})); err != nil {
		t.Fatalf("add listener: %v", err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	return addr
}

func TestConnAddSerializesEncryptedPassword(t *testing.T) {
	setupEnv(t)
	addTestConn(t, "mqtt", "conn", "add", "local",
		"--url", "mqtt://127.0.0.1:1883", "--username", "admin",
		"--password", testPassword, "--set-default")
	raw := readConfigFile(t)
	if !strings.Contains(raw, `"password": "enc:v1:`) {
		t.Errorf("password must be stored as an enc:v1: blob:\n%s", raw)
	}
	if strings.Contains(raw, testPassword) {
		t.Errorf("plaintext password leaked into %s:\n%s", FileName, raw)
	}
	// Protocol version defaults to 3 and is omitted from the file.
	if strings.Contains(raw, "protocolVersion") {
		t.Errorf("default protocolVersion should be omitted:\n%s", raw)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConnection != "local" {
		t.Errorf("defaultConnection = %q, want local", cfg.DefaultConnection)
	}
	if got := cfg.Instances["local"].protocolVersion(); got != ProtocolV3 {
		t.Errorf("protocolVersion = %d, want %d", got, ProtocolV3)
	}
	conn := cfg.Connections["local"]
	if conn.Username != "admin" {
		t.Errorf("unexpected connection: %+v", conn)
	}
	plain, err := decryptPassword(conn)
	if err != nil {
		t.Fatal(err)
	}
	if plain != testPassword {
		t.Errorf("decrypted password = %q", plain)
	}
}

func TestConnAddProtocolVersion5(t *testing.T) {
	setupEnv(t)
	runJSON(t, "mqtt", "conn", "add", "v5",
		"--url", "mqtt://127.0.0.1:1883", "--protocol-version", "5",
		"--set-default")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Instances["v5"].ProtocolVersion; got != ProtocolV5 {
		t.Errorf("protocolVersion = %d, want %d", got, ProtocolV5)
	}
}

func TestConnAddNormalizesSchemeAlias(t *testing.T) {
	setupEnv(t)
	runJSON(t, "mqtt", "conn", "add", "a", "--url", "tcp://h:1883", "--set-default")
	runJSON(t, "mqtt", "conn", "add", "b", "--url", "tls://h:8883")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Instances["a"].URL; got != "mqtt://h:1883" {
		t.Errorf("tcp:// should normalize to mqtt://, got %q", got)
	}
	if got := cfg.Instances["b"].URL; got != "mqtts://h:8883" {
		t.Errorf("tls:// should normalize to mqtts://, got %q", got)
	}
}

func TestConnAddPasswordFlagWarns(t *testing.T) {
	setupEnv(t)
	_, stderr, err := runMuxcatSE(t, "mqtt", "conn", "add", "local",
		"--url", "mqtt://127.0.0.1:1883", "--password", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "plaintext") {
		t.Errorf("expected a plaintext-credential warning on stderr, got %q", stderr)
	}
	if strings.Contains(stderr, testPassword) {
		t.Errorf("stderr must not echo the password: %q", stderr)
	}
}

func TestConnLsAndShowNeverEchoPassword(t *testing.T) {
	setupEnv(t)
	addTestConn(t, "mqtt", "conn", "add", "local",
		"--url", "mqtt://127.0.0.1:1883", "--username", "admin",
		"--password", testPassword, "--set-default")
	for _, args := range [][]string{
		{"mqtt", "conn", "ls"},
		{"mqtt", "conn", "ls", "--json"},
		{"mqtt", "conn", "show", "local"},
		{"mqtt", "conn", "show", "local", "--json"},
	} {
		out, err := runMuxcat(t, args...)
		if err != nil {
			t.Fatalf("%v failed: %v", args, err)
		}
		if strings.Contains(out, testPassword) {
			t.Errorf("%v echoes the plaintext password:\n%s", args, out)
		}
		if strings.Contains(out, "enc:v1:") {
			t.Errorf("%v echoes the encrypted blob:\n%s", args, out)
		}
	}
	// conn ls carries the unified columns.
	out, _ := runMuxcat(t, "mqtt", "conn", "ls")
	for _, col := range []string{"name", "url", "protocol", "username", "readonly", "default"} {
		if !strings.Contains(out, col) {
			t.Errorf("conn ls misses column %q:\n%s", col, out)
		}
	}
}

func TestConnAddRejectsCredentialedURL(t *testing.T) {
	setupEnv(t)
	out, err := runMuxcat(t, "mqtt", "conn", "add", "bad",
		"--url", "mqtt://admin:topsecret@127.0.0.1:1883")
	if err == nil {
		t.Fatalf("expected an error, got %s", out)
	}
	e := output.ToError(err)
	if e.Code != output.CodeConfigInvalid {
		t.Errorf("code = %s, want CONFIG_INVALID (%v)", e.Code, e)
	}
	if strings.Contains(e.Message+e.Hint, "topsecret") {
		t.Errorf("error must not echo the embedded credential: %v", e)
	}
	// An invalid scheme combined with embedded credentials must also not
	// echo the raw URL back.
	out, err = runMuxcat(t, "mqtt", "conn", "add", "bad2",
		"--url", "http://admin:topsecret@127.0.0.1:8083")
	if err == nil {
		t.Fatalf("expected an error, got %s", out)
	}
	e = output.ToError(err)
	if e.Code != output.CodeConfigInvalid {
		t.Errorf("code = %s, want CONFIG_INVALID (%v)", e.Code, e)
	}
	if strings.Contains(e.Message+e.Hint, "topsecret") {
		t.Errorf("error must not echo the embedded credential: %v", e)
	}
}

func TestConnAddValidation(t *testing.T) {
	setupEnv(t)
	cases := []struct {
		name string
		args []string
		code string
	}{
		{"bad protocol version", []string{"mqtt", "conn", "add", "x", "--url", "mqtt://h:1883", "--protocol-version", "7"}, output.CodeConfigInvalid},
		{"http scheme", []string{"mqtt", "conn", "add", "x", "--url", "http://h:8083"}, output.CodeConfigInvalid},
		{"skip-verify on plaintext", []string{"mqtt", "conn", "add", "x", "--url", "mqtt://h:1883", "--tls-skip-verify"}, output.CodeConfigInvalid},
		{"skip-verify on ws", []string{"mqtt", "conn", "add", "x", "--url", "ws://h:8083/mqtt", "--tls-skip-verify"}, output.CodeConfigInvalid},
		{"bad timeout", []string{"mqtt", "conn", "add", "x", "--url", "mqtt://h:1883", "--timeout", "never"}, output.CodeConfigInvalid},
		{"missing url non-interactive", []string{"mqtt", "conn", "add", "x"}, output.CodeMissingArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runMuxcat(t, tc.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := output.ToError(err).Code; got != tc.code {
				t.Errorf("code = %s, want %s (%v)", got, tc.code, err)
			}
		})
	}
	// The protocol version error lists the valid values.
	_, err := runMuxcat(t, "mqtt", "conn", "add", "x", "--url", "mqtt://h:1883", "--protocol-version", "7")
	e := output.ToError(err)
	if !strings.Contains(e.Hint, "3 | 5") {
		t.Errorf("protocolVersion hint should list valid values: %q", e.Hint)
	}
}

func TestConnRmAndDefault(t *testing.T) {
	setupEnv(t)
	runJSON(t, "mqtt", "conn", "add", "a", "--url", "mqtt://h1:1883", "--set-default")
	runJSON(t, "mqtt", "conn", "add", "b", "--url", "mqtt://h2:1883")
	runJSON(t, "mqtt", "conn", "default", "b")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConnection != "b" {
		t.Fatalf("defaultConnection = %q, want b", cfg.DefaultConnection)
	}
	runJSON(t, "mqtt", "conn", "rm", "a", "--yes")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Connections["a"]; ok {
		t.Error("connection a should be removed")
	}
	if _, ok := cfg.Instances["a"]; ok {
		t.Error("unreferenced instance a should be removed too")
	}
	// Removing a missing connection reports CONN_NOT_FOUND.
	if _, err := runMuxcat(t, "mqtt", "conn", "rm", "ghost", "--yes"); err == nil ||
		output.ToError(err).Code != output.CodeConnNotFound {
		t.Errorf("expected CONN_NOT_FOUND, got %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"mqtt://127.0.0.1:1883", "mqtt://127.0.0.1:1883", false},
		{"mqtt://broker", "mqtt://broker:1883", false},
		{"mqtts://broker.example.com/", "mqtts://broker.example.com:8883", false},
		{"tcp://h:1883", "mqtt://h:1883", false},
		{"ssl://h:8883", "mqtts://h:8883", false},
		{"tls://h:8883", "mqtts://h:8883", false},
		{"ws://h:8083/mqtt", "ws://h:8083/mqtt", false},
		{"ws://h:8083/mqtt/", "ws://h:8083/mqtt", false},
		{"wss://h:443/mqtt", "wss://h:443/mqtt", false},
		{" mqtt://h:1883 ", "mqtt://h:1883", false},
		{"http://h:8083", "", true},
		{"mqtt://user:pw@h:1883", "", true},
		{"not-a-url", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeURL(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("normalizeURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestSanitizeErr(t *testing.T) {
	err := errors.New("dial mqtt://admin:s3cr3t-pw@broker:1883: refused")
	got := sanitizeErr(err, testPassword)
	if strings.Contains(got, testPassword) {
		t.Errorf("password not scrubbed: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("expected a redaction marker: %q", got)
	}
	if got := sanitizeErr(errors.New("plain text"), ""); got != "plain text" {
		t.Errorf("empty password must leave the text alone: %q", got)
	}
}

func TestClassifyDial(t *testing.T) {
	// TCP refused: CONNECT_FAILED, never carrying the password.
	err := classifyDial(errors.New("dial tcp 10.0.0.1:1883: connect: connection refused"), testPassword)
	e := output.ToError(err)
	if e.Code != output.CodeConnectFailed {
		t.Errorf("code = %s, want CONNECT_FAILED", e.Code)
	}
	if strings.Contains(e.Message+e.Hint, testPassword) {
		t.Errorf("error must not leak the password: %v", e)
	}
	// Timeout: TIMEOUT.
	err = classifyDial(errors.New("dial tcp 10.0.0.1:1883: i/o timeout"), "")
	if output.ToError(err).Code != output.CodeTimeout {
		t.Errorf("code = %s, want TIMEOUT", output.ToError(err).Code)
	}
}

func TestClassifyDialV3(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"bad credentials", packetsv3.ErrorRefusedBadUsernameOrPassword, output.CodeAuthFailed},
		{"not authorized", packetsv3.ErrorRefusedNotAuthorised, output.CodeAuthFailed},
		{"bad protocol", packetsv3.ErrorRefusedBadProtocolVersion, output.CodeConnectFailed},
		{"id rejected", packetsv3.ErrorRefusedIDRejected, output.CodeConnectFailed},
		{"server unavailable", packetsv3.ErrorRefusedServerUnavailable, output.CodeConnectFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyDialV3(tc.err, testPassword)
			if got := output.ToError(err).Code; got != tc.code {
				t.Errorf("code = %s, want %s (%v)", got, tc.code, err)
			}
			if strings.Contains(output.ToError(err).Message, testPassword) {
				t.Errorf("error must not leak the password: %v", err)
			}
		})
	}
}

func TestClassifyDialV5(t *testing.T) {
	// A connack refusal is classified by its reason code.
	err := classifyDialV5(errors.New("failed to connect to server: bad user name or password"),
		&paho.Connack{ReasonCode: 0x86}, testPassword)
	if output.ToError(err).Code != output.CodeAuthFailed {
		t.Errorf("0x86: code = %s, want AUTH_FAILED", output.ToError(err).Code)
	}
	err = classifyDialV5(errors.New("failed to connect to server: not authorized"),
		&paho.Connack{ReasonCode: 0x87}, testPassword)
	if output.ToError(err).Code != output.CodeAuthFailed {
		t.Errorf("0x87: code = %s, want AUTH_FAILED", output.ToError(err).Code)
	}
	if code := output.ToError(classifyConnackV5(0x84)).Code; code != output.CodeConnectFailed {
		t.Errorf("0x84: code = %s, want CONNECT_FAILED", code)
	}
	// Without a connack, it falls back to the transport classifier.
	err = classifyDialV5(errors.New("dial tcp: connection refused"), nil, testPassword)
	if output.ToError(err).Code != output.CodeConnectFailed {
		t.Errorf("net failure: code = %s, want CONNECT_FAILED", output.ToError(err).Code)
	}
}

func TestReadonlyViolationBeforeDial(t *testing.T) {
	setupEnv(t)
	// The instance is unreachable; a readonly refusal must fire before any
	// dialing, so the outcome is READONLY_VIOLATION, not CONNECT_FAILED.
	runJSON(t, "mqtt", "conn", "add", "ro",
		"--url", "mqtt://127.0.0.1:1", "--readonly", "--set-default")
	_, err := runMuxcat(t, "mqtt", "pub", "test/topic", "--payload", "hi")
	if err == nil {
		t.Fatal("expected READONLY_VIOLATION")
	}
	if got := output.ToError(err).Code; got != output.CodeReadonlyViolation {
		t.Errorf("code = %s, want READONLY_VIOLATION (%v)", got, err)
	}
}

func TestConnTestUnreachableScrubsPassword(t *testing.T) {
	setupEnv(t)
	addTestConn(t, "mqtt", "conn", "add", "down",
		"--url", "mqtt://127.0.0.1:1",
		"--username", "admin", "--password", testPassword, "--set-default")
	_, stderr, err := runMuxcatSE(t, "mqtt", "conn", "test", "down")
	if err == nil {
		t.Fatal("expected a connect failure")
	}
	e := output.ToError(err)
	if e.Code != output.CodeConnectFailed {
		t.Errorf("code = %s, want CONNECT_FAILED (%v)", e.Code, e)
	}
	if strings.Contains(e.Message+e.Hint+stderr, testPassword) {
		t.Errorf("connect failure must not leak the password: %v / %s", e, stderr)
	}
	if output.ExitCode(err) != output.ExitConnect {
		t.Errorf("exit code = %d, want %d", output.ExitCode(err), output.ExitConnect)
	}
}

func TestSchemaValidate(t *testing.T) {
	good := `{
	  "version": 1,
	  "instances": {"local": {"url": "mqtt://127.0.0.1:1883", "protocolVersion": 5}},
	  "connections": {"local": {"instance": "local", "username": "admin",
	    "password": "enc:v1:AAAA", "clientId": "muxcat-ops", "readonly": false,
	    "timeout": "30s", "tlsSkipVerify": false}},
	  "defaultConnection": "local"
	}`
	if err := schema.Validate(FileName, []byte(good)); err != nil {
		t.Errorf("valid document rejected: %v", err)
	}
	bad := `{"version": 1, "instances": {"x": {"url": "mqtt://h", "protocolVersion": 4}}}`
	if err := schema.Validate(FileName, []byte(bad)); err == nil {
		t.Error("invalid protocolVersion must fail schema validation")
	}
	unknownField := `{"version": 1, "connections": {"x": {"instance": "x", "surprise": true}}}`
	if err := schema.Validate(FileName, []byte(unknownField)); err == nil {
		t.Error("unknown connection field must fail schema validation")
	}
}

func TestConfigValidateCommand(t *testing.T) {
	setupEnv(t)
	runJSON(t, "mqtt", "conn", "add", "local",
		"--url", "mqtt://127.0.0.1:1883", "--set-default")
	env := runJSON(t, "config", "validate")
	data, _ := env["data"].(map[string]any)
	rows, _ := data["rows"].([]any)
	found := false
	for _, row := range rows {
		r, _ := row.([]any)
		if len(r) >= 2 && r[0] == FileName {
			found = true
			if r[1] != "ok" {
				t.Errorf("%s status = %v, want ok (%v)", FileName, r[1], env)
			}
		}
	}
	if !found {
		t.Errorf("%s not covered by config validate: %v", FileName, env)
	}
}

func TestBrokenConfigReportsProtocolVersion(t *testing.T) {
	setupEnv(t)
	dir := os.Getenv("MUXCAT_HOME")
	doc := `{"version": 1, "instances": {"x": {"url": "mqtt://h", "protocolVersion": 7}},
	  "connections": {"x": {"instance": "x"}}, "defaultConnection": "x"}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runMuxcat(t, "mqtt", "conn", "ls")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("expected CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(output.ToError(err).Message, `"x"`) {
		t.Errorf("error should name the offending instance: %v", err)
	}
}

// subResult carries a backgrounded sub invocation's outcome.
type subResult struct {
	out string
	err error
}

// pubSubLoop exercises one real pub->sub round trip against the embedded
// broker through the CLI surface.
func pubSubLoop(t *testing.T, connName string) {
	t.Helper()
	payload := "hello-" + connName
	done := make(chan subResult, 1)
	go func() {
		out, err := runMuxcat(t, "mqtt", "sub", "muxcat/test/topic",
			"--count", "1", "--timeout", "15s", "--json")
		done <- subResult{out, err}
	}()
	// The sub goroutine needs a head start to establish its subscription;
	// publishing a few times tolerates the race (identical payloads).
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 3; i++ {
		runJSON(t, "mqtt", "pub", "muxcat/test/topic", "--payload", payload, "--qos", "1", "-c", connName)
		time.Sleep(200 * time.Millisecond)
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("sub failed: %v\n%s", res.err, res.out)
	}
	env := decodeEnvelope(t, res.out)
	data, _ := env["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 message, got %v", env["data"])
	}
	msg, _ := data[0].(map[string]any)
	if msg["payload"] != payload {
		t.Errorf("payload = %v, want %q", msg["payload"], payload)
	}
	if msg["topic"] != "muxcat/test/topic" {
		t.Errorf("topic = %v", msg["topic"])
	}
	// conn test reports ok against the live broker.
	env = runJSON(t, "mqtt", "conn", "test", connName)
	v, _ := env["data"].(map[string]any)
	if v["ok"] != true {
		t.Errorf("conn test not ok: %v", env["data"])
	}
}

func TestPubSubLoopV3(t *testing.T) {
	setupEnv(t)
	addr := startBroker(t)
	addTestConn(t, "mqtt", "conn", "add", "v3",
		"--url", "mqtt://"+addr, "--set-default")
	pubSubLoop(t, "v3")
}

func TestPubSubLoopV5(t *testing.T) {
	setupEnv(t)
	addr := startBroker(t)
	addTestConn(t, "mqtt", "conn", "add", "v5",
		"--url", "mqtt://"+addr, "--protocol-version", "5", "--set-default")
	pubSubLoop(t, "v5")
}

func TestSubTimeoutZeroMessages(t *testing.T) {
	setupEnv(t)
	addr := startBroker(t)
	addTestConn(t, "mqtt", "conn", "add", "local",
		"--url", "mqtt://"+addr, "--set-default")
	_, err := runMuxcat(t, "mqtt", "sub", "muxcat/quiet/#", "--count", "1", "--timeout", "1s")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	e := output.ToError(err)
	if e.Code != output.CodeTimeout {
		t.Errorf("code = %s, want TIMEOUT (%v)", e.Code, e)
	}
	if output.ExitCode(err) != output.ExitConnect {
		t.Errorf("exit code = %d, want %d", output.ExitCode(err), output.ExitConnect)
	}
}

func TestConnTestAuthFailedNeverLeaksPassword(t *testing.T) {
	setupEnv(t)
	addr := startBrokerWithHook(t, &auth.Ledger{
		Auth: auth.AuthRules{
			{Username: "good", Password: "goodpw", Allow: true},
			{Allow: false},
		},
		ACL: auth.ACLRules{
			{Filters: auth.Filters{"#": auth.ReadWrite}},
		},
	})
	for _, name := range []string{"v3", "v5"} {
		args := []string{"mqtt", "conn", "add", name, "--url", "mqtt://" + addr,
			"--username", "good", "--password", testPassword, "--set-default"}
		if name == "v5" {
			args = append(args, "--protocol-version", "5")
		}
		addTestConn(t, args...)
		_, stderr, err := runMuxcatSE(t, "mqtt", "conn", "test", name)
		if err == nil {
			t.Fatalf("%s: expected an auth failure", name)
		}
		e := output.ToError(err)
		if e.Code != output.CodeAuthFailed {
			t.Errorf("%s: code = %s, want AUTH_FAILED (%v)", name, e.Code, e)
		}
		if strings.Contains(e.Message+e.Hint+stderr, testPassword) {
			t.Errorf("%s: auth failure must not leak the password: %v / %s", name, e, stderr)
		}
		if output.ExitCode(err) != output.ExitAuth {
			t.Errorf("%s: exit code = %d, want %d", name, output.ExitCode(err), output.ExitAuth)
		}
	}
}
