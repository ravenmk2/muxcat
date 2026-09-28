package rabbitmq

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// rmqServer is a fake RabbitMQ Management API for tests. It dispatches on
// the escaped path (vhost "/" travels as %2F) and records write calls.
type rmqServer struct {
	*httptest.Server
	mu        sync.Mutex
	authUser  string
	authPass  string
	writes    map[string]int
	lastBody  map[string][]byte
	lastQuery map[string]string
}

func (s *rmqServer) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.Method + " " + r.URL.EscapedPath()
	s.writes[key]++
	if raw, err := io.ReadAll(r.Body); err == nil {
		s.lastBody[key] = raw
	}
	s.lastQuery[key] = r.URL.RawQuery
}

func (s *rmqServer) writeCount(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[method+" "+path]
}

func (s *rmqServer) bodyOf(method, path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBody[method+" "+path]
}

func (s *rmqServer) queryOf(method, path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastQuery[method+" "+path]
}

func (s *rmqServer) lastAuth() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authUser, s.authPass
}

const testPassword = "s3cr3t-pw"

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func newRmqServer(t *testing.T) *rmqServer {
	t.Helper()
	s := &rmqServer{
		writes:    map[string]int{},
		lastBody:  map[string][]byte{},
		lastQuery: map[string]string{},
	}

	// 4.x-shaped queue: carries a "type" field.
	queue4x := `{"name":"q1","vhost":"/","type":"classic","state":"running","messages":12,"messages_ready":10,"messages_unacknowledged":2,"consumers":1,"memory":1048576,"durable":true,"arguments":{}}`
	// 3.8-shaped queue: no "type" field, mirrored-queue fields present;
	// lenient parsing must accept both shapes.
	queue38 := `{"name":"legacy-q","vhost":"/","state":"running","messages":5,"messages_ready":5,"messages_unacknowledged":0,"consumers":1,"memory":2048,"durable":true,"slave_pids":[],"synchronised_slave_pids":[],"arguments":{"x-queue-type":"quorum"}}`
	streamQ := `{"name":"events-0","vhost":"/","type":"stream","state":"running","messages":100,"messages_ready":100,"messages_unacknowledged":0,"consumers":2,"memory":4096,"arguments":{"x-queue-type":"stream"}}`
	streamQPlain := `{"name":"orders","vhost":"/","type":"stream","state":"running","messages":7,"messages_ready":7,"messages_unacknowledged":0,"consumers":0,"memory":1024,"arguments":{"x-queue-type":"stream"}}`
	allQueues := "[" + strings.Join([]string{queue4x, queue38, streamQ, streamQPlain}, ",") + "]"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auth gate: everything requires the configured basic auth.
		user, pass, _ := r.BasicAuth()
		if user != "admin" || pass != testPassword {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"not_authorised","reason":"Login failed"}`))
			return
		}
		s.mu.Lock()
		s.authUser, s.authPass = user, pass
		s.mu.Unlock()

		p := r.URL.EscapedPath()
		switch {
		case p == "/api/whoami" && r.Method == "GET":
			writeJSON(w, `{"name":"admin","tags":"administrator"}`)
		case p == "/echo":
			writeJSON(w, fmt.Sprintf(`{"method":%q,"content_type":%q}`, r.Method, r.Header.Get("Content-Type")))
		case p == "/api/overview" && r.Method == "GET":
			writeJSON(w, `{"product_name":"RabbitMQ","rabbitmq_version":"4.3.1","cluster_name":"rabbit@test","erlang_version":"27.0","management_version":"4.3.1","object_totals":{"connections":3,"channels":5,"exchanges":8,"queues":4,"consumers":2},"queue_totals":{"messages":124,"messages_ready":122,"messages_unacknowledged":2}}`)
		case p == "/api/nodes" && r.Method == "GET":
			writeJSON(w, `[{"name":"rabbit@test","type":"disc","running":true,"mem_used":268435456,"disk_free":10737418240,"fd_used":42,"sockets_used":7,"uptime":3661000}]`)
		case strings.HasPrefix(p, "/api/nodes/") && r.Method == "GET":
			writeJSON(w, `{"name":"rabbit@test","type":"disc","running":true,"mem_used":268435456,"partitions":[]}`)
		case p == "/api/queues" && r.Method == "GET":
			writeJSON(w, allQueues)
		case p == "/api/queues/%2F" && r.Method == "GET":
			writeJSON(w, allQueues)
		case p == "/api/queues/%2F/q1/get" && r.Method == "POST":
			s.record(r)
			writeJSON(w, `[{"payload":"aGVsbG8=","payload_encoding":"base64","exchange":"ex1","routing_key":"rk","redelivered":false,"message_count":11,"properties":{}}]`)
		case strings.HasSuffix(p, "/contents") && r.Method == "DELETE":
			s.record(r)
			w.WriteHeader(http.StatusNoContent)
		case p == "/api/queues/%2F/bad" && r.Method == "PUT":
			s.record(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_request","reason":"some reason text"}`))
		case strings.HasPrefix(p, "/api/queues/%2F/") && r.Method == "GET":
			writeJSON(w, queue4x)
		case strings.HasPrefix(p, "/api/queues/%2F/") && r.Method == "PUT":
			s.record(r)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(p, "/api/queues/%2F/") && r.Method == "DELETE":
			s.record(r)
			w.WriteHeader(http.StatusNoContent)
		case p == "/api/exchanges" && r.Method == "GET":
			writeJSON(w, `[{"name":"ex1","vhost":"/","type":"fanout","durable":true,"auto_delete":false,"internal":false,"arguments":{}},{"name":"amq.direct","vhost":"/","type":"direct","durable":true,"auto_delete":false,"internal":false,"arguments":{}}]`)
		case strings.HasPrefix(p, "/api/exchanges/%2F/") && strings.HasSuffix(p, "/publish") && r.Method == "POST":
			s.record(r)
			writeJSON(w, `{"routed":true}`)
		case p == "/api/exchanges/%2F/ex1" && r.Method == "GET":
			writeJSON(w, `{"name":"ex1","vhost":"/","type":"fanout","durable":true,"auto_delete":false,"internal":false,"arguments":{}}`)
		case strings.HasPrefix(p, "/api/exchanges/%2F/") && r.Method == "PUT":
			s.record(r)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(p, "/api/exchanges/%2F/") && r.Method == "DELETE":
			s.record(r)
			w.WriteHeader(http.StatusNoContent)
		case p == "/api/bindings" && r.Method == "GET":
			writeJSON(w, `[{"vhost":"/","source":"ex1","destination":"q1","destination_type":"queue","routing_key":"rk","properties_key":"rk","arguments":{}}]`)
		case p == "/api/bindings/%2F/e/ex1/q/q1" && r.Method == "GET":
			writeJSON(w, `[{"vhost":"/","source":"ex1","destination":"q1","destination_type":"queue","routing_key":"rk","properties_key":"rk","arguments":{}}]`)
		case strings.HasPrefix(p, "/api/bindings/%2F/e/") && r.Method == "POST":
			s.record(r)
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(p, "/api/bindings/%2F/e/") && r.Method == "DELETE":
			s.record(r)
			w.WriteHeader(http.StatusNoContent)
		case p == "/api/connections" && r.Method == "GET":
			writeJSON(w, `[{"name":"127.0.0.1:50000 -> 127.0.0.1:5672","user":"admin","vhost":"/","state":"running","channels":2,"peer_host":"127.0.0.1","peer_port":50000,"protocol":"AMQP 0-9-1"}]`)
		case strings.HasPrefix(p, "/api/connections/") && r.Method == "GET":
			writeJSON(w, `{"name":"127.0.0.1:50000 -> 127.0.0.1:5672","user":"admin","vhost":"/","state":"running"}`)
		case strings.HasPrefix(p, "/api/connections/") && r.Method == "DELETE":
			s.record(r)
			w.WriteHeader(http.StatusNoContent)
		case p == "/api/channels" && r.Method == "GET":
			writeJSON(w, `[{"name":"127.0.0.1:50000 -> 127.0.0.1:5672 (1)","vhost":"/","user":"admin","state":"running","transactional":false,"confirm":true,"messages_unacknowledged":0,"prefetch_count":10,"consumer_count":1}]`)
		case p == "/api/consumers" && r.Method == "GET":
			writeJSON(w, `[{"queue":{"name":"q1","vhost":"/"},"vhost":"/","channel_details":{"name":"127.0.0.1:50000 -> 127.0.0.1:5672 (1)"},"consumer_tag":"ctag1.0","ack_required":true,"prefetch_count":10,"active":true}]`)
		case p == "/api/health/checks/alarms" && r.Method == "GET":
			writeJSON(w, `{"status":"ok"}`)
		case p == "/api/health/checks/local-alarms" && r.Method == "GET":
			writeJSON(w, `{"status":"ok"}`)
		case p == "/api/aliveness-test/%2F" && r.Method == "GET":
			writeJSON(w, `{"status":"ok"}`)
		case p == "/api/vhosts" && r.Method == "GET":
			writeJSON(w, `[{"name":"/","tracing":false,"messages":124,"messages_ready":122,"messages_unacknowledged":2,"description":"Default virtual host","tags":[]}]`)
		case p == "/api/vhosts/%2F" && r.Method == "GET":
			writeJSON(w, `{"name":"/","tracing":false}`)
		case strings.HasPrefix(p, "/api/vhosts/") && (r.Method == "PUT" || r.Method == "DELETE"):
			s.record(r)
			w.WriteHeader(http.StatusNoContent)
		default:
			// Note: no /api/stream/* handlers — a 404 there exercises the
			// version-gate mapping to UNSUPPORTED_OPERATION.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found","reason":"Not Found"}`))
		}
	})
	s.Server = httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

