package elasticsearch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

const testPassword = "s3cret-es-pw"
const testAPIKey = "dGVzdC1pZDp0b3BzZWNyZXQ="

// esServer is a fake Elasticsearch REST API for tests. It records auth and
// media-type headers per request and answers the root info endpoint.
type esServer struct {
	*httptest.Server
	mu            sync.Mutex
	productHeader bool
	requireAuth   bool
	lastBasicUser string
	lastBasicPass string
	lastAPIKey    string
	lastAccept    map[string]string
}

func (s *esServer) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.Method + " " + r.URL.EscapedPath()
	s.lastBasicUser, s.lastBasicPass, _ = r.BasicAuth()
	s.lastAPIKey = ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "ApiKey ") {
		s.lastAPIKey = h
	}
	s.lastAccept[key] = r.Header.Get("Accept")
}

func (s *esServer) acceptOf(method, path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAccept[method+" "+path]
}

func (s *esServer) lastAuth() (user, pass, apiKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBasicUser, s.lastBasicPass, s.lastAPIKey
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

const rootInfo = `{"name":"node-1","cluster_name":"docker-cluster","cluster_uuid":"abc","version":{"number":"8.15.0","build_flavor":"default","lucene_version":"9.11.1"},"tagline":"You Know, for Search"}`

func newEsHandler(s *esServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.requireAuth {
			user, pass, _ := r.BasicAuth()
			apiKey := r.Header.Get("Authorization")
			if (user != "elastic" || pass != testPassword) && apiKey != "ApiKey "+testAPIKey {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"type":"security_exception","reason":"unable to authenticate user"},"status":401}`))
				return
			}
		}
		s.record(r)
		p := r.URL.EscapedPath()
		switch {
		case p == "/" && r.Method == "GET":
			if s.productHeader {
				w.Header().Set("X-Elastic-Product", "Elasticsearch")
			}
			writeJSON(w, rootInfo)
		case p == "/echo":
			writeJSON(w, fmt.Sprintf(`{"method":%q,"accept":%q,"content_type":%q}`,
				r.Method, r.Header.Get("Accept"), r.Header.Get("Content-Type")))
		case p == "/olderror" && r.Method == "GET":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden legacy","status":403}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"index_not_found_exception","reason":"no such index [nope]"},"status":404}`))
		}
	}
}

func newEsServer(t *testing.T) *esServer {
	t.Helper()
	s := &esServer{
		productHeader: true,
		lastAccept:    map[string]string{},
	}
	s.Server = httptest.NewServer(newEsHandler(s))
	t.Cleanup(s.Close)
	return s
}

// addConn registers a connection pointing at the test server.
func (s *esServer) addConn(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"es", "conn", "add", name, "--url", s.URL}, extra...)
	if out, err := runMuxcat(t, args...); err != nil {
		t.Fatalf("conn add failed: %v\n%s", err, out)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	setupEnv(t)
	cfg := &Config{
		Version:   1,
		Instances: map[string]Instance{"prod": {URL: "https://es.example.com:9200"}},
		Connections: map[string]Connection{"prod": {
			Instance:           "prod",
			Username:           "elastic",
			Password:           "enc:v1:...",
			Readonly:           true,
			Timeout:            "30s",
			InsecureSkipVerify: true,
		}},
		DefaultConnection: "prod",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.DefaultConnection != "prod" ||
		got.Instances["prod"].URL != "https://es.example.com:9200" ||
		got.Connections["prod"].Password != "enc:v1:..." ||
		!got.Connections["prod"].InsecureSkipVerify ||
		got.Connections["prod"].authKind() != "basic" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	// omitempty fields stay absent from the serialized document.
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "apiKey") {
		t.Fatalf("empty apiKey serialized:\n%s", raw)
	}
}

func TestNormalizeURL(t *testing.T) {
	if got, err := normalizeURL("https://es.example.com:9200/"); err != nil || got != "https://es.example.com:9200" {
		t.Fatalf("normalize = %q, %v", got, err)
	}
	for _, bad := range []string{"", "es.example.com:9200", "ftp://es.example.com", "http://"} {
		if _, err := normalizeURL(bad); err == nil {
			t.Fatalf("normalizeURL(%q) accepted", bad)
		}
	}
	if _, err := normalizeURL("http://elastic:s3cret@127.0.0.1:9200"); err == nil {
		t.Fatal("userinfo url accepted")
	}
}

