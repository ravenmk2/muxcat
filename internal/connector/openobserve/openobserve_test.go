package openobserve

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
	"time"

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

// o2Server is a fake OpenObserve server for tests.
type o2Server struct {
	*httptest.Server
	mu         sync.Mutex
	searchBody map[string]any
	authUser   string
	authPass   string
}

func newO2Server(t *testing.T) *o2Server {
	t.Helper()
	s := &o2Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v0.0.0-test"))
	})
	mux.HandleFunc("/api/default/streams", func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		s.mu.Lock()
		s.authUser, s.authPass = user, pass
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"list":[{"name":"app","storage_type":"disk","stream_type":"logs","stats":{"doc_time_min":1673715046856933,"doc_time_max":1673849134852901,"doc_num":3300000,"file_num":16,"storage_size":3323.5,"compressed_size":11.42},"settings":{"partition_keys":{},"full_text_search_keys":["log"]}}]}`))
	})
	mux.HandleFunc("/api/default/streams/app/schema", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"app","storage_type":"disk","stream_type":"logs","stats":{"doc_num":3300000},"schema":[{"name":"_timestamp","type":"Int64"},{"name":"log","type":"Utf8"}],"settings":{"full_text_search_keys":["log"]}}`))
	})
	mux.HandleFunc("/api/default/_search", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.searchBody = body
		s.mu.Unlock()
		// A query referencing the unknown field "servce" fails like the
		// real server's SearchFieldNotFound (20004, with suggestions).
		if q, ok := body["query"].(map[string]any); ok {
			if sql, _ := q["sql"].(string); strings.Contains(sql, "servce") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":20004,"message":"unknown field 'servce'","suggestions":["service"]}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"took":12,"hits":[{"_timestamp":1674213225158000,"log":"first line","kubernetes":{"pod_name":"ziox-1"}},{"_timestamp":1674213226158000,"log":"second line","code":200,"missing":null}],"total":2,"from":0,"size":100,"scan_size":1.5}`))
	})
	mux.HandleFunc("/api/default/fail", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":20004,"message":"unknown field 'servce'","suggestions":["service"]}`))
	})
	mux.HandleFunc("/api/default/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"method": r.Method})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *o2Server) lastSearchBody() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searchBody
}

func (s *o2Server) lastAuth() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authUser, s.authPass
}

func (s *o2Server) addConn(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"o2", "conn", "add", name, "--url", s.URL, "--username", "root@example.com"}, extra...)
	if out, err := runMuxcat(t, args...); err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
}

func TestConnLifecycle(t *testing.T) {
	setupEnv(t)
	s := newO2Server(t)
	const password = "Sup3rSecret!"
	s.addConn(t, "local", "--password", password, "--set-default")

	// The config file holds only the enc:v1: blob, never the plaintext.
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "enc:v1:") {
		t.Fatalf("config does not contain an encrypted password:\n%s", raw)
	}
	if strings.Contains(string(raw), password) {
		t.Fatalf("config leaks the plaintext password:\n%s", raw)
	}

	// conn ls / conn show never echo the password.
	for _, args := range [][]string{{"o2", "conn", "ls"}, {"o2", "conn", "show", "local"}} {
		if out, err := runMuxcat(t, args...); err != nil {
			t.Fatalf("%v failed: %v", args, err)
		} else if strings.Contains(out, password) {
			t.Fatalf("%v leaks the plaintext password:\n%s", args, out)
		}
	}

	// conn test: auth check + version, and the server saw basic auth.
	env := runJSON(t, "o2", "conn", "test", "local")
	data := env["data"].(map[string]any)
	if data["ok"] != true || data["version"] != "v0.0.0-test" {
		t.Fatalf("unexpected conn test data: %v", data)
	}
	if user, pass := s.lastAuth(); user != "root@example.com" || pass != password {
		t.Fatalf("server saw auth %q/%q", user, pass)
	}
}

