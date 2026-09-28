package emqx

import (
	"bytes"
	"encoding/base64"
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

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
	"github.com/ravenmk2/muxcat/schema"
)

// runMuxcat executes through the full root command (including persistent
// flags and the registry mounts) and returns stdout+stderr and the error.
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

const (
	testUser      = "admin"
	testPassword  = "s3cr3t-dashboard-pass"
	testAPIKey    = "testapikey000001"
	testAPISecret = "s3cr3t-api-secret"
	testVersion   = "5.8.6"
)

// emqxServer is a fake EMQX 5.x server for tests.
type emqxServer struct {
	*httptest.Server
	mu           sync.Mutex
	loginCalls   int
	tokenSeq     int
	failNext401  bool // fail the next bearer-token request once (401 retry path)
	clients      []map[string]any
	banned       map[string]map[string]any // as\x02who -> entry
	retained     map[string]map[string]any // topic -> message
	kicked       []string
	published    []map[string]any
	bannedWrites int
}

func newEmqxServer(t *testing.T) *emqxServer {
	t.Helper()
	s := &emqxServer{
		banned:   map[string]map[string]any{},
		retained: map[string]map[string]any{},
	}

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	writeErr := func(w http.ResponseWriter, status int, code, message string) {
		writeJSON(w, status, map[string]any{"code": code, "message": message})
	}

	// authOk accepts API key Basic auth or a valid bearer token (dashboard
	// JWT). failNext401 fails one bearer request to exercise the client's
	// 401 re-login retry.
	authOk := func(r *http.Request) bool {
		if u, p, ok := r.BasicAuth(); ok {
			return u == testAPIKey && p == testAPISecret
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || !strings.HasPrefix(tok, "tok-") {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.failNext401 {
			s.failNext401 = false
			return false
		}
		return true
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v5/login", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.loginCalls++
		s.tokenSeq++
		seq := s.tokenSeq
		s.mu.Unlock()
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["username"] == testUser && body["password"] == testPassword {
			writeJSON(w, http.StatusOK, map[string]any{
				"token":   fmt.Sprintf("tok-%d", seq),
				"version": testVersion,
				"license": map[string]any{"edition": "Opensource"},
			})
			return
		}
		writeErr(w, http.StatusUnauthorized, "BAD_USERNAME_OR_PWD", "check your username and password")
	})

	// paginated serves {data, meta:{count, limit, page, hasnext}}.
	paginated := func(w http.ResponseWriter, r *http.Request, items []map[string]any) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		if page < 1 {
			page = 1
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit < 1 {
			limit = 100
		}
		from := (page - 1) * limit
		if from > len(items) {
			from = len(items)
		}
		to := from + limit
		if to > len(items) {
			to = len(items)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": items[from:to],
			"meta": map[string]any{
				"count":   len(items),
				"limit":   limit,
				"page":    page,
				"hasnext": to < len(items),
			},
		})
	}

	mux.HandleFunc("/api/v5/nodes", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{{
			"node": "emqx@127.0.0.1", "version": testVersion, "edition": "Opensource",
			"uptime": 9360000, "node_status": "running", "connections": 3, "live_connections": 3,
		}})
	})
	mux.HandleFunc("/api/v5/status", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		// Real shape: plain text, not JSON.
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Node emqx@127.0.0.1 is started\nemqx is running\n"))
	})
	mux.HandleFunc("/api/v5/monitor_current", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"connections": 3, "live_connections": 3, "subscriptions": 5, "topics": 4})
	})
	mux.HandleFunc("/api/v5/listeners", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		// Real shape: counters live in the nested status object, and
		// max_connections is the string "infinity" when unbounded.
		writeJSON(w, http.StatusOK, []map[string]any{{
			"id": "tcp:default", "type": "tcp", "name": "default", "bind": "1883",
			"status": map[string]any{"running": true, "current_connections": 3, "max_connections": "infinity"},
		}})
	})
	mux.HandleFunc("/api/v5/clients", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		s.mu.Lock()
		items := make([]map[string]any, len(s.clients))
		copy(items, s.clients)
		s.mu.Unlock()
		if f := r.URL.Query().Get("like_clientid"); f != "" {
			filtered := items[:0]
			for _, it := range items {
				if strings.Contains(strOf(it, "clientid"), f) {
					filtered = append(filtered, it)
				}
			}
			items = filtered
		}
		paginated(w, r, items)
	})
	mux.HandleFunc("/api/v5/clients/", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		id, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/v5/clients/"))
		switch r.Method {
		case http.MethodGet:
			for _, c := range s.clients {
				if strOf(c, "clientid") == id {
					writeJSON(w, http.StatusOK, []map[string]any{c})
					return
				}
			}
			writeErr(w, http.StatusNotFound, "CLIENTID_NOT_FOUND", "clientid not found")
		case http.MethodDelete:
			s.mu.Lock()
			s.kicked = append(s.kicked, id)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/v5/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		paginated(w, r, []map[string]any{
			{"clientid": "c-0", "topic": "sensors/#", "qos": 1, "node": "emqx@127.0.0.1"},
		})
	})
	mux.HandleFunc("/api/v5/topics", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		paginated(w, r, []map[string]any{
			{"topic": "sensors/#", "node": "emqx@127.0.0.1"},
		})
	})
	mux.HandleFunc("/api/v5/publish", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.published = append(s.published, body)
		s.mu.Unlock()
		// 202 + no_matching_subscribers is a normal result.
		writeJSON(w, http.StatusAccepted, map[string]any{"message": "no_matching_subscribers", "reason_code": 16})
	})
	mux.HandleFunc("/api/v5/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		// Real shape: one flat object per node, every key except "node" is
		// a counter.
		writeJSON(w, http.StatusOK, []map[string]any{{
			"node":              "emqx@127.0.0.1",
			"messages.received": 42,
			"messages.sent":     40,
		}})
	})
	mux.HandleFunc("/api/v5/stats", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{{
			"node":              "emqx@127.0.0.1",
			"connections.count": 3,
			"topics.count":      4,
		}})
	})
	mux.HandleFunc("/api/v5/alarms", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		paginated(w, r, []map[string]any{})
	})
	mux.HandleFunc("/api/v5/banned", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			items := make([]map[string]any, 0, len(s.banned))
			for _, e := range s.banned {
				items = append(items, e)
			}
			s.mu.Unlock()
			paginated(w, r, items)
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			entry := map[string]any{
				"as": body["as"], "who": body["who"], "by": "muxcat",
				"reason": body["reason"], "at": "2024-01-01T00:00:00Z", "until": "infinity",
			}
			s.mu.Lock()
			s.banned[strOf(body, "as")+"\x02"+strOf(body, "who")] = entry
			s.bannedWrites++
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, entry)
		}
	})
	mux.HandleFunc("/api/v5/banned/", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		if r.Method != http.MethodDelete {
			writeErr(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/v5/banned/"), "/", 2)
		if len(parts) != 2 {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "resource not found")
			return
		}
		as, _ := url.PathUnescape(parts[0])
		who, _ := url.PathUnescape(parts[1])
		s.mu.Lock()
		_, ok := s.banned[as+"\x02"+who]
		delete(s.banned, as+"\x02"+who)
		s.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "banned entry not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v5/mqtt/retainer/messages", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		s.mu.Lock()
		items := make([]map[string]any, 0, len(s.retained))
		for _, m := range s.retained {
			items = append(items, map[string]any{
				"topic": m["topic"], "msgid": m["msgid"],
				"from_clientid": m["from_clientid"], "from_username": m["from_username"],
				"publish_at": m["publish_at"],
			})
		}
		s.mu.Unlock()
		paginated(w, r, items)
	})
	mux.HandleFunc("/api/v5/mqtt/retainer/message/", func(w http.ResponseWriter, r *http.Request) {
		if !authOk(r) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authorized")
			return
		}
		topic, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/v5/mqtt/retainer/message/"))
		s.mu.Lock()
		m, ok := s.retained[topic]
		if r.Method == http.MethodDelete {
			delete(s.retained, topic)
		}
		s.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if !ok {
				writeErr(w, http.StatusNotFound, "NOT_FOUND", "retained message not found")
				return
			}
			writeJSON(w, http.StatusOK, m)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// failNextBearerOnce makes the server answer the next bearer-token
// request with 401, forcing the client into the re-login retry path.
func (s *emqxServer) failNextBearerOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext401 = true
}

