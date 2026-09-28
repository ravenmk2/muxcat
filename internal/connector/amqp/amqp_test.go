package amqp

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	amqp10 "github.com/Azure/go-amqp"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/rabbitmq/rabbitmq-amqp-go-client/pkg/rabbitmqamqp"

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

// readConfigFile returns the raw amqp.json content.
func readConfigFile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatalf("cannot read %s: %v", FileName, err)
	}
	return string(raw)
}

func TestConnAddSerializesEncryptedPassword(t *testing.T) {
	setupEnv(t)
	addTestConn(t, "amqp", "conn", "add", "local",
		"--url", "amqp://127.0.0.1:5672", "--username", "admin",
		"--password", testPassword, "--set-default")
	raw := readConfigFile(t)
	if !strings.Contains(raw, `"password": "enc:v1:`) {
		t.Errorf("password must be stored as an enc:v1: blob:\n%s", raw)
	}
	if strings.Contains(raw, testPassword) {
		t.Errorf("plaintext password leaked into %s:\n%s", FileName, raw)
	}
	// Protocol defaults to 1.0 and is omitted from the file.
	if strings.Contains(raw, "protocol") {
		t.Errorf("default protocol should be omitted:\n%s", raw)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConnection != "local" {
		t.Errorf("defaultConnection = %q, want local", cfg.DefaultConnection)
	}
	if got := cfg.Instances["local"].protocol(); got != Protocol10 {
		t.Errorf("protocol = %q, want %q", got, Protocol10)
	}
	conn := cfg.Connections["local"]
	if conn.Vhost != "" || conn.Username != "admin" {
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

func TestConnAddProtocol091(t *testing.T) {
	setupEnv(t)
	runJSON(t, "amqp", "conn", "add", "legacy",
		"--url", "amqp://127.0.0.1:5672", "--protocol", "0.9.1",
		"--username", "guest", "--set-default")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Instances["legacy"].Protocol; got != Protocol091 {
		t.Errorf("protocol = %q, want %q", got, Protocol091)
	}
}

func TestConnAddPasswordFlagWarns(t *testing.T) {
	setupEnv(t)
	_, stderr, err := runMuxcatSE(t, "amqp", "conn", "add", "local",
		"--url", "amqp://127.0.0.1:5672", "--password", testPassword)
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
	addTestConn(t, "amqp", "conn", "add", "local",
		"--url", "amqp://127.0.0.1:5672", "--username", "admin",
		"--password", testPassword, "--vhost", "/prod", "--set-default")
	for _, args := range [][]string{
		{"amqp", "conn", "ls"},
		{"amqp", "conn", "ls", "--json"},
		{"amqp", "conn", "show", "local"},
		{"amqp", "conn", "show", "local", "--json"},
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
	out, _ := runMuxcat(t, "amqp", "conn", "ls")
	for _, col := range []string{"name", "url", "protocol", "username", "vhost", "readonly", "default"} {
		if !strings.Contains(out, col) {
			t.Errorf("conn ls misses column %q:\n%s", col, out)
		}
	}
	if !strings.Contains(out, "/prod") {
		t.Errorf("conn ls should show the connection vhost:\n%s", out)
	}
}

func TestConnAddRejectsCredentialedURL(t *testing.T) {
	setupEnv(t)
	out, err := runMuxcat(t, "amqp", "conn", "add", "bad",
		"--url", "amqp://admin:topsecret@127.0.0.1:5672")
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
}

func TestConnAddValidation(t *testing.T) {
	setupEnv(t)
	cases := []struct {
		name string
		args []string
		code string
	}{
		{"bad protocol", []string{"amqp", "conn", "add", "x", "--url", "amqp://h:5672", "--protocol", "2.0"}, output.CodeConfigInvalid},
		{"http scheme", []string{"amqp", "conn", "add", "x", "--url", "http://h:15672"}, output.CodeConfigInvalid},
		{"skip-verify on plaintext", []string{"amqp", "conn", "add", "x", "--url", "amqp://h:5672", "--tls-skip-verify"}, output.CodeConfigInvalid},
		{"bad timeout", []string{"amqp", "conn", "add", "x", "--url", "amqp://h:5672", "--timeout", "never"}, output.CodeConfigInvalid},
		{"missing url non-interactive", []string{"amqp", "conn", "add", "x"}, output.CodeMissingArgument},
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
	// The protocol error lists the valid values.
	_, err := runMuxcat(t, "amqp", "conn", "add", "x", "--url", "amqp://h:5672", "--protocol", "2.0")
	e := output.ToError(err)
	if !strings.Contains(e.Hint, `"0.9.1"`) || !strings.Contains(e.Hint, `"1.0"`) {
		t.Errorf("protocol hint should list valid values: %q", e.Hint)
	}
}

func TestConnRmAndDefault(t *testing.T) {
	setupEnv(t)
	runJSON(t, "amqp", "conn", "add", "a", "--url", "amqp://h1:5672", "--set-default")
	runJSON(t, "amqp", "conn", "add", "b", "--url", "amqp://h2:5672")
	runJSON(t, "amqp", "conn", "default", "b")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConnection != "b" {
		t.Fatalf("defaultConnection = %q, want b", cfg.DefaultConnection)
	}
	runJSON(t, "amqp", "conn", "rm", "a", "--yes")
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
	if _, err := runMuxcat(t, "amqp", "conn", "rm", "ghost", "--yes"); err == nil ||
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
		{"amqp://127.0.0.1:5672", "amqp://127.0.0.1:5672", false},
		{"amqps://broker.example.com:5671/", "amqps://broker.example.com:5671", false},
		{" amqp://h:5672 ", "amqp://h:5672", false},
		{"http://h:15672", "", true},
		{"amqp://user:pw@h:5672", "", true},
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

func TestDialURI(t *testing.T) {
	inst := Instance{URL: "amqp://broker:5672"}
	conn := Connection{Username: "admin"}
	cases := []struct{ vhost, want string }{
		{"/", "amqp://admin:pw@broker:5672/"},
		{"", "amqp://admin:pw@broker:5672/"},
		{"/prod", "amqp://admin:pw@broker:5672/%2Fprod"},
		{"staging", "amqp://admin:pw@broker:5672/staging"},
	}
	for _, tc := range cases {
		if got := dialURI(inst, conn, tc.vhost, "pw"); got != tc.want {
			t.Errorf("dialURI vhost %q = %q, want %q", tc.vhost, got, tc.want)
		}
	}
}

func TestSanitizeErr(t *testing.T) {
	err := errors.New("dial amqp://admin:s3cr3t-pw@broker:5672/: refused")
	got := sanitizeErr(err, testPassword)
	if strings.Contains(got, testPassword) {
		t.Errorf("password not scrubbed: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("expected a redaction marker: %q", got)
	}
	// A password needing percent-encoding is scrubbed in encoded form too.
	raw := "p w/s"
	encErr := errors.New("dial amqp://u:p%20w%2Fs@h/: refused")
	if got := sanitizeErr(encErr, raw); strings.Contains(got, "p%20w%2Fs") {
		t.Errorf("encoded password not scrubbed: %q", got)
	}
	if got := sanitizeErr(errors.New("plain text"), ""); got != "plain text" {
		t.Errorf("empty password must leave the text alone: %q", got)
	}
}

func TestClassify091(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"auth", &amqp091.Error{Code: 403, Reason: "PLAIN login refused: invalid credentials"}, output.CodeAuthFailed},
		{"vhost", &amqp091.Error{Code: 403, Reason: "no access to this vhost"}, output.CodeAuthFailed},
		{"vhost perm", &amqp091.Error{Code: 530, Reason: "NOT_ALLOWED - access to vhost refused"}, output.CodeAuthFailed},
		{"not found", &amqp091.Error{Code: 404, Reason: "NOT_FOUND - no queue 'q'"}, output.CodeQueryError},
		{"locked", &amqp091.Error{Code: 405, Reason: "RESOURCE_LOCKED"}, output.CodeQueryError},
		{"precondition", &amqp091.Error{Code: 406, Reason: "PRECONDITION_FAILED - inequivalent arg"}, output.CodeQueryError},
		{"deprecated", &amqp091.Error{Code: 541, Reason: "INTERNAL_ERROR - Feature `transient_nonexcl_queues` is deprecated"}, output.CodeQueryError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classify091(tc.err, testPassword)
			if got := output.ToError(err).Code; got != tc.code {
				t.Errorf("code = %s, want %s (%v)", got, tc.code, err)
			}
		})
	}
	// The deprecated-feature reason rides in the hint.
	err := classify091(&amqp091.Error{Code: 541, Reason: "INTERNAL_ERROR - Feature `transient_nonexcl_queues` is deprecated"}, "")
	if !strings.Contains(output.ToError(err).Hint, "transient_nonexcl_queues") {
		t.Errorf("541 hint should carry the reason: %q", output.ToError(err).Hint)
	}
	// The vhost 403 points at vhost name and permissions, not credentials.
	err = classify091(&amqp091.Error{Code: 403, Reason: "no access to this vhost"}, "")
	if !strings.Contains(output.ToError(err).Hint, "vhost") {
		t.Errorf("vhost hint expected: %q", output.ToError(err).Hint)
	}
}

func TestClassify10(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"does not exist", rabbitmqamqp.ErrDoesNotExist, output.CodeQueryError},
		{"precondition", rabbitmqamqp.ErrPreconditionFailed, output.CodeQueryError},
		{"unauthorized", &amqp10.Error{Condition: "amqp:unauthorized-access", Description: "denied"}, output.CodeAuthFailed},
		{"not found", &amqp10.Error{Condition: "amqp:not-found", Description: "gone"}, output.CodeQueryError},
		{"locked", &amqp10.Error{Condition: "amqp:resource-locked", Description: "exclusive"}, output.CodeQueryError},
		{"precondition cond", &amqp10.Error{Condition: "amqp:precondition-failed", Description: "inequivalent"}, output.CodeQueryError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classify10(tc.err, testPassword)
			if got := output.ToError(err).Code; got != tc.code {
				t.Errorf("code = %s, want %s (%v)", got, tc.code, err)
			}
		})
	}
}

func TestClassifyDial(t *testing.T) {
	// TCP refused: CONNECT_FAILED, never carrying the password.
	err := classifyDial(errors.New("dial tcp 10.0.0.1:5672: connect: connection refused"), testPassword, Protocol10, "/")
	e := output.ToError(err)
	if e.Code != output.CodeConnectFailed {
		t.Errorf("code = %s, want CONNECT_FAILED", e.Code)
	}
	// 1.0 dropped mid-handshake (e.g. a vhost that does not exist): the
	// hint names the vhost.
	err = classifyDial(errors.New("read tcp: wsarecv: An existing connection was forcibly closed by the remote host"), "", Protocol10, "/ghost")
	if hint := output.ToError(err).Hint; !strings.Contains(hint, "/ghost") {
		t.Errorf("hint should name the effective vhost: %q", hint)
	}
	// 0.9.1 dial errors reuse the 0.9.1 classifier.
	err = classifyDial(&amqp091.Error{Code: 403, Reason: "PLAIN login refused"}, testPassword, Protocol091, "/")
	if output.ToError(err).Code != output.CodeAuthFailed {
		t.Errorf("code = %s, want AUTH_FAILED", output.ToError(err).Code)
	}
	// SASL credential rejection maps to AUTH_FAILED.
	err = classifyDial(errors.New("failed to open connection: SASL PLAIN auth failed with code 0x1"), testPassword, Protocol10, "/")
	if output.ToError(err).Code != output.CodeAuthFailed {
		t.Errorf("code = %s, want AUTH_FAILED", output.ToError(err).Code)
	}
}

func TestValidateProps(t *testing.T) {
	good := map[string]any{
		"message_id": "m-1", "correlation_id": "c-1", "content_type": "text/plain",
		"content_encoding": "utf-8", "reply_to": "rq", "type": "event",
		"expiration": "60000", "priority": float64(5), "timestamp": float64(1700000000),
		"user_id": "admin", "app_id": "myapp",
	}
	if err := validateProps(good); err != nil {
		t.Errorf("valid props rejected: %v", err)
	}
	cases := []struct {
		name  string
		props map[string]any
	}{
		{"unknown key", map[string]any{"bogus": 1}},
		{"priority range", map[string]any{"priority": float64(300)}},
		{"priority type", map[string]any{"priority": "high"}},
		{"timestamp type", map[string]any{"timestamp": true}},
		{"timestamp bad string", map[string]any{"timestamp": "yesterday"}},
		{"message_id type", map[string]any{"message_id": 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProps(tc.props)
			if err == nil {
				t.Fatal("expected CONFIG_INVALID")
			}
			if output.ToError(err).Code != output.CodeConfigInvalid {
				t.Errorf("code = %s, want CONFIG_INVALID", output.ToError(err).Code)
			}
		})
	}
	// Unknown keys list the valid ones.
	err := validateProps(map[string]any{"bogus": 1})
	if hint := output.ToError(err).Hint; !strings.Contains(hint, "message_id") || !strings.Contains(hint, "app_id") {
		t.Errorf("hint should list valid keys: %q", hint)
	}
	// RFC 3339 timestamps are accepted.
	if err := validateProps(map[string]any{"timestamp": "2026-09-28T12:00:00Z"}); err != nil {
		t.Errorf("RFC 3339 timestamp rejected: %v", err)
	}
}

func TestParseHeaders(t *testing.T) {
	h, err := parseHeaders("tenant=eu,trace=1")
	if err != nil || h["tenant"] != "eu" || h["trace"] != "1" {
		t.Errorf("parseHeaders = %v, %v", h, err)
	}
	if _, err := parseHeaders("no-equals-sign"); err == nil {
		t.Error("expected an error for a malformed entry")
	}
	if h, err := parseHeaders(""); err != nil || len(h) != 0 {
		t.Errorf("empty headers = %v, %v", h, err)
	}
}

func TestReadonlyViolationBeforeDial(t *testing.T) {
	setupEnv(t)
	// The instance is unreachable; a readonly refusal must fire before any
	// dialing, so the outcome is READONLY_VIOLATION, not CONNECT_FAILED.
	runJSON(t, "amqp", "conn", "add", "ro",
		"--url", "amqp://127.0.0.1:1", "--readonly", "--set-default")
	cases := [][]string{
		{"amqp", "queue", "declare", "q"},
		{"amqp", "queue", "delete", "q"},
		{"amqp", "queue", "purge", "q"},
		{"amqp", "queue", "get", "q", "--ackmode", "ack"},
		{"amqp", "exchange", "declare", "ex"},
		{"amqp", "exchange", "delete", "ex"},
		{"amqp", "exchange", "publish", "ex", "--payload", "hi"},
		{"amqp", "binding", "bind", "--exchange", "ex", "--queue", "q"},
		{"amqp", "binding", "unbind", "--exchange", "ex", "--queue", "q"},
		{"amqp", "consume", "q", "--ack"},
	}
	for _, args := range cases {
		_, err := runMuxcat(t, args...)
		if err == nil {
			t.Errorf("%v: expected READONLY_VIOLATION", args)
			continue
		}
		if got := output.ToError(err).Code; got != output.CodeReadonlyViolation {
			t.Errorf("%v: code = %s, want READONLY_VIOLATION (%v)", args, got, err)
		}
	}
}

func TestConnTestUnreachableScrubsPassword(t *testing.T) {
	setupEnv(t)
	addTestConn(t, "amqp", "conn", "add", "down",
		"--url", "amqp://127.0.0.1:1", "--protocol", "0.9.1",
		"--username", "admin", "--password", testPassword, "--set-default")
	_, stderr, err := runMuxcatSE(t, "amqp", "conn", "test", "down")
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
	  "instances": {"local": {"url": "amqp://127.0.0.1:5672", "protocol": "1.0"}},
	  "connections": {"local": {"instance": "local", "username": "admin",
	    "password": "enc:v1:AAAA", "vhost": "/", "readonly": false,
	    "timeout": "30s", "tlsSkipVerify": false}},
	  "defaultConnection": "local"
	}`
	if err := schema.Validate(FileName, []byte(good)); err != nil {
		t.Errorf("valid document rejected: %v", err)
	}
	bad := `{"version": 1, "instances": {"x": {"url": "amqp://h", "protocol": "2.0"}}}`
	if err := schema.Validate(FileName, []byte(bad)); err == nil {
		t.Error("invalid protocol must fail schema validation")
	}
	unknownField := `{"version": 1, "connections": {"x": {"instance": "x", "surprise": true}}}`
	if err := schema.Validate(FileName, []byte(unknownField)); err == nil {
		t.Error("unknown connection field must fail schema validation")
	}
}

func TestConfigValidateCommand(t *testing.T) {
	setupEnv(t)
	runJSON(t, "amqp", "conn", "add", "local",
		"--url", "amqp://127.0.0.1:5672", "--set-default")
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

func TestBrokenConfigReportsProtocol(t *testing.T) {
	setupEnv(t)
	dir := os.Getenv("MUXCAT_HOME")
	doc := `{"version": 1, "instances": {"x": {"url": "amqp://h", "protocol": "3.0"}},
	  "connections": {"x": {"instance": "x"}}, "defaultConnection": "x"}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runMuxcat(t, "amqp", "conn", "ls")
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("expected CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(output.ToError(err).Message, `"x"`) {
		t.Errorf("error should name the offending instance: %v", err)
	}
}