func TestConnTestVersionFallback(t *testing.T) {
	setupEnv(t)
	// A newer-style server: no GET /version; the version comes from the
	// _meta org's node list instead.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/default/streams", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"list":[]}`))
	})
	mux.HandleFunc("/api/_meta/node/list", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openobserve":{"zo1":[{"name":"n1","version":"v1.0.4"}]}}`))
	})
	s := &o2Server{Server: httptest.NewServer(mux)}
	t.Cleanup(s.Close)
	s.addConn(t, "local")

	env := runJSON(t, "o2", "conn", "test", "local")
	data := env["data"].(map[string]any)
	if data["ok"] != true || data["version"] != "v1.0.4" {
		t.Fatalf("version fallback failed: %v", data)
	}
}

func TestConnAddRejectsUserinfoURL(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "o2", "conn", "add", "bad", "--url", "http://user:pass@127.0.0.1:5080")
	if e := output.ToError(err); e.Code != output.CodeConfigInvalid {
		t.Fatalf("code = %v, want %s", e, output.CodeConfigInvalid)
	}
}

func TestStreamLsAndSchema(t *testing.T) {
	setupEnv(t)
	s := newO2Server(t)
	s.addConn(t, "local")

	// Text mode derives a table.
	out, err := runMuxcat(t, "o2", "stream", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name", "stream_type", "doc_num", "app", "logs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream ls output missing %q:\n%s", want, out)
		}
	}

	// JSON mode keeps the raw {list: [...]} response.
	env := runJSON(t, "o2", "stream", "ls")
	list := env["data"].(map[string]any)["list"].([]any)
	first := list[0].(map[string]any)
	if first["name"] != "app" || first["settings"] == nil {
		t.Fatalf("JSON mode did not keep the raw response: %v", first)
	}

	// schema: text table of fields, JSON keeps the raw object.
	out, err = runMuxcat(t, "o2", "stream", "schema", "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "_timestamp") || !strings.Contains(out, "Utf8") {
		t.Fatalf("stream schema output unexpected:\n%s", out)
	}
	env = runJSON(t, "o2", "stream", "schema", "app")
	if env["data"].(map[string]any)["name"] != "app" {
		t.Fatalf("schema JSON not raw: %v", env["data"])
	}
}

func TestSearchTextAndJSON(t *testing.T) {
	setupEnv(t)
	s := newO2Server(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "o2", "search", "SELECT * FROM app")
	if err != nil {
		t.Fatal(err)
	}
	// _timestamp pinned first and rendered as local readable time; nested
	// object compacted; first-appearance column order.
	header := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	if !strings.HasPrefix(header, "_timestamp") {
		t.Fatalf("first column is not _timestamp: %q", header)
	}
	if !strings.Contains(out, `{"pod_name":"ziox-1"}`) {
		t.Fatalf("nested object not compact JSON:\n%s", out)
	}
	if strings.Contains(out, "1674213225158000") {
		t.Fatalf("_timestamp not rendered as readable time:\n%s", out)
	}

	// JSON mode keeps the raw _search response.
	env := runJSON(t, "o2", "search", "SELECT * FROM app")
	data := env["data"].(map[string]any)
	if data["took"].(float64) != 12 || data["total"].(float64) != 2 {
		t.Fatalf("JSON mode did not keep the raw response: %v", data)
	}
	hits := data["hits"].([]any)
	if hits[0].(map[string]any)["kubernetes"].(map[string]any)["pod_name"] != "ziox-1" {
		t.Fatalf("hits not raw: %v", hits[0])
	}

	// The request body carried the default 1h window and from/size.
	body := s.lastSearchBody()
	q := body["query"].(map[string]any)
	diff := int64(q["end_time"].(float64)) - int64(q["start_time"].(float64))
	if diff != int64(time.Hour/time.Microsecond) {
		t.Fatalf("default window = %d µs, want 1h", diff)
	}
	if q["from"].(float64) != 0 || q["size"].(float64) != 100 {
		t.Fatalf("from/size not mapped: %v", q)
	}
}