func (s *emqxServer) loginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginCalls
}

func (s *emqxServer) addConn(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"emqx", "conn", "add", name, "--url", s.URL}, extra...)
	if out, err := runMuxcat(t, args...); err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
}

func (s *emqxServer) seedClients(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < n; i++ {
		s.clients = append(s.clients, map[string]any{
			"clientid":     fmt.Sprintf("c-%d", i),
			"username":     "sensor",
			"node":         "emqx@127.0.0.1",
			"ip_address":   "10.0.0.1",
			"port":         18830 + i,
			"connected":    true,
			"connected_at": "2024-01-01T00:00:00Z",
		})
	}
}

func rowsOf(t *testing.T, env map[string]any) []any {
	t.Helper()
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope data is not an object: %v", env)
	}
	rows, ok := data["rows"].([]any)
	if !ok {
		t.Fatalf("envelope data has no rows: %v", env)
	}
	return rows
}

func TestAPIKeyPreferredRouting(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	// Both pairs configured: requests must use API key Basic auth and
	// never hit the login endpoint.
	s.addConn(t, "local",
		"--username", testUser, "--password", testPassword,
		"--api-key", testAPIKey, "--api-secret", testAPISecret,
		"--set-default")

	env := runJSON(t, "emqx", "node", "ls")
	if rows := rowsOf(t, env); len(rows) != 1 {
		t.Fatalf("node ls rows = %d, want 1", len(rows))
	}
	if n := s.loginCount(); n != 0 {
		t.Fatalf("login called %d times with an API key configured, want 0", n)
	}
	// conn ls reports the combined auth label.
	env = runJSON(t, "emqx", "conn", "ls")
	rows := rowsOf(t, env)
	if len(rows) != 1 {
		t.Fatalf("conn ls rows = %d", len(rows))
	}
	row, ok := rows[0].([]any)
	if !ok || len(row) < 3 || row[2] != "dashboard+apikey" {
		t.Fatalf("conn ls auth column = %v", rows[0])
	}
	// conn test verifies each configured pair; login happens here.
	env = runJSON(t, "emqx", "conn", "test", "local")
	data := env["data"].(map[string]any)
	if data["ok"] != true || data["version"] != testVersion {
		t.Fatalf("conn test data = %v", data)
	}
	auth := data["auth"].(map[string]any)
	if auth["apiKey"] != "ok" || !strings.Contains(auth["dashboard"].(string), "ok") {
		t.Fatalf("conn test auth = %v", auth)
	}
}