func (s *rmqServer) addConn(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"rmq", "conn", "add", name, "--url", s.URL, "--username", "admin", "--password", testPassword}, extra...)
	if out, err := runMuxcat(t, args...); err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
}

func TestConnLifecycle(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local", "--set-default")

	// The config file holds only the enc:v1: blob, never the plaintext.
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
	for _, args := range [][]string{{"rmq", "conn", "ls"}, {"rmq", "conn", "show", "local"}} {
		if out, err := runMuxcat(t, args...); err != nil {
			t.Fatalf("%v failed: %v", args, err)
		} else if strings.Contains(out, testPassword) {
			t.Fatalf("%v leaks the plaintext password:\n%s", args, out)
		}
	}

	// conn test: auth check + version probe; the server saw basic auth with
	// the decrypted password.
	env := runJSON(t, "rmq", "conn", "test", "local")
	data := env["data"].(map[string]any)
	if data["ok"] != true || data["user"] != "admin" || data["version"] != "4.3.1" || data["product"] != "RabbitMQ" {
		t.Fatalf("unexpected conn test data: %v", data)
	}
	if user, pw := s.lastAuth(); user != "admin" || pw != testPassword {
		t.Fatalf("server saw auth %q/%q", user, pw)
	}

	// conn rm removes the connection (and its unreferenced instance).
	if out, err := runMuxcat(t, "rmq", "conn", "rm", "local", "--yes"); err != nil {
		t.Fatalf("conn rm failed: %v\n%s", err, out)
	}
	if _, err := runMuxcat(t, "rmq", "conn", "show", "local"); output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("show after rm code = %v", output.ToError(err))
	}
}

