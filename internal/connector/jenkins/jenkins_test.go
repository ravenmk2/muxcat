package jenkins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	buildQuery   string
	postCalls    map[string]int
	writeCrumb   map[string]string
}

func newJkServer(t *testing.T) *jkServer {
	t.Helper()
	s := &jkServer{
		crumbValue: "crumb-v1",
		postCalls:  map[string]int{},
		writeCrumb: map[string]string{},
	}

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
			"content_type": r.Header.Get("Content-Type"),
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

	// --- v2 write-operation state machine ---------------------------------
	recordPost := func(r *http.Request) {
		s.mu.Lock()
		s.postCalls[r.URL.Path]++
		s.writeCrumb[r.URL.Path] = r.Header.Get("Jenkins-Crumb")
		s.mu.Unlock()
	}
	queueLoc := func(w http.ResponseWriter, id string) {
		w.Header().Set("Location", s.URL+"/queue/item/"+id+"/")
		w.WriteHeader(http.StatusCreated)
	}
	var queue99Calls, build7Calls int
	// pipe is parameterized: the probe sees parameterDefinitions, so job
	// build must use buildWithParameters even without --param.
	mux.HandleFunc("/job/pipe/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"property":[{"parameterDefinitions":[{"name":"ENV"},{"name":"TAG"}]}]}`))
	})
	mux.HandleFunc("/job/pipe/build", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
		queueLoc(w, "99")
	})
	mux.HandleFunc("/job/pipe/buildWithParameters", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
		s.mu.Lock()
		s.buildQuery = r.URL.RawQuery
		s.mu.Unlock()
		queueLoc(w, "99")
	})
	// Queue item 99 assigns build 7 on the second poll.
	mux.HandleFunc("/queue/item/99/api/json", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		queue99Calls++
		n := queue99Calls
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"why":"Waiting for next available executor"}`))
			return
		}
		_, _ = w.Write([]byte(`{"executable":{"number":7,"url":"` + s.URL + `/job/pipe/7/"}}`))
	})
	// Build 7 reports running on the first poll, SUCCESS after; its log
	// arrives in three progressiveText chunks.
	mux.HandleFunc("/job/pipe/7/api/json", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		build7Calls++
		n := build7Calls
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"number":7,"displayName":"#7","building":true,"result":null,"timestamp":1700000000000,"duration":0,"url":"` + s.URL + `/job/pipe/7/"}`))
			return
		}
		_, _ = w.Write([]byte(`{"number":7,"displayName":"#7","building":false,"result":"SUCCESS","timestamp":1700000000000,"duration":61500,"url":"` + s.URL + `/job/pipe/7/"}`))
	})
	pipeLog := []string{"part-one\n", "part-two\n", "part-three\n"}
	pipeLogEnds := make([]int64, 0, len(pipeLog))
	var pipeLogTotal int64
	for _, c := range pipeLog {
		pipeLogTotal += int64(len(c))
		pipeLogEnds = append(pipeLogEnds, pipeLogTotal)
	}
	mux.HandleFunc("/job/pipe/7/logText/progressiveText", func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		idx := 0
		for idx < len(pipeLogEnds) && pipeLogEnds[idx] <= start {
			idx++
		}
		body, size, more := "", pipeLogTotal, false
		if idx < len(pipeLog) {
			body, size = pipeLog[idx], pipeLogEnds[idx]
			more = idx < len(pipeLog)-1
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Text-Size", fmt.Sprintf("%d", size))
		if more {
			w.Header().Set("X-More-Data", "true")
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/job/pipe/7/stop", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
	})
	mux.HandleFunc("/job/pipe/enable", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
	})
	mux.HandleFunc("/job/pipe/disable", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
	})
	// plain is not parameterized: the probe sees no parameterDefinitions,
	// so job build must use plain /build.
	mux.HandleFunc("/job/plain/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"property":[]}`))
	})
	mux.HandleFunc("/job/plain/build", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
		queueLoc(w, "99")
	})
	// failing: build 8 reaches a terminal FAILURE immediately.
	mux.HandleFunc("/job/failing/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"property":[]}`))
	})
	mux.HandleFunc("/job/failing/build", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
		queueLoc(w, "102")
	})
	mux.HandleFunc("/queue/item/102/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"executable":{"number":8,"url":"` + s.URL + `/job/failing/8/"}}`))
	})
	mux.HandleFunc("/job/failing/8/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":8,"displayName":"#8","building":false,"result":"FAILURE","timestamp":1700000000000,"duration":12000,"url":"` + s.URL + `/job/failing/8/"}`))
	})
	// cancelled: the queue item is cancelled before assignment.
	mux.HandleFunc("/job/cancelled/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"property":[]}`))
	})
	mux.HandleFunc("/job/cancelled/build", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
		queueLoc(w, "100")
	})
	mux.HandleFunc("/queue/item/100/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cancelled":true,"why":"Cancelled by Alice"}`))
	})
	// stuck: the queue item never gets an executable (wait-timeout test).
	mux.HandleFunc("/job/stuck/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"property":[]}`))
	})
	mux.HandleFunc("/job/stuck/build", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
		queueLoc(w, "101")
	})
	// fast: the idle-server race — the queue item is already gone (404) at
	// the first poll, so the build must be found via builds[queueId]. The
	// api/json handler serves both the parameter probe and the builds
	// fallback (the mock ignores the tree filter).
	mux.HandleFunc("/job/fast/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"property":[],"builds":[{"number":9,"queueId":103},{"number":8,"queueId":97}]}`))
	})
	mux.HandleFunc("/job/fast/build", func(w http.ResponseWriter, r *http.Request) {
		recordPost(r)
		queueLoc(w, "103")
	})
	mux.HandleFunc("/queue/item/103/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/job/fast/9/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":9,"displayName":"#9","building":false,"result":"SUCCESS","timestamp":1700000000000,"duration":5000,"url":"` + s.URL + `/job/fast/9/"}`))
	})
	mux.HandleFunc("/queue/item/101/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"why":"Waiting for next available executor"}`))
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