func TestConnLifecycle(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.requireAuth = true

	// Plaintext --password warns on stderr; stdout carries no credential.
	stdout, stderr, err := runMuxcatSE(t, "es", "conn", "add", "local", "--url", s.URL,
		"--username", "elastic", "--password", testPassword, "--set-default")
	if err != nil {
		t.Fatalf("conn add failed: %v\n%s", err, stdout)
	}
	if !strings.Contains(stderr, "Warning: --password") {
		t.Fatalf("expected plaintext warning on stderr:\n%s", stderr)
	}
	if strings.Contains(stdout, testPassword) || strings.Contains(stderr, testPassword) {
		t.Fatalf("output leaks the plaintext password:\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	// The config holds an enc:v1: blob, never the plaintext.
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "enc:v1:") {
		t.Fatalf("config does not contain an encrypted password:\n%s", raw)
	}
	if strings.Contains(string(raw), testPassword) {
		t.Fatalf("config leaks the plaintext password:\n%s", raw)
	}

	// conn ls / conn show never echo the password.
	for _, args := range [][]string{{"es", "conn", "ls"}, {"es", "conn", "show", "local"}} {
		if out, err := runMuxcat(t, args...); err != nil {
			t.Fatalf("%v failed: %v", args, err)
		} else if strings.Contains(out, testPassword) {
			t.Fatalf("%v leaks the plaintext password:\n%s", args, out)
		}
	}
	ls := runJSON(t, "es", "conn", "ls")
	rows := ls["data"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("conn ls rows = %v", rows)
	}
	row := rows[0].([]any)
	if row[0] != "local" || row[2] != "elastic" || row[3] != "basic" || row[7] != "*" {
		t.Fatalf("conn ls row unexpected: %v", row)
	}
	show := runJSON(t, "es", "conn", "show", "local")["data"].(map[string]any)
	if show["auth"] != "basic" || show["default"] != true {
		t.Fatalf("conn show unexpected: %v", show)
	}
	if _, leaked := show["password"]; leaked {
		t.Fatalf("conn show echoes the password field: %v", show)
	}

	// conn test: root info probe; the server saw basic auth with the
	// decrypted password.
	env := runJSON(t, "es", "conn", "test")
	data := env["data"].(map[string]any)
	if data["ok"] != true || data["version"] != "8.15.0" || data["cluster_name"] != "docker-cluster" {
		t.Fatalf("unexpected conn test data: %v", data)
	}
	if _, warned := data["warning"]; warned {
		t.Fatalf("unexpected warning: %v", data)
	}
	if user, pw, key := s.lastAuth(); user != "elastic" || pw != testPassword || key != "" {
		t.Fatalf("server saw auth %q/%q/%q", user, pw, key)
	}

	// conn default + rm.
	if _, err := runMuxcat(t, "es", "conn", "default", "nope"); err == nil ||
		output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("default nope should be CONN_NOT_FOUND: %v", err)
	}
	if out, err := runMuxcat(t, "es", "conn", "rm", "local", "--yes"); err != nil {
		t.Fatalf("conn rm failed: %v\n%s", err, out)
	}
	if _, err := runMuxcat(t, "es", "conn", "show", "local"); output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("show after rm code = %v", output.ToError(err))
	}
}

func TestConnAddMutualExclusion(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	_, err := runMuxcat(t, "es", "conn", "add", "bad", "--url", s.URL,
		"--username", "elastic", "--password", testPassword, "--apikey", testAPIKey)
	if e := output.ToError(err); e.Code != output.CodeConfigInvalid {
		t.Fatalf("code = %v, want %s", e, output.CodeConfigInvalid)
	}
	// Nothing was persisted.
	if _, err := runMuxcat(t, "es", "conn", "show", "bad"); output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("rejected connection should not exist: %v", err)
	}
}