func TestConnAddRejectsUserinfoURL(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "rmq", "conn", "add", "bad", "--url", "http://user:pass@127.0.0.1:15672")
	if e := output.ToError(err); e.Code != output.CodeConfigInvalid {
		t.Fatalf("code = %v, want %s", e, output.CodeConfigInvalid)
	}
}

func TestAuthFailedHint(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	if out, err := runMuxcat(t, "rmq", "conn", "add", "bad", "--url", s.URL, "--username", "admin", "--password", "wrong-pw"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_, err := runMuxcat(t, "rmq", "conn", "test", "bad")
	e := output.ToError(err)
	if e.Code != output.CodeAuthFailed {
		t.Fatalf("code = %v, want %s", e, output.CodeAuthFailed)
	}
	if !strings.Contains(e.Hint, "guest") || !strings.Contains(e.Hint, "localhost") {
		t.Fatalf("hint should mention the guest/localhost caveat: %q", e.Hint)
	}
}

func TestOverview(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "rmq", "overview")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"4.3.1", "rabbit@test", "connections", "124"} {
		if !strings.Contains(out, want) {
			t.Fatalf("overview output missing %q:\n%s", want, out)
		}
	}
	env := runJSON(t, "rmq", "overview")
	if env["data"].(map[string]any)["rabbitmq_version"] != "4.3.1" {
		t.Fatalf("overview JSON not raw: %v", env["data"])
	}
}

func TestNodeLsAndShow(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "rmq", "node", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rabbit@test", "disc", "256MiB", "1h1m"} {
		if !strings.Contains(out, want) {
			t.Fatalf("node ls output missing %q:\n%s", want, out)
		}
	}
	env := runJSON(t, "rmq", "node", "show", "rabbit@test")
	if env["data"].(map[string]any)["name"] != "rabbit@test" {
		t.Fatalf("node show JSON unexpected: %v", env["data"])
	}
}