func TestDashboardLazyLoginAndRetry(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	s.addConn(t, "local", "--username", testUser, "--password", testPassword, "--set-default")

	if env := runJSON(t, "emqx", "node", "ls"); env["ok"] != true {
		t.Fatalf("node ls failed: %v", env)
	}
	if n := s.loginCount(); n != 1 {
		t.Fatalf("login count = %d, want 1 (lazy login before the first call)", n)
	}
	// A mid-request 401 (expired/revoked token) triggers one re-login and
	// the request is retried transparently: the command still succeeds.
	s.failNextBearerOnce()
	if env := runJSON(t, "emqx", "node", "ls"); env["ok"] != true {
		t.Fatalf("node ls after a 401 failed: %v", env)
	}
	// Second run: login (fresh client per command run) + re-login retry.
	if n := s.loginCount(); n != 3 {
		t.Fatalf("login count = %d, want 3 (login + login + re-login retry)", n)
	}
}

func TestWrongPasswordNoLeak(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	s.addConn(t, "bad", "--username", testUser, "--password", "wrong-s3cr3t", "--set-default")

	out, err := runMuxcat(t, "emqx", "status")
	if err == nil {
		t.Fatalf("status with wrong password succeeded:\n%s", out)
	}
	e := output.ToError(err)
	if e.Code != output.CodeAuthFailed {
		t.Fatalf("error code = %s, want AUTH_FAILED (%v)", e.Code, e)
	}
	if strings.Contains(e.Message, "wrong-s3cr3t") || strings.Contains(e.Hint, "wrong-s3cr3t") || strings.Contains(out, "wrong-s3cr3t") {
		t.Fatalf("error/output leaks the password: %v\n%s", e, out)
	}
}