func (s *jkServer) lastBuildQuery() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildQuery
}

func (s *jkServer) postCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.postCalls[path]
}

func (s *jkServer) crumbOn(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeCrumb[path]
}

// shrinkPollIntervals makes the --wait/--follow poll loops test-fast.
func shrinkPollIntervals(t *testing.T) {
	t.Helper()
	origQ, origB, origL := queuePollInterval, buildPollInterval, logPollInterval
	queuePollInterval, buildPollInterval, logPollInterval = time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() { queuePollInterval, buildPollInterval, logPollInterval = origQ, origB, origL })
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

// ---------------------------------------------------------------------------
// v2: write operations + log --follow
// ---------------------------------------------------------------------------

func TestJobBuildNoWait(t *testing.T) {
	setupEnv(t)
	shrinkPollIntervals(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	// --param switches to buildWithParameters and passes k=v via query.
	out, err := runMuxcat(t, "jk", "job", "build", "pipe", "--param", "ENV=uat", "--param", "TAG=v1.2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "queue item 99") {
		t.Fatalf("job build output unexpected:\n%s", out)
	}
	if s.postCount("/job/pipe/buildWithParameters") != 1 {
		t.Fatalf("buildWithParameters calls = %d", s.postCount("/job/pipe/buildWithParameters"))
	}
	q, _ := url.ParseQuery(s.lastBuildQuery())
	if q.Get("ENV") != "uat" || q.Get("TAG") != "v1.2" {
		t.Fatalf("build query = %q", s.lastBuildQuery())
	}
	if s.crumbOn("/job/pipe/buildWithParameters") != "crumb-v1" {
		t.Fatalf("crumb not attached: %q", s.crumbOn("/job/pipe/buildWithParameters"))
	}

	// A parameterized job goes to buildWithParameters even without
	// --param (a bare POST /build would 400 "Nothing is submitted").
	if _, err := runMuxcat(t, "jk", "job", "build", "pipe"); err != nil {
		t.Fatal(err)
	}
	if s.postCount("/job/pipe/buildWithParameters") != 2 || s.postCount("/job/pipe/build") != 0 {
		t.Fatalf("parameterized endpoint pick: bwp=%d build=%d",
			s.postCount("/job/pipe/buildWithParameters"), s.postCount("/job/pipe/build"))
	}

	// A non-parameterized job goes to plain /build; --json carries the
	// queue id.
	env := runJSON(t, "jk", "job", "build", "plain")
	data := env["data"].(map[string]any)
	if data["queued"] != true || data["queue_id"] != "99" {
		t.Fatalf("job build JSON unexpected: %v", data)
	}
	if s.postCount("/job/plain/build") != 1 {
		t.Fatalf("/job/plain/build calls = %d", s.postCount("/job/plain/build"))
	}
}

func TestJobBuildWaitSuccess(t *testing.T) {
	setupEnv(t)
	shrinkPollIntervals(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "jk", "job", "build", "pipe", "--wait")
	if err != nil {
		t.Fatalf("--wait failed: %v\n%s", err, out)
	}
	for _, want := range []string{"7", "SUCCESS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--wait output missing %q:\n%s", want, out)
		}
	}
}