func TestQueueLsLenientParsing(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "rmq", "queue", "ls")
	if err != nil {
		t.Fatal(err)
	}
	// Both shapes parse: the 4.x queue shows its type field, the 3.8 queue
	// (slave_pids, no type) falls back to arguments.x-queue-type, and a
	// queue with neither would read classic.
	for _, want := range []string{"q1", "classic", "legacy-q", "quorum", "events-0", "stream"} {
		if !strings.Contains(out, want) {
			t.Fatalf("queue ls output missing %q:\n%s", want, out)
		}
	}
	// --vhost / encodes to %2F in the path.
	if _, err := runMuxcat(t, "rmq", "queue", "ls", "--vhost", "/"); err != nil {
		t.Fatal(err)
	}
	env := runJSON(t, "rmq", "queue", "ls")
	if len(env["data"].([]any)) != 4 {
		t.Fatalf("queue ls JSON not raw: %v", env["data"])
	}
}

func TestQueueShow(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	env := runJSON(t, "rmq", "queue", "show", "q1", "--vhost", "/")
	data := env["data"].(map[string]any)
	if data["name"] != "q1" || data["type"] != "classic" {
		t.Fatalf("queue show JSON unexpected: %v", data)
	}
}

func TestQueueDeclareRmPurge(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	if _, err := runMuxcat(t, "rmq", "queue", "declare", "new-q", "--type", "quorum", "--args", `{"x-max-length":1000}`); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(s.bodyOf("PUT", "/api/queues/%2F/new-q"), &body); err != nil {
		t.Fatalf("declare body not recorded: %v", err)
	}
	args := body["arguments"].(map[string]any)
	if body["durable"] != true || args["x-queue-type"] != "quorum" || args["x-max-length"].(float64) != 1000 {
		t.Fatalf("declare body unexpected: %v", body)
	}

	// Invalid --type is a usage error and sends nothing.
	if _, err := runMuxcat(t, "rmq", "queue", "declare", "nope", "--type", "bogus"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("bogus type code = %v", output.ToError(err))
	}
	if s.writeCount("PUT", "/api/queues/%2F/nope") != 0 {
		t.Fatal("invalid --type still sent a declare")
	}

	if _, err := runMuxcat(t, "rmq", "queue", "delete", "new-q", "--if-empty", "--if-unused"); err != nil {
		t.Fatal(err)
	}
	q := s.queryOf("DELETE", "/api/queues/%2F/new-q")
	if !strings.Contains(q, "if-empty=true") || !strings.Contains(q, "if-unused=true") {
		t.Fatalf("delete query = %q", q)
	}

	// delete is also reachable via the del/rm aliases.
	if _, err := runMuxcat(t, "rmq", "queue", "del", "new-q"); err != nil {
		t.Fatal(err)
	}
	if _, err := runMuxcat(t, "rmq", "queue", "rm", "new-q"); err != nil {
		t.Fatal(err)
	}
	if s.writeCount("DELETE", "/api/queues/%2F/new-q") != 3 {
		t.Fatal("delete aliases did not reach the endpoint")
	}

	if _, err := runMuxcat(t, "rmq", "queue", "purge", "q1"); err != nil {
		t.Fatal(err)
	}
	if s.writeCount("DELETE", "/api/queues/%2F/q1/contents") != 1 {
		t.Fatal("purge endpoint not called")
	}

	// Server-side reason text rides in the error hint.
	_, err := runMuxcat(t, "rmq", "queue", "declare", "bad")
	e := output.ToError(err)
	if e.Code != output.CodeQueryError || !strings.Contains(e.Hint, "some reason text") {
		t.Fatalf("declare bad error = %v", e)
	}
}