func TestReadonlyGuard(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	s.addConn(t, "ro", "--api-key", testAPIKey, "--api-secret", testAPISecret, "--readonly", "--set-default")

	for _, args := range [][]string{
		{"emqx", "pub", "dev/t", "--payload", "hello"},
		{"emqx", "client", "kick", "c-0"},
		{"emqx", "banned", "add", "c-0", "--as", "clientid"},
		{"emqx", "banned", "rm", "c-0", "--as", "clientid"},
		{"emqx", "retained", "rm", "dev/t"},
	} {
		_, err := runMuxcat(t, args...)
		if err == nil {
			t.Fatalf("%v succeeded on a readonly connection", args)
		}
		if e := output.ToError(err); e.Code != output.CodeReadonlyViolation {
			t.Fatalf("%v code = %s, want READONLY_VIOLATION", args, e.Code)
		}
	}
	// Read commands still work on a readonly connection.
	if env := runJSON(t, "emqx", "client", "ls"); env["ok"] != true {
		t.Fatalf("client ls on readonly connection failed: %v", env)
	}
}

func TestPaginationTruncated(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	s.seedClients(250)
	s.addConn(t, "local", "--api-key", testAPIKey, "--api-secret", testAPISecret, "--set-default")

	env := runJSON(t, "emqx", "client", "ls", "--limit", "120")
	if rows := rowsOf(t, env); len(rows) != 120 {
		t.Fatalf("client ls --limit 120 rows = %d, want 120", len(rows))
	}
	metaMap := env["meta"].(map[string]any)
	if metaMap["truncated"] != true {
		t.Fatalf("meta.truncated = %v, want true", metaMap["truncated"])
	}
	// Below the limit: everything fits, no truncation.
	env = runJSON(t, "emqx", "client", "ls", "--limit", "300")
	if rows := rowsOf(t, env); len(rows) != 250 {
		t.Fatalf("client ls --limit 300 rows = %d, want 250", len(rows))
	}
	if env["meta"].(map[string]any)["truncated"] != false {
		t.Fatalf("meta.truncated = %v, want false", env["meta"])
	}
	// Server-side filter passes through.
	env = runJSON(t, "emqx", "client", "ls", "--like-clientid", "c-1", "--limit", "500")
	for _, row := range rowsOf(t, env) {
		if !strings.Contains(row.([]any)[0].(string), "c-1") {
			t.Fatalf("like_clientid filter not applied: %v", row)
		}
	}
}

func TestPublishAcceptedAsData(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	s.addConn(t, "local", "--api-key", testAPIKey, "--api-secret", testAPISecret, "--set-default")

	env := runJSON(t, "emqx", "pub", "dev/t", "--payload", "hello", "--qos", "1")
	if env["ok"] != true {
		t.Fatalf("pub failed: %v", env)
	}
	data := env["data"].(map[string]any)
	if data["message"] != "no_matching_subscribers" || data["reason_code"] != float64(16) {
		t.Fatalf("pub data = %v, want the 202 no_matching_subscribers result as data", data)
	}
	s.mu.Lock()
	n := len(s.published)
	var body map[string]any
	if n > 0 {
		body = s.published[0]
	}
	s.mu.Unlock()
	if n != 1 || body["topic"] != "dev/t" || body["payload"] != "hello" || body["qos"] != float64(1) {
		t.Fatalf("published body = %v (n=%d)", body, n)
	}
}

func TestBannedAddLsRm(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	s.addConn(t, "local", "--api-key", testAPIKey, "--api-secret", testAPISecret, "--set-default")

	env := runJSON(t, "emqx", "banned", "add", "bad-client", "--as", "clientid", "--reason", "spam")
	if env["ok"] != true {
		t.Fatalf("banned add failed: %v", env)
	}
	env = runJSON(t, "emqx", "banned", "ls")
	if rows := rowsOf(t, env); len(rows) != 1 {
		t.Fatalf("banned ls rows = %d, want 1", len(rows))
	}
	env = runJSON(t, "emqx", "banned", "rm", "bad-client", "--as", "clientid")
	if env["ok"] != true {
		t.Fatalf("banned rm failed: %v", env)
	}
	env = runJSON(t, "emqx", "banned", "ls")
	if rows := rowsOf(t, env); len(rows) != 0 {
		t.Fatalf("banned ls rows = %d, want 0", len(rows))
	}
	// Invalid --as value is rejected before any server call.
	if _, err := runMuxcat(t, "emqx", "banned", "add", "x", "--as", "ip"); err == nil {
		t.Fatal("banned add --as ip accepted")
	}
}

