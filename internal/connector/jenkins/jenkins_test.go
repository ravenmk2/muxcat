package jenkins

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

// jkServer is a fake Jenkins server for tests.
type jkServer struct {
	*httptest.Server
	mu           sync.Mutex
	authUser     string
	authToken    string
	crumbValue   string
	crumbFetches int
	logStart     string
}

func newJkServer(t *testing.T) *jkServer {
	t.Helper()
	s := &jkServer{crumbValue: "crumb-v1"}

	// A 250-line console log, long enough to exercise the tail cutoff.
	var logSB strings.Builder
	for i := 1; i <= 250; i++ {
		fmt.Fprintf(&logSB, "line-%d\n", i)
	}
	logBody := logSB.String()

	mux := http.NewServeMux()
	mux.HandleFunc("/crumbIssuer/api/json", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.crumbFetches++
		crumb := s.crumbValue
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"crumbRequestField": "Jenkins-Crumb", "crumb": crumb,
		})
	})
	mux.HandleFunc("/whoAmI/api/json", func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		s.mu.Lock()
		s.authUser, s.authToken = user, pass
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Jenkins", "2.462.3")
		_, _ = w.Write([]byte(`{"authenticated":true,"fullName":"Test Admin"}`))
	})
	mux.HandleFunc("/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Jenkins", "2.462.3")
		_, _ = w.Write([]byte(`{"_class":"hudson.model.Hudson","mode":"NORMAL","jobs":[
			{"_class":"org.jenkinsci.plugins.workflow.job.WorkflowJob","name":"deploy","fullName":"deploy","url":"http://x/job/deploy/","color":"blue","buildable":true,"lastBuild":{"number":42,"result":"SUCCESS","timestamp":1700000000000}},
			{"_class":"com.cloudbees.hudson.plugins.folder.Folder","name":"team","fullName":"team","url":"http://x/job/team/","color":"blue","buildable":true,"lastBuild":null},
			{"_class":"hudson.model.FreeStyleProject","name":"flaky","fullName":"flaky","url":"http://x/job/flaky/","color":"red_anime","buildable":true,"lastBuild":{"number":7,"result":null,"timestamp":1700000001000}}
		]}`))
	})
	mux.HandleFunc("/job/team/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[
			{"_class":"org.jenkinsci.plugins.workflow.job.WorkflowJob","name":"backend","fullName":"team/backend","url":"http://x/job/team/job/backend/","color":"yellow","buildable":true,"lastBuild":{"number":3,"result":"UNSTABLE","timestamp":1700000002000}}
		]}`))
	})
	mux.HandleFunc("/job/deploy/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"displayName":"deploy","fullName":"deploy","description":"deploy job","url":"http://x/job/deploy/","buildable":true,"color":"blue","concurrentBuild":false,
			"healthReport":[{"description":"Build stability: 1 out of the last 5 builds failed.","score":80}],
			"lastBuild":{"number":42,"result":"SUCCESS","timestamp":1700000000000},
			"lastSuccessfulBuild":{"number":42},"lastFailedBuild":{"number":40},
			"property":[{"parameterDefinitions":[
				{"name":"ENV","type":"StringParameterDefinition","defaultParameterValue":{"value":"prod"},"description":"target env"},
				{"name":"API_PASSWORD","type":"PasswordParameterDefinition","defaultParameterValue":{"value":"s3cr3t-value"},"description":"api password"}
			]}],
			"builds":[
				{"number":42,"result":"SUCCESS","timestamp":1700000000000,"duration":61500,"url":"http://x/job/deploy/42/","building":false},
				{"number":43,"result":null,"timestamp":1700000060000,"duration":0,"url":"http://x/job/deploy/43/","building":true}
			]}`))
	})
	buildJSON := `{"number":42,"displayName":"#42","description":"","result":"SUCCESS","building":false,"timestamp":1700000000000,"duration":61500,"url":"http://x/job/deploy/42/",
		"actions":[{"parameters":[
			{"_class":"hudson.model.StringParameterValue","name":"ENV","value":"prod"},
			{"_class":"hudson.model.PasswordParameterValue","name":"API_PASSWORD","value":"s3cr3t-value"},
			{"_class":"hudson.model.StringParameterValue","name":"DEPLOY_KEY","value":"key-material-123"}
		]},{"causes":[{"shortDescription":"Started by user Test Admin"}]}],
		"artifacts":[{"fileName":"app.tar.gz","relativePath":"dist/app.tar.gz","size":1048576}],
		"changeSet":{"items":[{"msg":"fix deploy script","author":{"fullName":"Alice"}}]}}`
	mux.HandleFunc("/job/deploy/42/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(buildJSON))
	})
	mux.HandleFunc("/job/deploy/lastBuild/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(buildJSON))
	})
	mux.HandleFunc("/job/deploy/42/logText/progressiveText", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.logStart = r.URL.Query().Get("start")
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Text-Size", fmt.Sprintf("%d", len(logBody)))
		w.Header().Set("X-More-Data", "false")
		_, _ = w.Write([]byte(logBody))
	})
	mux.HandleFunc("/job/deploy/lastBuild/logText/progressiveText", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Text-Size", fmt.Sprintf("%d", len(logBody)))
		_, _ = w.Write([]byte(logBody))
	})
	mux.HandleFunc("/queue/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		since := time.Now().Add(-90 * time.Second).UnixMilli()
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"id": 123, "task": map[string]any{"fullName": "deploy", "url": "http://x/job/deploy/"},
				"why": "Waiting for next available executor", "blocked": true, "buildable": false, "stuck": false, "inQueueSince": since},
		}})
	})
	mux.HandleFunc("/computer/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"computer":[
			{"displayName":"Built-In Node","offline":false,"temporarilyOffline":false,"idle":false,"numExecutors":2,
				"executors":[{"currentExecutable":{"fullName":"deploy","number":43}},{"currentExecutable":null}],"description":"controller"},
			{"displayName":"agent-1","offline":true,"temporarilyOffline":true,"idle":true,"numExecutors":4,"executors":[],"description":"linux agent"}
		]}`))
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"method": r.Method, "crumb": r.Header.Get("Jenkins-Crumb"),
		})
	})
	// /stale-crumb rejects the crumb it currently issues (simulating a
	// server restart) and accepts it once the client has refreshed: the
	// first failing attempt flips the issued crumb to crumb-v2.
	mux.HandleFunc("/stale-crumb", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Jenkins-Crumb") != "crumb-v2" {
			s.mu.Lock()
			s.crumbValue = "crumb-v2"
			s.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("No valid crumb was included in the request"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/protected", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html>Access denied</html>"))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *jkServer) lastAuth() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authUser, s.authToken
}

func (s *jkServer) crumbFetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.crumbFetches
}

func (s *jkServer) lastLogStart() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logStart
}

func (s *jkServer) addConn(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"jk", "conn", "add", name, "--url", s.URL, "--username", "admin"}, extra...)
	if out, err := runMuxcat(t, args...); err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
}

func TestConnLifecycle(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	const token = "11abcdef0123456789"
	s.addConn(t, "local", "--token", token, "--set-default")

	// The config file holds only the enc:v1: blob, never the plaintext.
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "enc:v1:") {
		t.Fatalf("config does not contain an encrypted token:\n%s", raw)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("config leaks the plaintext token:\n%s", raw)
	}

	// conn ls / conn show never echo the token.
	for _, args := range [][]string{{"jk", "conn", "ls"}, {"jk", "conn", "show", "local"}} {
		if out, err := runMuxcat(t, args...); err != nil {
			t.Fatalf("%v failed: %v", args, err)
		} else if strings.Contains(out, token) {
			t.Fatalf("%v leaks the plaintext token:\n%s", args, out)
		}
	}

	// conn test: auth check + version, and the server saw basic auth with
	// the decrypted token.
	env := runJSON(t, "jk", "conn", "test", "local")
	data := env["data"].(map[string]any)
	if data["ok"] != true || data["user"] != "Test Admin" || data["version"] != "2.462.3" {
		t.Fatalf("unexpected conn test data: %v", data)
	}
	if user, tok := s.lastAuth(); user != "admin" || tok != token {
		t.Fatalf("server saw auth %q/%q", user, tok)
	}

	// conn rm removes the connection (and its unreferenced instance).
	if out, err := runMuxcat(t, "jk", "conn", "rm", "local", "--yes"); err != nil {
		t.Fatalf("conn rm failed: %v\n%s", err, out)
	}
	if _, err := runMuxcat(t, "jk", "conn", "show", "local"); output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("show after rm code = %v", output.ToError(err))
	}
}

func TestConnAddRejectsUserinfoURL(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "jk", "conn", "add", "bad", "--url", "http://user:pass@127.0.0.1:8080")
	if e := output.ToError(err); e.Code != output.CodeConfigInvalid {
		t.Fatalf("code = %v, want %s", e, output.CodeConfigInvalid)
	}
}

func TestJobLs(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "jk", "job", "ls")
	if err != nil {
		t.Fatal(err)
	}
	// Folder recursion pulls team/backend into the flat listing; the color
	// ball maps to a readable status.
	for _, want := range []string{"deploy", "team", "team/backend", "workflow-job", "folder", "success", "building", "unstable", "#42 SUCCESS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("job ls output missing %q:\n%s", want, out)
		}
	}

	// JSON mode keeps the raw root response.
	env := runJSON(t, "jk", "job", "ls")
	jobs := env["data"].(map[string]any)["jobs"].([]any)
	if len(jobs) != 3 {
		t.Fatalf("JSON mode jobs = %d, want 3 (root only)", len(jobs))
	}

	// --folder starts the listing from that folder.
	out, err = runMuxcat(t, "jk", "job", "ls", "--folder", "team")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "team/backend") || strings.Contains(out, "flaky") {
		t.Fatalf("--folder listing unexpected:\n%s", out)
	}

	// --class filters display rows only: recursion still descends into
	// folders, so team/backend shows up under --class workflow-job.
	out, err = runMuxcat(t, "jk", "job", "ls", "--class", "workflow-job")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "team/backend") || strings.Contains(out, "folder") {
		t.Fatalf("--class workflow-job listing unexpected:\n%s", out)
	}
	out, err = runMuxcat(t, "jk", "job", "ls", "--class", "FOLDER")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "team") || strings.Contains(out, "workflow-job") {
		t.Fatalf("--class FOLDER (case-insensitive) listing unexpected:\n%s", out)
	}
}

func TestJobShow(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "jk", "job", "show", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deploy job") || !strings.Contains(out, "success") {
		t.Fatalf("job show output unexpected:\n%s", out)
	}
	// The password parameter default is masked, never echoed.
	if strings.Contains(out, "s3cr3t-value") {
		t.Fatalf("job show leaks a password parameter default:\n%s", out)
	}

	env := runJSON(t, "jk", "job", "show", "deploy")
	data := env["data"].(map[string]any)
	params := data["parameters"].([]any)
	if params[0].(map[string]any)["default"] != "prod" {
		t.Fatalf("string param default = %v", params[0])
	}
	if params[1].(map[string]any)["default"] != "***" {
		t.Fatalf("password param default not masked: %v", params[1])
	}
	if strings.Contains(fmt.Sprint(env), "s3cr3t-value") {
		t.Fatalf("job show JSON leaks a password parameter default: %v", env)
	}

	// Unknown job: 404 maps to QUERY_ERROR with a full-name hint.
	_, err = runMuxcat(t, "jk", "job", "show", "no-such-job")
	e := output.ToError(err)
	if e.Code != output.CodeQueryError || !strings.Contains(e.Message, "not found") {
		t.Fatalf("404 error = %v", e)
	}
}

func TestBuildLs(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "jk", "build", "ls", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"number", "result", "timestamp", "duration", "42", "SUCCESS", "BUILDING"} {
		if !strings.Contains(out, want) {
			t.Fatalf("build ls output missing %q:\n%s", want, out)
		}
	}
	env := runJSON(t, "jk", "build", "ls", "deploy")
	builds := env["data"].(map[string]any)["builds"].([]any)
	if len(builds) != 2 {
		t.Fatalf("JSON mode did not keep the raw builds: %v", builds)
	}
}

func TestBuildShow(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	// The "last" alias resolves to lastBuild.
	out, err := runMuxcat(t, "jk", "build", "show", "deploy", "last")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SUCCESS") || !strings.Contains(out, "Started by user Test Admin") {
		t.Fatalf("build show output unexpected:\n%s", out)
	}
	// Credential-bearing parameters are masked in text output.
	if strings.Contains(out, "s3cr3t-value") || strings.Contains(out, "key-material-123") {
		t.Fatalf("build show leaks parameter values:\n%s", out)
	}

	env := runJSON(t, "jk", "build", "show", "deploy", "42")
	data := env["data"].(map[string]any)
	params := data["parameters"].([]any)
	got := map[string]string{}
	for _, p := range params {
		pm := p.(map[string]any)
		got[pm["name"].(string)] = pm["value"].(string)
	}
	if got["ENV"] != "prod" {
		t.Fatalf("ENV param = %q", got["ENV"])
	}
	if got["API_PASSWORD"] != "***" || got["DEPLOY_KEY"] != "***" {
		t.Fatalf("credential params not masked: %v", got)
	}
	if data["causes"].([]any)[0] != "Started by user Test Admin" {
		t.Fatalf("causes = %v", data["causes"])
	}
	if data["artifacts"].([]any)[0].(map[string]any)["file"] != "app.tar.gz" {
		t.Fatalf("artifacts = %v", data["artifacts"])
	}
	if data["changes"].([]any)[0] != "Alice: fix deploy script" {
		t.Fatalf("changes = %v", data["changes"])
	}
	if strings.Contains(fmt.Sprint(env), "s3cr3t-value") || strings.Contains(fmt.Sprint(env), "key-material-123") {
		t.Fatalf("build show JSON leaks parameter values: %v", env)
	}

	// Invalid build reference is a usage error.
	_, err = runMuxcat(t, "jk", "build", "show", "deploy", "bogus")
	if output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("bogus ref code = %v", output.ToError(err))
	}
}

func TestBuildLog(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	// Default: the tail (last 200 lines) with a truncation marker.
	out, err := runMuxcat(t, "jk", "build", "log", "deploy", "42")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "showing last 200 lines, --full for all") {
		t.Fatalf("truncation marker missing:\n%s", out)
	}
	if !strings.Contains(out, "line-250\n") || strings.Contains(out, "line-50\n") {
		t.Fatalf("tail window wrong:\n%s", out)
	}
	if s.lastLogStart() != "0" {
		t.Fatalf("progressiveText start = %q, want 0", s.lastLogStart())
	}

	// --full prints everything without a marker.
	out, err = runMuxcat(t, "jk", "build", "log", "deploy", "42", "--full")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "line-1\n") || strings.Contains(out, "showing last") {
		t.Fatalf("--full output unexpected:\n%s", out)
	}

	// The global --limit overrides the tail size.
	out, err = runMuxcat(t, "jk", "build", "log", "deploy", "42", "--limit", "10")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "showing last 10 lines") || !strings.Contains(out, "line-241\n") || strings.Contains(out, "line-240\n") {
		t.Fatalf("--limit tail wrong:\n%s", out)
	}

	// JSON mode carries size + truncation state.
	env := runJSON(t, "jk", "build", "log", "deploy", "last")
	data := env["data"].(map[string]any)
	if data["truncated"] != true || data["size"].(float64) <= 0 {
		t.Fatalf("log JSON unexpected: %v", data)
	}
}

func TestQueueLs(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "jk", "queue", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id", "job", "why", "waiting", "state", "123", "deploy", "Waiting for next available executor", "blocked"} {
		if !strings.Contains(out, want) {
			t.Fatalf("queue ls output missing %q:\n%s", want, out)
		}
	}
	env := runJSON(t, "jk", "queue", "ls")
	if len(env["data"].(map[string]any)["items"].([]any)) != 1 {
		t.Fatalf("queue JSON not raw: %v", env["data"])
	}
}

func TestNodeLs(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "jk", "node", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Built-In Node", "agent-1", "online", "temp-offline", "1/2", "0/4", "controller"} {
		if !strings.Contains(out, want) {
			t.Fatalf("node ls output missing %q:\n%s", want, out)
		}
	}
	env := runJSON(t, "jk", "node", "ls")
	if len(env["data"].(map[string]any)["computer"].([]any)) != 2 {
		t.Fatalf("node JSON not raw: %v", env["data"])
	}
}

func TestRequestPassthrough(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	env := runJSON(t, "jk", "request", "GET", "/echo")
	data := env["data"].(map[string]any)
	if data["status"].(float64) != 200 || data["body"].(map[string]any)["method"] != "GET" {
		t.Fatalf("GET exchange = %v", data)
	}

	// A completed 403 exchange is reported, not an error.
	env = runJSON(t, "jk", "request", "GET", "/protected")
	if env["ok"] != true || env["data"].(map[string]any)["status"].(float64) != 403 {
		t.Fatalf("403 exchange = %v", env)
	}

	// Invalid method / path rejected as usage errors.
	if _, err := runMuxcat(t, "jk", "request", "FETCH", "/x"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("invalid method code = %v", output.ToError(err))
	}
	if _, err := runMuxcat(t, "jk", "request", "GET", "api/json"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("relative path code = %v", output.ToError(err))
	}
}

func TestRequestCrumb(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	// Unsafe methods fetch the crumb and attach it to the request.
	env := runJSON(t, "jk", "request", "POST", "/echo")
	data := env["data"].(map[string]any)
	if data["body"].(map[string]any)["crumb"] != "crumb-v1" {
		t.Fatalf("crumb not attached: %v", data["body"])
	}
	if s.crumbFetchCount() == 0 {
		t.Fatal("crumb issuer was never queried for a POST")
	}

	// A stale crumb is refreshed once and the call retried.
	fetches := s.crumbFetchCount()
	env = runJSON(t, "jk", "request", "POST", "/stale-crumb")
	if env["data"].(map[string]any)["status"].(float64) != 200 {
		t.Fatalf("stale-crumb retry = %v", env["data"])
	}
	if s.crumbFetchCount() != fetches+2 {
		t.Fatalf("crumb fetches = %d, want %d (initial + refresh)", s.crumbFetchCount(), fetches+2)
	}
}

func TestRequestReadonly(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "ro", "--readonly")

	if _, err := runMuxcat(t, "jk", "-c", "ro", "request", "GET", "/echo"); err != nil {
		t.Fatalf("readonly GET should pass: %v", err)
	}
	_, err := runMuxcat(t, "jk", "-c", "ro", "request", "POST", "/echo")
	e := output.ToError(err)
	if e.Code != output.CodeReadonlyViolation {
		t.Fatalf("code = %v, want %s", e, output.CodeReadonlyViolation)
	}
}

func TestConnectFailedAndTimeout(t *testing.T) {
	setupEnv(t)

	// Connection refused → CONNECT_FAILED (exit 3).
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if out, err := runMuxcat(t, "jk", "conn", "add", "dead", "--url", deadURL); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_, err := runMuxcat(t, "jk", "-c", "dead", "job", "ls")
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
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	}))
	defer slow.Close()
	if _, err := runMuxcat(t, "jk", "conn", "add", "slow", "--url", slow.URL, "--timeout", "50ms"); err != nil {
		t.Fatal(err)
	}
	_, err = runMuxcat(t, "jk", "-c", "slow", "job", "ls")
	e = output.ToError(err)
	if e.Code != output.CodeTimeout {
		t.Fatalf("code = %v, want %s", e, output.CodeTimeout)
	}
}

func TestClassifyStatus(t *testing.T) {
	// 401/403 map to AUTH_FAILED with an API-token hint.
	for _, status := range []int{401, 403} {
		e := classifyStatus(status, []byte("<html>denied</html>"))
		if e.Code != output.CodeAuthFailed {
			t.Fatalf("%d code = %s, want AUTH_FAILED", status, e.Code)
		}
		if !strings.Contains(e.Hint, "token") {
			t.Fatalf("%d hint = %q", status, e.Hint)
		}
	}
	// 404 maps to QUERY_ERROR with a "not found" message.
	if e := classifyStatus(404, nil); e.Code != output.CodeQueryError || !strings.Contains(e.Message, "not found") {
		t.Fatalf("404 error = %v", e)
	}
	// Other statuses truncate the (HTML) body.
	long := strings.Repeat("x", 1000)
	if e := classifyStatus(500, []byte(long)); e.Code != output.CodeQueryError || len(e.Message) > 560 {
		t.Fatalf("500 error = %v (len %d)", e.Code, len(e.Message))
	}
}

func TestJobPathAndBuildRef(t *testing.T) {
	if got := jobPath("folder/sub/job"); got != "/job/folder/job/sub/job/job" {
		t.Fatalf("jobPath = %q", got)
	}
	if got := jobPath("a b"); got != "/job/a%20b" {
		t.Fatalf("jobPath escaping = %q", got)
	}
	for in, want := range map[string]string{
		"last": "lastBuild", "lastSuccessful": "lastSuccessfulBuild",
		"lastFailed": "lastFailedBuild", "lastCompleted": "lastCompletedBuild",
		"42": "42",
	} {
		if got, err := buildRef(in); err != nil || got != want {
			t.Fatalf("buildRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-1", "latest", "abc"} {
		if _, err := buildRef(bad); err == nil {
			t.Fatalf("buildRef(%q) should fail", bad)
		}
	}
}

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{"version":1,"instances":{"local":{"url":"http://127.0.0.1:8080"}},"connections":{"local":{"instance":"local","username":"u","token":"enc:v1:x","readonly":false,"timeout":"5s"}},"defaultConnection":"local"}`)
	if err := schema.Validate(FileName, valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	invalid := []byte(`{"version":1,"instances":{"local":{"url":"http://x","extra":1}}}`)
	if err := schema.Validate(FileName, invalid); err == nil {
		t.Fatal("config with unknown instance field accepted")
	}
	invalidConn := []byte(`{"version":1,"connections":{"local":{"instance":"local","password":"enc:v1:x"}}}`)
	if err := schema.Validate(FileName, invalidConn); err == nil {
		t.Fatal("config with unknown connection field accepted")
	}
}