func TestQueueGet(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	// The base64 payload is decoded for display; JSON keeps the raw value.
	out, err := runMuxcat(t, "rmq", "queue", "get", "q1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") || strings.Contains(out, "aGVsbG8=") {
		t.Fatalf("queue get output unexpected:\n%s", out)
	}
	env := runJSON(t, "rmq", "queue", "get", "q1")
	msg := env["data"].([]any)[0].(map[string]any)
	if msg["payload"] != "aGVsbG8=" || msg["payload_encoding"] != "base64" {
		t.Fatalf("queue get JSON not raw: %v", msg)
	}
	var body map[string]any
	if err := json.Unmarshal(s.bodyOf("POST", "/api/queues/%2F/q1/get"), &body); err != nil {
		t.Fatal(err)
	}
	if body["ackmode"] != "ack_requeue_true" || body["count"].(float64) != 1 || body["encoding"] != "auto" {
		t.Fatalf("get body unexpected: %v", body)
	}

	// --limit is capped at 50, --ackmode is an enum.
	if _, err := runMuxcat(t, "rmq", "queue", "get", "q1", "--limit", "99"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("limit 99 code = %v", output.ToError(err))
	}
	if _, err := runMuxcat(t, "rmq", "queue", "get", "q1", "--ackmode", "bogus"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("bogus ackmode code = %v", output.ToError(err))
	}
}

func TestExchangeDeclareAndPublish(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	if _, err := runMuxcat(t, "rmq", "exchange", "declare", "ex2", "--type", "fanout"); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(s.bodyOf("PUT", "/api/exchanges/%2F/ex2"), &body); err != nil {
		t.Fatal(err)
	}
	if body["type"] != "fanout" || body["durable"] != true {
		t.Fatalf("exchange declare body unexpected: %v", body)
	}

	out, err := runMuxcat(t, "rmq", "exchange", "publish", "ex1", "--payload", "hello", "--routing-key", "rk")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "routed: true") {
		t.Fatalf("publish output unexpected:\n%s", out)
	}
	var pbody map[string]any
	if err := json.Unmarshal(s.bodyOf("POST", "/api/exchanges/%2F/ex1/publish"), &pbody); err != nil {
		t.Fatal(err)
	}
	if pbody["payload"] != "hello" || pbody["payload_encoding"] != "string" || pbody["routing_key"] != "rk" {
		t.Fatalf("publish body unexpected: %v", pbody)
	}
	env := runJSON(t, "rmq", "exchange", "publish", "ex1", "--payload", "aGVsbG8=", "--payload-encoding", "base64")
	if env["data"].(map[string]any)["routed"] != true {
		t.Fatalf("publish JSON unexpected: %v", env["data"])
	}
}

func TestBindingLifecycle(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "rmq", "binding", "ls", "--exchange", "ex1", "--queue", "q1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ex1") || !strings.Contains(out, "rk") {
		t.Fatalf("binding ls output unexpected:\n%s", out)
	}

	if _, err := runMuxcat(t, "rmq", "binding", "bind", "--exchange", "ex1", "--queue", "q1", "--routing-key", "rk"); err != nil {
		t.Fatal(err)
	}
	if s.writeCount("POST", "/api/bindings/%2F/e/ex1/q/q1") != 1 {
		t.Fatal("bind endpoint not called")
	}

	if _, err := runMuxcat(t, "rmq", "binding", "unbind", "--exchange", "ex1", "--queue", "q1", "--props", "rk"); err != nil {
		t.Fatal(err)
	}
	if s.writeCount("DELETE", "/api/bindings/%2F/e/ex1/q/q1/rk") != 1 {
		t.Fatal("unbind endpoint not called with the properties key")
	}

	// unbind without --props is a usage error pointing at binding ls.
	_, err = runMuxcat(t, "rmq", "binding", "unbind", "--exchange", "ex1", "--queue", "q1")
	e := output.ToError(err)
	if e.Code != output.CodeMissingArgument || !strings.Contains(e.Hint, "properties_key") {
		t.Fatalf("unbind w/o props error = %v", e)
	}
}

func TestConnectionLsAndClose(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "rmq", "connection", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "127.0.0.1:50000") || !strings.Contains(out, "AMQP 0-9-1") {
		t.Fatalf("connection ls output unexpected:\n%s", out)
	}

	name := "127.0.0.1:50000 -> 127.0.0.1:5672"
	if _, err := runMuxcat(t, "rmq", "connection", "close", name); err != nil {
		t.Fatal(err)
	}
	// The name contains spaces and " -> "; it must be path-escaped (":"
	// is left raw by Go's canonical escaped-path form).
	if s.writeCount("DELETE", "/api/connections/127.0.0.1:50000%20-%3E%20127.0.0.1:5672") != 1 {
		t.Fatalf("close called with unescaped name: %v", s.writes)
	}
}