func TestSearchTimeFlags(t *testing.T) {
	setupEnv(t)
	s := newO2Server(t)
	s.addConn(t, "local")

	if _, err := runMuxcat(t, "o2", "search", "SELECT * FROM app", "--last", "15m"); err != nil {
		t.Fatal(err)
	}
	q := s.lastSearchBody()["query"].(map[string]any)
	if diff := int64(q["end_time"].(float64)) - int64(q["start_time"].(float64)); diff != int64(15*time.Minute/time.Microsecond) {
		t.Fatalf("--last 15m window = %d µs", diff)
	}

	// --last is mutually exclusive with --start-time/--end-time.
	_, err := runMuxcat(t, "o2", "search", "SELECT * FROM app", "--last", "15m", "--start-time", "-2h")
	if output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("mutual exclusion code = %v", output.ToError(err))
	}
	// Invalid --last enum value.
	_, err = runMuxcat(t, "o2", "search", "SELECT * FROM app", "--last", "42m")
	if output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("invalid --last code = %v", output.ToError(err))
	}
	// Explicit absolute window (microseconds).
	_, err = runMuxcat(t, "o2", "search", "SELECT * FROM app",
		"--start-time", "1674000000000000", "--end-time", "1674003600000000")
	if err != nil {
		t.Fatal(err)
	}
	q = s.lastSearchBody()["query"].(map[string]any)
	if q["start_time"].(float64) != 1674000000000000 {
		t.Fatalf("start_time = %v", q["start_time"])
	}
	// start >= end rejected.
	_, err = runMuxcat(t, "o2", "search", "SELECT * FROM app", "--start-time", "-1h", "--end-time", "-2h")
	if output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("inverted window code = %v", output.ToError(err))
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"1674000000000000", time.UnixMicro(1674000000000000)},
		{"2026-07-03T11:00:00Z", now.Add(-time.Hour)},
		{"-90m", now.Add(-90 * time.Minute)},
		{"now", now},
	}
	for _, tc := range cases {
		got, err := parseTime(tc.in, now)
		if err != nil || !got.Equal(tc.want) {
			t.Fatalf("parseTime(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "1h", "tomorrow", "2026-13-01"} {
		if _, err := parseTime(bad, now); err == nil {
			t.Fatalf("parseTime(%q) should fail", bad)
		}
	}
}

func TestHitsToTable(t *testing.T) {
	hits := []json.RawMessage{
		json.RawMessage(`{"log":"a","_timestamp":1674213225158000,"meta":{"x":1}}`),
		json.RawMessage(`{"log":"b","extra":[1,2],"nothing":null,"_timestamp":1674213226158000}`),
	}
	cols, rows := hitsToTable(hits)
	want := []string{"_timestamp", "log", "meta", "extra", "nothing"}
	if fmt.Sprint(cols) != fmt.Sprint(want) {
		t.Fatalf("cols = %v, want %v", cols, want)
	}
	if rows[0][2] != `{"x":1}` || rows[1][3] != `[1,2]` {
		t.Fatalf("nested values not compact JSON: %v %v", rows[0][2], rows[1][3])
	}
	if rows[1][4] != nil {
		t.Fatalf("null should stay nil, got %v", rows[1][4])
	}
	ts, ok := rows[0][0].(string)
	if !ok || !strings.Contains(ts, ":") {
		t.Fatalf("_timestamp cell not formatted time: %v", rows[0][0])
	}
	// A hit missing a column yields a nil cell.
	if rows[0][3] != nil {
		t.Fatalf("missing column cell = %v, want nil", rows[0][3])
	}
}

func TestSearchErrorMapping(t *testing.T) {
	setupEnv(t)
	s := newO2Server(t)
	s.addConn(t, "local")

	// End to end: the server's hint/suggestions pass through into the
	// structured error, mapped to QUERY_ERROR (exit 5).
	_, err := runMuxcat(t, "o2", "search", "SELECT servce FROM app")
	e := output.ToError(err)
	if e.Code != output.CodeQueryError {
		t.Fatalf("code = %v, want %s", e, output.CodeQueryError)
	}
	if !strings.Contains(e.Hint, "service") {
		t.Fatalf("hint lost suggestions: %q", e.Hint)
	}
	if got := output.ExitCode(e); got != output.ExitExec {
		t.Fatalf("exit = %d, want %d", got, output.ExitExec)
	}

	// Unit-level: 401 maps to AUTH_FAILED (exit 4).
	authErr := classifyStatus(401, []byte(`{"message":"unauthorized"}`))
	if authErr.Code != output.CodeAuthFailed {
		t.Fatalf("401 code = %s, want AUTH_FAILED", authErr.Code)
	}
	if got := output.ExitCode(authErr); got != output.ExitAuth {
		t.Fatalf("401 exit = %d, want %d", got, output.ExitAuth)
	}
}

func TestRequestPassthrough(t *testing.T) {
	setupEnv(t)
	s := newO2Server(t)
	s.addConn(t, "local")

	env := runJSON(t, "o2", "request", "GET", "/api/default/echo")
	data := env["data"].(map[string]any)
	if data["status"].(float64) != 200 {
		t.Fatalf("status = %v", data["status"])
	}
	if data["headers"].(map[string]any)["Content-Type"] != "application/json" {
		t.Fatalf("headers = %v", data["headers"])
	}
	if data["body"].(map[string]any)["method"] != "GET" {
		t.Fatalf("body = %v", data["body"])
	}

	// A completed 400 exchange is reported, not an error.
	env = runJSON(t, "o2", "request", "POST", "/api/default/fail")
	if env["ok"] != true || env["data"].(map[string]any)["status"].(float64) != 400 {
		t.Fatalf("400 exchange = %v", env)
	}

	// Invalid method / path rejected as usage errors.
	if _, err := runMuxcat(t, "o2", "request", "FETCH", "/x"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("invalid method code = %v", output.ToError(err))
	}
	if _, err := runMuxcat(t, "o2", "request", "GET", "api/x"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("relative path code = %v", output.ToError(err))
	}
}

func TestRequestReadonly(t *testing.T) {
	setupEnv(t)
	s := newO2Server(t)
	s.addConn(t, "ro", "--readonly")

	// GET/HEAD allowed on a readonly connection.
	if _, err := runMuxcat(t, "o2", "-c", "ro", "request", "GET", "/api/default/echo"); err != nil {
		t.Fatalf("readonly GET should pass: %v", err)
	}
	// Write methods blocked with READONLY_VIOLATION (exit 5).
	_, err := runMuxcat(t, "o2", "-c", "ro", "request", "POST", "/api/default/echo")
	e := output.ToError(err)
	if e.Code != output.CodeReadonlyViolation {
		t.Fatalf("code = %v, want %s", e, output.CodeReadonlyViolation)
	}
	if got := output.ExitCode(e); got != output.ExitExec {
		t.Fatalf("readonly exit = %d, want %d", got, output.ExitExec)
	}
	// search is read-only and always allowed.
	if _, err := runMuxcat(t, "o2", "-c", "ro", "search", "SELECT * FROM app"); err != nil {
		t.Fatalf("readonly search should pass: %v", err)
	}
}

func TestConnectFailedAndTimeout(t *testing.T) {
	setupEnv(t)

	// Connection refused → CONNECT_FAILED (exit 3).
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if out, err := runMuxcat(t, "o2", "conn", "add", "dead", "--url", deadURL); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_, err := runMuxcat(t, "o2", "-c", "dead", "stream", "ls")
	e := output.ToError(err)
	if e.Code != output.CodeConnectFailed {
		t.Fatalf("code = %v, want %s", e, output.CodeConnectFailed)
	}
	if got := output.ExitCode(e); got != output.ExitConnect {
		t.Fatalf("exit = %d, want %d", got, output.ExitConnect)
	}

	// Slow server + tight connection timeout → TIMEOUT (exit 3).
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"list":[]}`))
	}))
	defer slow.Close()
	if _, err := runMuxcat(t, "o2", "conn", "add", "slow", "--url", slow.URL, "--timeout", "50ms"); err != nil {
		t.Fatal(err)
	}
	_, err = runMuxcat(t, "o2", "-c", "slow", "stream", "ls")
	e = output.ToError(err)
	if e.Code != output.CodeTimeout {
		t.Fatalf("code = %v, want %s", e, output.CodeTimeout)
	}
}

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{"version":1,"instances":{"local":{"url":"http://127.0.0.1:5080"}},"connections":{"local":{"instance":"local","org":"default","username":"u","password":"enc:v1:x","readonly":false,"timeout":"5s"}},"defaultConnection":"local"}`)
	if err := schema.Validate(FileName, valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	invalid := []byte(`{"version":1,"instances":{"local":{"url":"http://x","extra":1}}}`)
	if err := schema.Validate(FileName, invalid); err == nil {
		t.Fatal("config with unknown instance field accepted")
	}
}