func TestConnAddAPIKeyAuth(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.requireAuth = true

	stdout, stderr, err := runMuxcatSE(t, "es", "conn", "add", "key", "--url", s.URL, "--apikey", testAPIKey)
	if err != nil {
		t.Fatalf("conn add failed: %v\n%s", err, stdout)
	}
	if !strings.Contains(stderr, "Warning: --apikey") {
		t.Fatalf("expected plaintext warning on stderr:\n%s", stderr)
	}
	if strings.Contains(stdout, testAPIKey) || strings.Contains(stderr, testAPIKey) {
		t.Fatalf("output leaks the api key:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testAPIKey) || !strings.Contains(string(raw), "enc:v1:") {
		t.Fatalf("config mishandles the api key:\n%s", raw)
	}

	env := runJSON(t, "es", "conn", "test")
	if env["data"].(map[string]any)["ok"] != true {
		t.Fatalf("conn test failed: %v", env)
	}
	if _, _, key := s.lastAuth(); key != "ApiKey "+testAPIKey {
		t.Fatalf("server saw Authorization %q", key)
	}
}

// Without the X-Elastic-Product header (OpenSearch fork or very old ES) the
// probe still succeeds but carries a warning.
func TestConnTestDegradedNoProductHeader(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.productHeader = false
	s.addConn(t, "fork")

	env := runJSON(t, "es", "conn", "test")
	data := env["data"].(map[string]any)
	if data["ok"] != true || data["version"] != "8.15.0" {
		t.Fatalf("unexpected conn test data: %v", data)
	}
	warning, _ := data["warning"].(string)
	if !strings.Contains(warning, "X-Elastic-Product") {
		t.Fatalf("expected degradation warning: %v", data)
	}
}

// conn test must honor the -c/--conn flag when no positional name is given,
// not silently fall back to the default connection.
func TestConnTestHonorsConnFlag(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.addConn(t, "first") // becomes the default
	s.addConn(t, "second")

	env := runJSON(t, "es", "-c", "second", "conn", "test")
	meta := env["meta"].(map[string]any)
	if meta["connection"] != "second" {
		t.Fatalf("conn test ignored -c, tested %v", meta["connection"])
	}
}

func TestRequestRawSemantics(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.addConn(t, "local")

	// A completed 404 exchange is data, not an error.
	env := runJSON(t, "es", "request", "GET", "/nope/_search")
	data := env["data"].(map[string]any)
	if data["status"].(float64) != 404 {
		t.Fatalf("status = %v", data)
	}
	body := data["body"].(map[string]any)
	if body["error"].(map[string]any)["reason"] != "no such index [nope]" {
		t.Fatalf("body = %v", body)
	}

	// Path must start with /.
	if _, err := runMuxcat(t, "es", "request", "GET", "nope"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("bad path code = %v", output.ToError(err))
	}
	// Method enum.
	if _, err := runMuxcat(t, "es", "request", "YEET", "/nope"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("bad method code = %v", output.ToError(err))
	}
}

func TestRequestReadonlyGuard(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.addConn(t, "ro", "--readonly")

	if _, err := runMuxcat(t, "es", "request", "POST", "/idx/_doc", "-c", "ro"); output.ToError(err).Code != output.CodeReadonlyViolation {
		t.Fatalf("POST on readonly code = %v", output.ToError(err))
	}
	if _, err := runMuxcat(t, "es", "request", "GET", "/idx/_search", "-c", "ro"); err != nil {
		t.Fatalf("GET on readonly should pass: %v", err)
	}
}

func TestRequestCompatHeader(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.addConn(t, "local")

	f := filepath.Join(t.TempDir(), "q.json")
	if err := os.WriteFile(f, []byte(`{"query":{"match_all":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Default: no compat media type; a body goes out as application/json.
	env := runJSON(t, "es", "request", "POST", "/echo", "--file", f)
	body := env["data"].(map[string]any)["body"].(map[string]any)
	if body["content_type"] != "application/json" || body["accept"] != "" {
		t.Fatalf("default headers unexpected: %v", body)
	}
	if s.acceptOf("POST", "/echo") != "" {
		t.Fatalf("Accept should be unset by default: %q", s.acceptOf("POST", "/echo"))
	}

	// --compat 7 sends the versioned media type on Accept and Content-Type.
	want := "application/vnd.elasticsearch+json;compatible-with=7"
	env = runJSON(t, "es", "request", "POST", "/echo", "--file", f, "--compat", "7")
	body = env["data"].(map[string]any)["body"].(map[string]any)
	if body["accept"] != want || body["content_type"] != want {
		t.Fatalf("compat headers unexpected: %v", body)
	}

	// Invalid compat values are rejected client-side.
	if _, err := runMuxcat(t, "es", "request", "GET", "/", "--compat", "9"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("bad compat code = %v", output.ToError(err))
	}
}

// classifyStatus extracts the server reason from both error body shapes.
func TestClassifyStatus(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.addConn(t, "local")

	// An auth failure surfaces AUTH_FAILED with the server's reason.
	s.requireAuth = true
	_, err := runMuxcat(t, "es", "conn", "test")
	if e := output.ToError(err); e.Code != output.CodeAuthFailed ||
		!strings.Contains(e.Message, "unable to authenticate user") {
		t.Fatalf("auth failure classification = %v", e)
	}
	s.requireAuth = false

	// Legacy string error shape on 403.
	_, err = runMuxcat(t, "es", "request", "GET", "/olderror")
	if err != nil {
		t.Fatalf("request is raw passthrough, 403 should be data: %v", err)
	}
	if got := errorReason([]byte(`{"error":"forbidden legacy","status":403}`)); got != "forbidden legacy" {
		t.Fatalf("errorReason string shape = %q", got)
	}
	if got := errorReason([]byte(`{"error":{"type":"x","reason":"y"},"status":400}`)); got != "y" {
		t.Fatalf("errorReason object shape = %q", got)
	}
}

func TestInsecureSkipVerify(t *testing.T) {
	setupEnv(t)
	s := &esServer{
		productHeader: true,
		lastAccept:    map[string]string{},
	}
	s.Server = httptest.NewTLSServer(newEsHandler(s))
	t.Cleanup(s.Close)

	// Default verification fails against the self-signed certificate.
	s.addConn(t, "strict")
	_, err := runMuxcat(t, "es", "conn", "test", "strict")
	if e := output.ToError(err); e.Code != output.CodeConnectFailed {
		t.Fatalf("self-signed without skip-verify code = %v, want %s", e, output.CodeConnectFailed)
	}

	// insecureSkipVerify opts out and the probe succeeds.
	s.addConn(t, "lax", "--insecure-skip-verify")
	env := runJSON(t, "es", "conn", "test", "lax")
	if env["data"].(map[string]any)["ok"] != true {
		t.Fatalf("conn test with skip-verify failed: %v", env)
	}
}

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{"version":1,"instances":{"prod":{"url":"https://es.example.com:9200"}},"connections":{"prod":{"instance":"prod","username":"elastic","password":"enc:v1:x","readonly":false,"timeout":"30s","insecureSkipVerify":true}},"defaultConnection":"prod"}`)
	if err := schema.Validate(FileName, valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	validKey := []byte(`{"version":1,"connections":{"prod":{"instance":"prod","apiKey":"enc:v1:x"}}}`)
	if err := schema.Validate(FileName, validKey); err != nil {
		t.Fatalf("valid apikey config rejected: %v", err)
	}
	invalid := []byte(`{"version":1,"instances":{"prod":{"url":"https://x","extra":1}}}`)
	if err := schema.Validate(FileName, invalid); err == nil {
		t.Fatal("config with unknown instance field accepted")
	}
	invalidConn := []byte(`{"version":1,"connections":{"prod":{"instance":"prod","token":"enc:v1:x"}}}`)
	if err := schema.Validate(FileName, invalidConn); err == nil {
		t.Fatal("config with unknown connection field accepted")
	}
}

// A failed request must not leak decrypted credentials through any output
// channel (error message, hint, stdout, stderr).
func TestErrorNoCredentialLeak(t *testing.T) {
	setupEnv(t)
	s := newEsServer(t)
	s.requireAuth = true
	s.addConn(t, "local", "--username", "elastic", "--password", testPassword)

	// Wrong credentials at the server: AUTH_FAILED; the error and output
	// must not contain the decrypted password.
	s.addConn(t, "wrong", "--username", "elastic", "--password", "wr0ng-pw")
	stdout, stderr, err := runMuxcatSE(t, "es", "conn", "test", "wrong")
	if err == nil {
		t.Fatal("conn test with wrong password should fail")
	}
	if e := output.ToError(err); strings.Contains(e.Message, "wr0ng-pw") || strings.Contains(e.Hint, "wr0ng-pw") {
		t.Fatalf("error leaks the password: %v", e)
	}
	if strings.Contains(stdout, "wr0ng-pw") || strings.Contains(stderr, "wr0ng-pw") {
		t.Fatalf("output leaks the password:\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	// A transport failure against a down server reports the URL (userinfo-
	// free) and no credentials.
	s.addConn(t, "down", "--url", "http://127.0.0.1:1", "--username", "elastic", "--password", testPassword, "--timeout", "1s")
	stdout, stderr, err = runMuxcatSE(t, "es", "conn", "test", "down")
	if e := output.ToError(err); e.Code != output.CodeConnectFailed {
		t.Fatalf("down server code = %v", e)
	}
	if strings.Contains(stdout, testPassword) || strings.Contains(stderr, testPassword) {
		t.Fatalf("output leaks the password:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}