func TestConsumerAndChannelLs(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "rmq", "consumer", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "q1") || !strings.Contains(out, "ctag1.0") {
		t.Fatalf("consumer ls output unexpected:\n%s", out)
	}
	out, err = runMuxcat(t, "rmq", "channel", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "confirm") {
		t.Fatalf("channel ls output unexpected:\n%s", out)
	}
}

func TestStreamLs(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "rmq", "stream", "ls")
	if err != nil {
		t.Fatal(err)
	}
	// Only stream queues are listed; the partition "events-0" is attributed
	// to super stream "events" by the name heuristic.
	if !strings.Contains(out, "events-0") || !strings.Contains(out, "events") || !strings.Contains(out, "orders") {
		t.Fatalf("stream ls output unexpected:\n%s", out)
	}
	if strings.Contains(out, "legacy-q") {
		t.Fatalf("stream ls listed a non-stream queue:\n%s", out)
	}
}

func TestHealth(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "rmq", "health")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "status: ok") {
		t.Fatalf("health output unexpected:\n%s", out)
	}
	out, err = runMuxcat(t, "rmq", "health", "--aliveness", "--vhost", "/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "status: ok") {
		t.Fatalf("aliveness output unexpected:\n%s", out)
	}
	_, err = runMuxcat(t, "rmq", "health", "bogus-check")
	if output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("bogus check code = %v", output.ToError(err))
	}
}

func TestVersionGated404(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	// 4.x-only health check on a server that 404s it.
	_, err := runMuxcat(t, "rmq", "health", "is-in-service")
	e := output.ToError(err)
	if e.Code != output.CodeUnsupportedOperation || !strings.Contains(e.Hint, "4.0") {
		t.Fatalf("is-in-service 404 error = %v", e)
	}
	// 3.9+ stream endpoint on a server that 404s it.
	_, err = runMuxcat(t, "rmq", "stream", "publisher", "ls")
	e = output.ToError(err)
	if e.Code != output.CodeUnsupportedOperation || !strings.Contains(e.Hint, "3.9") {
		t.Fatalf("stream publishers 404 error = %v", e)
	}
}

func TestRequestPassthrough(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	env := runJSON(t, "rmq", "request", "GET", "/api/whoami")
	data := env["data"].(map[string]any)
	if data["status"].(float64) != 200 || data["body"].(map[string]any)["name"] != "admin" {
		t.Fatalf("GET exchange = %v", data)
	}

	// A completed 404 exchange is reported, not an error.
	env = runJSON(t, "rmq", "request", "GET", "/api/nope")
	if env["ok"] != true || env["data"].(map[string]any)["status"].(float64) != 404 {
		t.Fatalf("404 exchange = %v", env)
	}

	// Invalid method / path rejected as usage errors.
	if _, err := runMuxcat(t, "rmq", "request", "FETCH", "/x"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("invalid method code = %v", output.ToError(err))
	}
	if _, err := runMuxcat(t, "rmq", "request", "GET", "api/overview"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("relative path code = %v", output.ToError(err))
	}
}

func TestWriteReadonly(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "ro", "--readonly")

	cases := [][]string{
		{"rmq", "-c", "ro", "queue", "declare", "x"},
		{"rmq", "-c", "ro", "queue", "delete", "x"},
		{"rmq", "-c", "ro", "queue", "purge", "x"},
		{"rmq", "-c", "ro", "exchange", "declare", "x"},
		{"rmq", "-c", "ro", "exchange", "delete", "x"},
		{"rmq", "-c", "ro", "exchange", "publish", "x", "--payload", "p"},
		{"rmq", "-c", "ro", "binding", "bind", "--exchange", "e", "--queue", "q"},
		{"rmq", "-c", "ro", "binding", "unbind", "--exchange", "e", "--queue", "q", "--props", "p"},
		{"rmq", "-c", "ro", "connection", "close", "a -> b"},
		{"rmq", "-c", "ro", "vhost", "add", "x"},
		{"rmq", "-c", "ro", "vhost", "delete", "x"},
		{"rmq", "-c", "ro", "request", "PUT", "/api/vhosts/x"},
	}
	for _, args := range cases {
		_, err := runMuxcat(t, args...)
		if e := output.ToError(err); e.Code != output.CodeReadonlyViolation {
			t.Fatalf("%v code = %v, want %s", args, e, output.CodeReadonlyViolation)
		}
	}
	// No write request reached the server.
	for key, n := range s.writes {
		if n != 0 {
			t.Fatalf("readonly connection reached %s", key)
		}
	}
	// Reads still work.
	if _, err := runMuxcat(t, "rmq", "-c", "ro", "queue", "ls"); err != nil {
		t.Fatalf("readonly GET should pass: %v", err)
	}
}