func TestJobBuildWaitFailure(t *testing.T) {
	setupEnv(t)
	shrinkPollIntervals(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	// Text mode: the summary is rendered, then the command fails with
	// QUERY_ERROR (exit 5).
	out, err := runMuxcat(t, "jk", "job", "build", "failing", "--wait")
	e := output.ToError(err)
	if e.Code != output.CodeQueryError {
		t.Fatalf("code = %v, want %s", e, output.CodeQueryError)
	}
	if got := output.ExitCode(e); got != output.ExitExec {
		t.Fatalf("exit = %d, want %d", got, output.ExitExec)
	}
	if !strings.Contains(e.Message, "FAILURE") || !strings.Contains(out, "FAILURE") {
		t.Fatalf("failure not reported: %v\n%s", e, out)
	}

	// JSON mode: the error carries the build summary as partial data.
	_, err = runMuxcat(t, "jk", "job", "build", "failing", "--wait", "--json")
	e = output.ToError(err)
	if e.Code != output.CodeQueryError {
		t.Fatalf("json code = %v", e)
	}
	data, ok := e.Data.(map[string]any)
	if !ok || data["result"] != "FAILURE" {
		t.Fatalf("failure envelope lost the build data: %v", e.Data)
	}
}

func TestJobBuildQueueCancelled(t *testing.T) {
	setupEnv(t)
	shrinkPollIntervals(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	_, err := runMuxcat(t, "jk", "job", "build", "cancelled", "--wait")
	e := output.ToError(err)
	if e.Code != output.CodeQueryError || !strings.Contains(e.Message, "cancelled") {
		t.Fatalf("cancelled error = %v", e)
	}
}

func TestJobBuildWaitQueueGone(t *testing.T) {
	setupEnv(t)
	shrinkPollIntervals(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	// The queue item 404s at the first poll (idle-server race); the build
	// number is recovered by matching queueId in the job's builds.
	out, err := runMuxcat(t, "jk", "job", "build", "fast", "--wait")
	if err != nil {
		t.Fatalf("--wait over the queue-404 race failed: %v\n%s", err, out)
	}
	for _, want := range []string{"9", "SUCCESS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestJobBuildWaitTimeout(t *testing.T) {
	setupEnv(t)
	shrinkPollIntervals(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	_, err := runMuxcat(t, "jk", "job", "build", "stuck", "--wait", "--wait-timeout", "200ms")
	e := output.ToError(err)
	if e.Code != output.CodeTimeout {
		t.Fatalf("code = %v, want %s", e, output.CodeTimeout)
	}
	if !strings.Contains(e.Hint, "--wait-timeout") {
		t.Fatalf("hint should point at --wait-timeout: %q", e.Hint)
	}
}

func TestJobBuildParamInvalid(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	for _, bad := range []string{"NOEQ", "=novalue"} {
		_, err := runMuxcat(t, "jk", "job", "build", "pipe", "--param", bad)
		if output.ToError(err).Code != output.CodeConfigInvalid {
			t.Fatalf("--param %q code = %v", bad, output.ToError(err))
		}
	}
	if s.postCount("/job/pipe/build")+s.postCount("/job/pipe/buildWithParameters") != 0 {
		t.Fatal("invalid --param still triggered a build")
	}
}

func TestWriteReadonly(t *testing.T) {
	setupEnv(t)
	shrinkPollIntervals(t)
	s := newJkServer(t)
	s.addConn(t, "ro", "--readonly")

	cases := [][]string{
		{"jk", "-c", "ro", "job", "build", "pipe"},
		{"jk", "-c", "ro", "build", "stop", "pipe", "7"},
		{"jk", "-c", "ro", "job", "enable", "pipe"},
		{"jk", "-c", "ro", "job", "disable", "pipe"},
	}
	for _, args := range cases {
		_, err := runMuxcat(t, args...)
		if e := output.ToError(err); e.Code != output.CodeReadonlyViolation {
			t.Fatalf("%v code = %v, want %s", args, e, output.CodeReadonlyViolation)
		}
	}
	// No write request reached the server.
	for _, path := range []string{"/job/pipe/build", "/job/pipe/7/stop", "/job/pipe/enable", "/job/pipe/disable"} {
		if s.postCount(path) != 0 {
			t.Fatalf("readonly connection reached %s", path)
		}
	}
}

func TestBuildLogFollow(t *testing.T) {
	setupEnv(t)
	shrinkPollIntervals(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	// Text mode streams the chunks in order.
	out, err := runMuxcat(t, "jk", "build", "log", "pipe", "7", "--follow")
	if err != nil {
		t.Fatal(err)
	}
	i1 := strings.Index(out, "part-one")
	i2 := strings.Index(out, "part-two")
	i3 := strings.Index(out, "part-three")
	if i1 < 0 || i2 < 0 || i3 < 0 || i1 >= i2 || i2 >= i3 {
		t.Fatalf("follow chunks missing or out of order:\n%s", out)
	}

	// --json collects the whole log into one envelope.
	env := runJSON(t, "jk", "build", "log", "pipe", "7", "--follow")
	data := env["data"].(map[string]any)
	if data["log"] != "part-one\npart-two\npart-three\n" {
		t.Fatalf("follow JSON log = %q", data["log"])
	}
}

func TestBuildStopAndJobEnableDisable(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "jk", "build", "stop", "pipe", "7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "stop requested for pipe #7") {
		t.Fatalf("stop output unexpected:\n%s", out)
	}
	if s.postCount("/job/pipe/7/stop") != 1 || s.crumbOn("/job/pipe/7/stop") == "" {
		t.Fatalf("stop endpoint calls = %d, crumb = %q", s.postCount("/job/pipe/7/stop"), s.crumbOn("/job/pipe/7/stop"))
	}

	out, err = runMuxcat(t, "jk", "job", "disable", "pipe")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled job pipe") || s.postCount("/job/pipe/disable") != 1 {
		t.Fatalf("disable failed:\n%s", out)
	}
	out, err = runMuxcat(t, "jk", "job", "enable", "pipe")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "enabled job pipe") || s.postCount("/job/pipe/enable") != 1 {
		t.Fatalf("enable failed:\n%s", out)
	}

	// JSON shape of a write command.
	env := runJSON(t, "jk", "job", "disable", "pipe")
	if env["data"].(map[string]any)["enabled"] != false {
		t.Fatalf("disable JSON unexpected: %v", env["data"])
	}
}

func TestRequestContentType(t *testing.T) {
	setupEnv(t)
	s := newJkServer(t)
	s.addConn(t, "local")

	f := filepath.Join(t.TempDir(), "config.xml")
	if err := os.WriteFile(f, []byte("<project/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Default: a body goes out as application/json.
	env := runJSON(t, "jk", "request", "POST", "/echo", "--file", f)
	if env["data"].(map[string]any)["body"].(map[string]any)["content_type"] != "application/json" {
		t.Fatalf("default content type = %v", env["data"])
	}
	// --content-type overrides it (config.xml needs text/xml).
	env = runJSON(t, "jk", "request", "POST", "/echo", "--file", f, "--content-type", "text/xml")
	if env["data"].(map[string]any)["body"].(map[string]any)["content_type"] != "text/xml" {
		t.Fatalf("content type override lost = %v", env["data"])
	}
}