func TestRetainedShowTruncation(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	s.addConn(t, "local", "--api-key", testAPIKey, "--api-secret", testAPISecret, "--set-default")
	s.mu.Lock()
	// EMQX returns payloads base64-encoded.
	s.retained["dev/cfg"] = map[string]any{
		"topic": "dev/cfg", "msgid": "0001", "qos": 0, "retain": true,
		"payload": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 5000))), "from_clientid": "c-0",
		"from_username": "sensor", "publish_at": "2024-01-01T00:00:00Z",
	}
	s.retained["dev/text"] = map[string]any{
		"topic": "dev/text", "msgid": "0002", "qos": 0, "retain": true,
		"payload": base64.StdEncoding.EncodeToString([]byte("hello-e2e")), "from_clientid": "c-0",
		"from_username": "sensor", "publish_at": "2024-01-01T00:00:00Z",
	}
	s.mu.Unlock()

	env := runJSON(t, "emqx", "retained", "show", "dev/cfg")
	data := env["data"].(map[string]any)
	payload, _ := data["payload"].(string)
	if len(payload) != retainedPayloadMax {
		t.Fatalf("payload length = %d, want %d", len(payload), retainedPayloadMax)
	}
	if data["payload_truncated"] != true {
		t.Fatalf("payload_truncated = %v", data["payload_truncated"])
	}

	// A UTF-8 payload is decoded from base64 for display.
	env = runJSON(t, "emqx", "retained", "show", "dev/text")
	data = env["data"].(map[string]any)
	if data["payload"] != "hello-e2e" {
		t.Fatalf("payload = %v, want decoded hello-e2e", data["payload"])
	}
}

func TestNoCredentialLeakAcrossCommands(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)
	s.seedClients(2)
	s.addConn(t, "local",
		"--username", testUser, "--password", testPassword,
		"--api-key", testAPIKey, "--api-secret", testAPISecret,
		"--set-default")

	// The config file holds only enc:v1: blobs, never the plaintext.
	raw, err := os.ReadFile(filepath.Join(os.Getenv("MUXCAT_HOME"), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testPassword) || strings.Contains(string(raw), testAPISecret) {
		t.Fatalf("config file leaks plaintext credentials:\n%s", raw)
	}

	for _, args := range [][]string{
		{"emqx", "conn", "ls"},
		{"emqx", "conn", "show", "local"},
		{"emqx", "conn", "test", "local"},
		{"emqx", "status"},
		{"emqx", "node", "ls"},
		{"emqx", "listener", "ls"},
		{"emqx", "client", "ls"},
		{"emqx", "sub", "ls"},
		{"emqx", "topic", "ls"},
		{"emqx", "metric", "ls"},
		{"emqx", "alarm", "ls"},
		{"emqx", "banned", "ls"},
	} {
		out, err := runMuxcat(t, args...)
		if err != nil {
			t.Fatalf("%v failed: %v", args, err)
		}
		for _, cred := range []string{testPassword, testAPISecret} {
			if strings.Contains(out, cred) {
				t.Fatalf("%v leaks credentials:\n%s", args, out)
			}
		}
		if strings.Contains(out, "tok-") {
			t.Fatalf("%v leaks the login token:\n%s", args, out)
		}
		env := runJSON(t, args...)
		if str := fmt.Sprint(env); strings.Contains(str, testPassword) || strings.Contains(str, testAPISecret) {
			t.Fatalf("%v --json leaks credentials: %v", args, env)
		}
	}
}