func TestConnectFailedAndTimeout(t *testing.T) {
	setupEnv(t)

	// Connection refused → CONNECT_FAILED (exit 3).
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if out, err := runMuxcat(t, "rmq", "conn", "add", "dead", "--url", deadURL); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_, err := runMuxcat(t, "rmq", "-c", "dead", "queue", "ls")
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
		_, _ = w.Write([]byte(`[]`))
	}))
	defer slow.Close()
	if _, err := runMuxcat(t, "rmq", "conn", "add", "slow", "--url", slow.URL, "--timeout", "50ms"); err != nil {
		t.Fatal(err)
	}
	_, err = runMuxcat(t, "rmq", "-c", "slow", "queue", "ls")
	e = output.ToError(err)
	if e.Code != output.CodeTimeout {
		t.Fatalf("code = %v, want %s", e, output.CodeTimeout)
	}
}

func TestClassifyStatus(t *testing.T) {
	// 401/403 map to AUTH_FAILED with the guest/localhost hint.
	for _, status := range []int{401, 403} {
		e := classifyStatus(status, []byte(`{"error":"not_authorised","reason":"Login failed"}`), "/api/queues")
		if e.Code != output.CodeAuthFailed {
			t.Fatalf("%d code = %s, want AUTH_FAILED", status, e.Code)
		}
		if !strings.Contains(e.Hint, "guest") || !strings.Contains(e.Message, "Login failed") {
			t.Fatalf("%d error = %v", status, e)
		}
	}
	// 404 maps to QUERY_ERROR with the reason in the message.
	if e := classifyStatus(404, []byte(`{"error":"not_found","reason":"Not Found"}`), "/api/queues/%2F/nope"); e.Code != output.CodeQueryError || !strings.Contains(e.Message, "not found") {
		t.Fatalf("404 error = %v", e)
	}
	// 404 on a gated endpoint maps to UNSUPPORTED_OPERATION.
	if e := classifyStatus(404, nil, "/api/stream/connections"); e.Code != output.CodeUnsupportedOperation {
		t.Fatalf("gated 404 error = %v", e)
	}
	// Other statuses: the message carries the error tag, the reason goes to
	// the hint only — the same text never appears twice.
	e := classifyStatus(400, []byte(`{"error":"bad_request","reason":"some reason"}`), "/api/queues")
	if e.Code != output.CodeQueryError || e.Message != "HTTP 400: bad_request" || e.Hint != "some reason" {
		t.Fatalf("400 error = %v", e)
	}
	if strings.Contains(e.Message, e.Hint) {
		t.Fatalf("reason duplicated in message: %v", e)
	}
}

func TestEsc(t *testing.T) {
	if got := esc("/"); got != "%2F" {
		t.Fatalf("esc(/) = %q", got)
	}
	if got := esc("a b"); got != "a%20b" {
		t.Fatalf("esc(a b) = %q", got)
	}
}

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{"version":1,"instances":{"local":{"url":"http://127.0.0.1:15672"}},"connections":{"local":{"instance":"local","username":"u","password":"enc:v1:x","readonly":false,"timeout":"5s"}},"defaultConnection":"local"}`)
	if err := schema.Validate(FileName, valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	invalid := []byte(`{"version":1,"instances":{"local":{"url":"http://x","extra":1}}}`)
	if err := schema.Validate(FileName, invalid); err == nil {
		t.Fatal("config with unknown instance field accepted")
	}
	invalidConn := []byte(`{"version":1,"connections":{"local":{"instance":"local","token":"enc:v1:x"}}}`)
	if err := schema.Validate(FileName, invalidConn); err == nil {
		t.Fatal("config with unknown connection field accepted")
	}
}

func TestDisplayPayloadTruncation(t *testing.T) {
	long := strings.Repeat("x", 3000)
	got := displayPayload(map[string]any{"payload": long, "payload_encoding": "string"})
	if !strings.Contains(got, "truncated") || len(got) > 1100 {
		t.Fatalf("truncation marker/length wrong (len %d)", len(got))
	}
	// base64 payloads decode before display.
	got = displayPayload(map[string]any{"payload": "aGVsbG8=", "payload_encoding": "base64"})
	if got != "hello" {
		t.Fatalf("base64 display = %q", got)
	}
}

func TestRequestContentType(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	f := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(f, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Default: a body goes out as application/json.
	env := runJSON(t, "rmq", "request", "POST", "/echo", "--file", f)
	if env["data"].(map[string]any)["body"].(map[string]any)["content_type"] != "application/json" {
		t.Fatalf("default content type = %v", env["data"])
	}
	// --content-type overrides it.
	env = runJSON(t, "rmq", "request", "POST", "/echo", "--file", f, "--content-type", "text/plain")
	if env["data"].(map[string]any)["body"].(map[string]any)["content_type"] != "text/plain" {
		t.Fatalf("content type override lost = %v", env["data"])
	}
}

func TestQueueGetReadonly(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "ro", "--readonly")

	// Default peek (ack_requeue_true) is allowed on readonly connections.
	if _, err := runMuxcat(t, "rmq", "-c", "ro", "queue", "get", "q1"); err != nil {
		t.Fatalf("readonly peek should pass: %v", err)
	}
	if _, err := runMuxcat(t, "rmq", "-c", "ro", "queue", "get", "q1", "--ackmode", "reject_requeue_true"); err != nil {
		t.Fatalf("readonly reject_requeue_true should pass: %v", err)
	}

	// Destructive ackmodes are refused before any request is sent.
	for _, ackmode := range []string{"ack_requeue_false", "reject_requeue_false"} {
		_, err := runMuxcat(t, "rmq", "-c", "ro", "queue", "get", "q1", "--ackmode", ackmode)
		if e := output.ToError(err); e.Code != output.CodeReadonlyViolation {
			t.Fatalf("ackmode %s code = %v, want %s", ackmode, e, output.CodeReadonlyViolation)
		}
	}
	if n := s.writeCount("POST", "/api/queues/%2F/q1/get"); n != 2 {
		t.Fatalf("get endpoint calls = %d, want 2 (the two peeks)", n)
	}
}

func TestVhostAdd(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	// description/tags are only sent when the flags are set.
	if _, err := runMuxcat(t, "rmq", "vhost", "add", "vh1", "--description", "staging env", "--tags", "team-a, team-b"); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(s.bodyOf("PUT", "/api/vhosts/vh1"), &body); err != nil {
		t.Fatalf("vhost add body not recorded: %v", err)
	}
	tags, _ := body["tags"].([]any)
	if body["description"] != "staging env" || len(tags) != 2 || tags[0] != "team-a" || tags[1] != "team-b" {
		t.Fatalf("vhost add body unexpected: %v", body)
	}

	if _, err := runMuxcat(t, "rmq", "vhost", "add", "vh2"); err != nil {
		t.Fatal(err)
	}
	body = map[string]any{}
	if err := json.Unmarshal(s.bodyOf("PUT", "/api/vhosts/vh2"), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["description"]; ok {
		t.Fatalf("description sent without the flag: %v", body)
	}
	if _, ok := body["tags"]; ok {
		t.Fatalf("tags sent without the flag: %v", body)
	}
}

func TestExchangeRmIfUnused(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)
	s.addConn(t, "local")

	if _, err := runMuxcat(t, "rmq", "exchange", "rm", "ex1", "--if-unused"); err != nil {
		t.Fatal(err)
	}
	if q := s.queryOf("DELETE", "/api/exchanges/%2F/ex1"); !strings.Contains(q, "if-unused=true") {
		t.Fatalf("exchange rm query = %q", q)
	}
}

func TestConnAddPasswordWarning(t *testing.T) {
	setupEnv(t)
	s := newRmqServer(t)

	stdout, stderr, err := runMuxcatSE(t, "rmq", "conn", "add", "local",
		"--url", s.URL, "--username", "admin", "--password", testPassword)
	if err != nil {
		t.Fatalf("conn add failed: %v\n%s", err, stdout)
	}
	// The plaintext flag triggers a stderr warning...
	if !strings.Contains(stderr, "Warning: --password") {
		t.Fatalf("stderr warning missing:\n%s", stderr)
	}
	// ...but the plaintext never reaches stdout or the config file.
	if strings.Contains(stdout, testPassword) {
		t.Fatalf("stdout leaks the plaintext password:\n%s", stdout)
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testPassword) {
		t.Fatalf("config leaks the plaintext password:\n%s", raw)
	}
	if !strings.Contains(string(raw), "enc:v1:") {
		t.Fatalf("config does not contain an encrypted password:\n%s", raw)
	}
}