func TestConnAddValidation(t *testing.T) {
	setupEnv(t)
	s := newEmqxServer(t)

	// Non-TTY without --url: MISSING_ARGUMENT.
	if _, err := runMuxcat(t, "emqx", "conn", "add", "x"); err == nil {
		t.Fatal("conn add without --url accepted")
	} else if e := output.ToError(err); e.Code != output.CodeMissingArgument {
		t.Fatalf("code = %s, want MISSING_ARGUMENT", e.Code)
	}
	// No credential pair at all.
	if _, err := runMuxcat(t, "emqx", "conn", "add", "x", "--url", s.URL); err == nil {
		t.Fatal("conn add without credentials accepted")
	} else if e := output.ToError(err); e.Code != output.CodeConfigInvalid {
		t.Fatalf("code = %s, want CONFIG_INVALID", e.Code)
	}
	// Incomplete dashboard pair.
	if _, err := runMuxcat(t, "emqx", "conn", "add", "x", "--url", s.URL, "--username", testUser); err == nil {
		t.Fatal("conn add with username but no password accepted")
	}
	// Incomplete apikey pair.
	if _, err := runMuxcat(t, "emqx", "conn", "add", "x", "--url", s.URL, "--api-key", testAPIKey); err == nil {
		t.Fatal("conn add with apiKey but no apiSecret accepted")
	}
	// URL with embedded credentials is rejected.
	if _, err := runMuxcat(t, "emqx", "conn", "add", "x", "--url", "http://u:p@host:18083", "--username", testUser, "--password", testPassword); err == nil {
		t.Fatal("conn add with userinfo URL accepted")
	}
}

func TestSchemaValidation(t *testing.T) {
	// Valid: dashboard pair only.
	valid := []byte(`{"version":1,"instances":{"local":{"url":"http://127.0.0.1:18083"}},"connections":{"local":{"instance":"local","username":"u","password":"enc:v1:x","readonly":false,"timeout":"5s"}},"defaultConnection":"local"}`)
	if err := schema.Validate(FileName, valid); err != nil {
		t.Fatalf("valid dashboard-pair config rejected: %v", err)
	}
	// Valid: apikey pair only.
	validKey := []byte(`{"version":1,"instances":{"local":{"url":"http://127.0.0.1:18083"}},"connections":{"local":{"instance":"local","apiKey":"k","apiSecret":"enc:v1:x"}},"defaultConnection":"local"}`)
	if err := schema.Validate(FileName, validKey); err != nil {
		t.Fatalf("valid apikey-pair config rejected: %v", err)
	}
	// Valid: both pairs.
	validBoth := []byte(`{"version":1,"instances":{"local":{"url":"http://x"}},"connections":{"local":{"instance":"local","username":"u","password":"enc:v1:x","apiKey":"k","apiSecret":"enc:v1:y"}}}`)
	if err := schema.Validate(FileName, validBoth); err != nil {
		t.Fatalf("valid dual-credential config rejected: %v", err)
	}
	// Invalid: no credential pair at all (anyOf fails).
	noCreds := []byte(`{"version":1,"instances":{"local":{"url":"http://x"}},"connections":{"local":{"instance":"local"}}}`)
	if err := schema.Validate(FileName, noCreds); err == nil {
		t.Fatal("config without any credential pair accepted")
	}
	// Invalid: username without password (dependentRequired fails).
	halfDash := []byte(`{"version":1,"connections":{"local":{"instance":"local","username":"u"}}}`)
	if err := schema.Validate(FileName, halfDash); err == nil {
		t.Fatal("config with username but no password accepted")
	}
	// Invalid: apiKey without apiSecret.
	halfKey := []byte(`{"version":1,"connections":{"local":{"instance":"local","apiKey":"k"}}}`)
	if err := schema.Validate(FileName, halfKey); err == nil {
		t.Fatal("config with apiKey but no apiSecret accepted")
	}
	// Invalid: unknown connection field.
	unknown := []byte(`{"version":1,"connections":{"local":{"instance":"local","username":"u","password":"enc:v1:x","token":"enc:v1:x"}}}`)
	if err := schema.Validate(FileName, unknown); err == nil {
		t.Fatal("config with unknown connection field accepted")
	}
	// Invalid: instance without url.
	noURL := []byte(`{"version":1,"instances":{"local":{}}}`)
	if err := schema.Validate(FileName, noURL); err == nil {
		t.Fatal("config with url-less instance accepted")
	}
}
