package nacos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	testUser     = "nacos"
	testPassword = "s3cr3t-nacos-pass"
	v2Token      = "v2-token-abc"
	v3Token      = "v3-token-xyz"
)

// ncServer is a fake Nacos server for tests; flavor picks the 2.x or 3.x
// API surface.
type ncServer struct {
	*httptest.Server
	mu            sync.Mutex
	flavor        int
	token         string
	configs       map[string]string // dataId\x02group\x02ns -> content
	types         map[string]string // dataId\x02group\x02ns -> config type
	lastAuth      string
	published     map[string]int
	deleted       map[string]int
	listenerCalls int
}

func configKey(dataID, group, ns string) string { return dataID + "\x02" + group + "\x02" + ns }

// wildcardMatch mirrors the server's blur search: * matches any run.
func wildcardMatch(pattern, s string) bool {
	if pattern == "*" || pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, p := range parts {
		if p == "" {
			continue
		}
		idx := strings.Index(s[pos:], p)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 { // anchored prefix
			return false
		}
		pos += idx + len(p)
	}
	if last := parts[len(parts)-1]; last != "" && !strings.HasSuffix(s, last) {
		return false
	}
	return true
}

// fakeConfigType mirrors the server's type resolution on publish: the
// explicit type wins, otherwise it is inferred from the dataId suffix,
// defaulting to text.
func fakeConfigType(explicit, dataID string) string {
	if explicit != "" {
		return explicit
	}
	if f := inferFormat(dataID); f != "" {
		return f
	}
	return "text"
}

func newNcServer(t *testing.T, flavor int) *ncServer {
	t.Helper()
	s := &ncServer{
		flavor:    flavor,
		configs:   map[string]string{},
		types:     map[string]string{},
		published: map[string]int{},
		deleted:   map[string]int{},
	}
	if flavor == 3 {
		s.token = v3Token
	} else {
		s.token = v2Token
	}
	s.configs[configKey("seed.yaml", "DEFAULT_GROUP", "public")] = "seed: true"
	s.types[configKey("seed.yaml", "DEFAULT_GROUP", "public")] = "yaml"

	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		s.lastAuth = got
		s.mu.Unlock()
		if got != s.token {
			w.WriteHeader(http.StatusForbidden)
			if s.flavor == 3 {
				_, _ = w.Write([]byte(`{"code":10001,"message":"access denied","data":null}`))
			} else {
				_, _ = w.Write([]byte(`{"timestamp":1700000000000,"status":403,"error":"Forbidden","message":"unknown user!"}`))
			}
			return false
		}
		return true
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux := http.NewServeMux()

	// --- version detection -------------------------------------------------
	mux.HandleFunc("/nacos/v3/admin/core/state", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.lastAuth = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Unlock()
		if s.flavor != 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"version": "3.1.0", "standalone_mode": "standalone"})
	})

	// --- login --------------------------------------------------------------
	login := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("username") == testUser && r.Form.Get("password") == testPassword {
			writeJSON(w, map[string]any{"accessToken": s.token, "tokenTtl": 18000, "globalAdmin": true, "username": testUser})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		if s.flavor == 3 {
			_, _ = w.Write([]byte(`{"timestamp":1700000000000,"status":403,"error":"Forbidden","message":"User not found!"}`))
		} else {
			_, _ = w.Write([]byte(`{"code":10001,"message":"User not found!","data":null}`))
		}
	}
	mux.HandleFunc("/nacos/v1/auth/login", login)
	mux.HandleFunc("/nacos/v3/auth/user/login", login)

	// --- config get ----------------------------------------------------------
	getConfig := func(w http.ResponseWriter, dataID, group, ns string, envelope bool) {
		content, ok := s.configs[configKey(dataID, group, ns)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":20004,"message":"resource not found","data":"config data not exist"}`))
			return
		}
		if envelope {
			writeJSON(w, map[string]any{"code": 0, "message": "success", "data": content})
			return
		}
		_, _ = w.Write([]byte(content))
	}
	mux.HandleFunc("/nacos/v2/cs/config", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_ = r.ParseForm()
		dataID, group, ns := r.Form.Get("dataId"), r.Form.Get("group"), r.Form.Get("namespaceId")
		switch r.Method {
		case http.MethodGet:
			getConfig(w, dataID, group, ns2Display(ns), true)
		case http.MethodPost:
			s.mu.Lock()
			s.configs[configKey(dataID, group, ns2Display(ns))] = r.Form.Get("content")
			s.types[configKey(dataID, group, ns2Display(ns))] = fakeConfigType(r.Form.Get("type"), dataID)
			s.published[configKey(dataID, group, ns2Display(ns))]++
			s.mu.Unlock()
			writeJSON(w, map[string]any{"code": 0, "message": "success", "data": true})
		case http.MethodDelete:
			s.mu.Lock()
			delete(s.configs, configKey(dataID, group, ns2Display(ns)))
			delete(s.types, configKey(dataID, group, ns2Display(ns)))
			s.deleted[configKey(dataID, group, ns2Display(ns))]++
			s.mu.Unlock()
			writeJSON(w, map[string]any{"code": 0, "message": "success", "data": true})
		}
	})
	mux.HandleFunc("/nacos/v3/client/cs/config", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("group") != "" { // 3.x rejects the 2.x parameter name
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// 3.x: data is an object carrying the content; a missing config is
		// HTTP 200 with envelope code 20004.
		key := configKey(q.Get("dataId"), q.Get("groupName"), q.Get("namespaceId"))
		content, ok := s.configs[key]
		if !ok {
			writeJSON(w, map[string]any{"code": 20004, "message": "resource not found", "data": nil})
			return
		}
		ctype := s.types[key]
		if ctype == "" {
			ctype = "text"
		}
		writeJSON(w, map[string]any{"code": 0, "message": "success", "data": map[string]any{
			"resultCode": 200, "errorCode": 0, "content": content, "contentType": ctype,
		}})
	})
	mux.HandleFunc("/nacos/v3/admin/cs/config", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("group") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		dataID, group, ns := r.Form.Get("dataId"), r.Form.Get("groupName"), r.Form.Get("namespaceId")
		switch r.Method {
		case http.MethodPost:
			s.mu.Lock()
			s.configs[configKey(dataID, group, ns)] = r.Form.Get("content")
			s.types[configKey(dataID, group, ns)] = fakeConfigType(r.Form.Get("type"), dataID)
			s.published[configKey(dataID, group, ns)]++
			s.mu.Unlock()
			writeJSON(w, map[string]any{"code": 0, "message": "success", "data": true})
		case http.MethodDelete:
			s.mu.Lock()
			delete(s.configs, configKey(dataID, group, ns))
			delete(s.types, configKey(dataID, group, ns))
			s.deleted[configKey(dataID, group, ns)]++
			s.mu.Unlock()
			writeJSON(w, map[string]any{"code": 0, "message": "success", "data": true})
		}
	})

	// --- config list ----------------------------------------------------------
	listConfigs := func(w http.ResponseWriter, r *http.Request, v3 bool) {
		q := r.URL.Query()
		ns := q.Get("tenant")
		groupFilter, dataIDFilter := q.Get("group"), q.Get("dataId")
		if v3 {
			ns, groupFilter = q.Get("namespaceId"), q.Get("groupName")
			if ns == "" { // the admin side misbehaves on an empty namespace
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		var items []map[string]any
		for k := range s.configs {
			parts := strings.Split(k, "\x02")
			if ns2Display(ns) != parts[2] {
				continue
			}
			if dataIDFilter != "" && !wildcardMatch(dataIDFilter, parts[0]) {
				continue
			}
			if groupFilter != "" && !wildcardMatch(groupFilter, parts[1]) {
				continue
			}
			if v3 {
				items = append(items, map[string]any{"dataId": parts[0], "groupName": parts[1], "namespaceId": parts[2], "type": s.types[k]})
			} else {
				items = append(items, map[string]any{"dataId": parts[0], "group": parts[1], "tenant": parts[2], "type": s.types[k]})
			}
		}
		data := map[string]any{"totalCount": len(items), "pageNumber": 1, "pagesAvailable": 1, "pageItems": items}
		if v3 {
			writeJSON(w, map[string]any{"code": 0, "message": "success", "data": data})
			return
		}
		writeJSON(w, data)
	}
	mux.HandleFunc("/nacos/v1/cs/configs", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		// The v1 endpoint is both single-config get (bare text) and list
		// (search param present), mirroring the real server.
		if r.URL.Query().Get("search") == "" {
			q := r.URL.Query()
			getConfig(w, q.Get("dataId"), q.Get("group"), ns2Display(q.Get("tenant")), false)
			return
		}
		listConfigs(w, r, false)
	})
	mux.HandleFunc("/nacos/v3/admin/cs/config/list", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		listConfigs(w, r, true)
	})

	// --- listener (watch) ------------------------------------------------------
	mux.HandleFunc("/nacos/v1/cs/configs/listener", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		s.mu.Lock()
		s.listenerCalls++
		calls := s.listenerCalls
		s.mu.Unlock()
		if calls > 5 { // bound the watch loop in tests
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("listener boom"))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(raw))
		key := v.Get("Listening-Configs")
		parts := strings.Split(strings.TrimSuffix(key, "\x01"), "\x02")
		if len(parts) >= 4 {
			cur := md5Hex(s.configs[configKey(parts[0], parts[1], ns2Display(parts[3]))])
			if cur != parts[2] {
				_, _ = w.Write([]byte(parts[0] + "\x02" + parts[1] + "\x02" + parts[3] + "\x01"))
				return
			}
		}
		// No change: the real server holds for 30s; the fake answers at
		// once so watch unit tests do not block.
	})

	// --- naming ---------------------------------------------------------------
	mux.HandleFunc("/nacos/v1/ns/service/list", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, map[string]any{"count": 2, "doms": []string{"order-service", "pay-service"}})
	})
	mux.HandleFunc("/nacos/v1/ns/service", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, map[string]any{"name": "order-service", "groupName": "DEFAULT_GROUP", "protectThreshold": 0, "metadata": map[string]any{}})
	})
	mux.HandleFunc("/nacos/v1/ns/instance/list", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, map[string]any{"hosts": []map[string]any{
			{"ip": "10.0.0.1", "port": 8080, "weight": 1.0, "healthy": true, "enabled": true},
			{"ip": "10.0.0.2", "port": 8080, "weight": 2.0, "healthy": false, "enabled": true},
		}})
	})
	mux.HandleFunc("/nacos/v3/admin/ns/service/list", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		if r.URL.Query().Get("namespaceId") == "" { // empty namespace misbehaves on the admin side
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"code": 0, "message": "success", "data": map[string]any{
			"count":       1,
			"serviceList": []map[string]any{{"name": "order-service", "groupName": "DEFAULT_GROUP"}},
		}})
	})
	mux.HandleFunc("/nacos/v3/admin/ns/service", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, map[string]any{"code": 0, "message": "success", "data": map[string]any{"name": "order-service", "groupName": "DEFAULT_GROUP"}})
	})
	mux.HandleFunc("/nacos/v3/client/ns/instance/list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "message": "success", "data": map[string]any{"hosts": []map[string]any{
			{"ip": "10.0.0.9", "port": 9090, "weight": 1.0, "healthy": true, "enabled": true},
		}}})
	})

	// --- namespaces -------------------------------------------------------------
	namespaces := []map[string]any{
		{"namespace": "", "namespaceShowName": "public", "quota": 200, "configCount": 1},
		{"namespace": "staging", "namespaceShowName": "staging", "quota": 200, "configCount": 3},
	}
	mux.HandleFunc("/nacos/v1/console/namespaces", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		// 2.x wraps the console namespaces answer in a code:200 envelope.
		writeJSON(w, map[string]any{"code": 200, "message": nil, "data": namespaces})
	})
	mux.HandleFunc("/nacos/v3/admin/core/namespace/list", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, map[string]any{"code": 0, "message": "success", "data": namespaces})
	})
	mux.HandleFunc("/nacos/v1/console/server/state", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, map[string]any{"version": "2.5.1"})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *ncServer) lastBearer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuth
}

func (s *ncServer) publishCount(dataID, group, ns string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.published[configKey(dataID, group, ns)]
}

func (s *ncServer) deleteCount(dataID, group, ns string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted[configKey(dataID, group, ns)]
}

func (s *ncServer) addConn(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"nacos", "conn", "add", name, "--url", s.URL}, extra...)
	if out, err := runMuxcat(t, args...); err != nil {
		t.Fatalf("conn add %s failed: %v\n%s", name, err, out)
	}
}

func TestConnLifecycle(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 2)
	s.addConn(t, "local", "--username", testUser, "--password", testPassword, "--set-default")

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
	for _, args := range [][]string{{"nacos", "conn", "ls"}, {"nacos", "conn", "show", "local"}} {
		if out, err := runMuxcat(t, args...); err != nil {
			t.Fatalf("%v failed: %v", args, err)
		} else if strings.Contains(out, testPassword) {
			t.Fatalf("%v leaks the plaintext password:\n%s", args, out)
		}
	}

	// conn test: version detection + login, and the server saw the bearer
	// token.
	env := runJSON(t, "nacos", "conn", "test", "local")
	data := env["data"].(map[string]any)
	if data["ok"] != true || data["api_version"] != "v2" || data["auth"] != "authenticated as nacos" {
		t.Fatalf("unexpected conn test data: %v", data)
	}
	if data["server_version"] != "2.5.1" {
		t.Fatalf("server_version = %v", data)
	}
	if got := s.lastBearer(); got != v2Token {
		t.Fatalf("server saw bearer %q, want %q", got, v2Token)
	}

	// conn rm removes the connection (and its unreferenced instance).
	if out, err := runMuxcat(t, "nacos", "conn", "rm", "local", "--yes"); err != nil {
		t.Fatalf("conn rm failed: %v\n%s", err, out)
	}
	if _, err := runMuxcat(t, "nacos", "conn", "show", "local"); output.ToError(err).Code != output.CodeConnNotFound {
		t.Fatalf("show after rm code = %v", output.ToError(err))
	}
}

func TestConnAddRejectsUserinfoURL(t *testing.T) {
	setupEnv(t)
	_, err := runMuxcat(t, "nacos", "conn", "add", "bad", "--url", "http://user:pass@127.0.0.1:8848")
	if e := output.ToError(err); e.Code != output.CodeConfigInvalid {
		t.Fatalf("code = %v, want %s", e, output.CodeConfigInvalid)
	}
}

func TestConnAddRejectsBadVersion(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 2)
	_, err := runMuxcat(t, "nacos", "conn", "add", "bad", "--url", s.URL, "--version", "4")
	if e := output.ToError(err); e.Code != output.CodeConfigInvalid {
		t.Fatalf("code = %v, want %s", e, output.CodeConfigInvalid)
	}
}

func TestVersionAutoDetectV3(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 3)
	s.addConn(t, "v3", "--username", testUser, "--password", testPassword)

	env := runJSON(t, "nacos", "conn", "test", "v3")
	data := env["data"].(map[string]any)
	if data["api_version"] != "v3" || data["server_version"] != "3.1.0" {
		t.Fatalf("unexpected conn test data: %v", data)
	}
	if got := s.lastBearer(); got != v3Token {
		t.Fatalf("server saw bearer %q, want %q", got, v3Token)
	}
}

func TestAuthFailure(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 2)
	s.addConn(t, "bad", "--username", testUser, "--password", "wrong-password")

	_, err := runMuxcat(t, "nacos", "conn", "test", "bad")
	e := output.ToError(err)
	if e.Code != output.CodeAuthFailed {
		t.Fatalf("code = %v, want %s", e, output.CodeAuthFailed)
	}
	if got := output.ExitCode(e); got != output.ExitAuth {
		t.Fatalf("exit = %d, want %d", got, output.ExitAuth)
	}
	// The misleading server text must not become the classification; the
	// password never appears in the error.
	if strings.Contains(e.Message, "wrong-password") || strings.Contains(e.Hint, "wrong-password") {
		t.Fatalf("error leaks the password: %v", e)
	}
}

func TestConfigCRUDV2(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 2)
	s.addConn(t, "local", "--username", testUser, "--password", testPassword)

	// publish → get reads back the same content (bare in text mode).
	content := "app:\n  port: 8080\n"
	if out, err := runMuxcat(t, "nacos", "config", "publish", "app.yaml", "--content", content); err != nil {
		t.Fatalf("publish failed: %v\n%s", err, out)
	}
	if s.publishCount("app.yaml", "DEFAULT_GROUP", "public") != 1 {
		t.Fatalf("publish did not reach the server: %v", s.published)
	}
	out, err := runMuxcat(t, "nacos", "config", "get", "app.yaml")
	if err != nil {
		t.Fatalf("get failed: %v\n%s", err, out)
	}
	if strings.TrimRight(out, "\n") != strings.TrimRight(content, "\n") {
		t.Fatalf("get content mismatch:\n%s", out)
	}
	env := runJSON(t, "nacos", "config", "get", "app.yaml")
	data := env["data"].(map[string]any)
	if data["dataId"] != "app.yaml" || data["group"] != "DEFAULT_GROUP" || data["namespace"] != "public" || data["content"] != content {
		t.Fatalf("get JSON unexpected: %v", data)
	}
	// 2.x get carries no server-side type; the client infers it from the
	// dataId suffix.
	if data["type"] != "yaml" {
		t.Fatalf("get JSON type = %v, want inferred yaml", data["type"])
	}

	// ls lists the published config with its type column.
	out, err = runMuxcat(t, "nacos", "config", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "app.yaml") || !strings.Contains(out, "seed.yaml") {
		t.Fatalf("config ls output unexpected:\n%s", out)
	}
	if !strings.Contains(out, "type") || !strings.Contains(out, "yaml") {
		t.Fatalf("config ls should show the type column:\n%s", out)
	}
	// --dataId filters (blur).
	out, err = runMuxcat(t, "nacos", "config", "ls", "--dataId", "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "app.yaml") || strings.Contains(out, "seed.yaml") {
		t.Fatalf("config ls --dataId output unexpected:\n%s", out)
	}

	// delete, then get 404s with the addressing hint.
	if _, err := runMuxcat(t, "nacos", "config", "delete", "app.yaml"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if s.deleteCount("app.yaml", "DEFAULT_GROUP", "public") != 1 {
		t.Fatal("delete did not reach the server")
	}
	_, err = runMuxcat(t, "nacos", "config", "get", "app.yaml")
	e := output.ToError(err)
	if e.Code != output.CodeQueryError || !strings.Contains(e.Message, "not found") {
		t.Fatalf("get after delete error = %v", e)
	}
	if !strings.Contains(e.Hint, "dataId") {
		t.Fatalf("404 hint should address the triple: %q", e.Hint)
	}
}

func TestConfigCRUDV3(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 3)
	s.addConn(t, "v3", "--username", testUser, "--password", testPassword)

	content := `{"feature": true}`
	if _, err := runMuxcat(t, "nacos", "config", "publish", "app.json", "--content", content, "--type", "json"); err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if s.publishCount("app.json", "DEFAULT_GROUP", "public") != 1 {
		t.Fatal("publish did not reach the 3.x admin endpoint")
	}
	env := runJSON(t, "nacos", "config", "get", "app.json")
	envData := env["data"].(map[string]any)
	if envData["content"] != content {
		t.Fatalf("get JSON unexpected: %v", env["data"])
	}
	// The 3.x server-reported contentType reaches the output.
	if envData["type"] != "json" {
		t.Fatalf("get JSON type = %v, want server-reported json", envData["type"])
	}

	out, err := runMuxcat(t, "nacos", "config", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "app.json") || !strings.Contains(out, "seed.yaml") {
		t.Fatalf("config ls output unexpected:\n%s", out)
	}
	if !strings.Contains(out, "json") || !strings.Contains(out, "yaml") {
		t.Fatalf("config ls should show item types:\n%s", out)
	}

	if _, err := runMuxcat(t, "nacos", "config", "delete", "app.json"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if s.deleteCount("app.json", "DEFAULT_GROUP", "public") != 1 {
		t.Fatal("delete did not reach the server")
	}
}

func TestConfigGetFormatAndNoHighlight(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 3)
	s.addConn(t, "v3", "--username", testUser, "--password", testPassword)

	// seed.yaml: the server reports yaml. --no-highlight still prints the
	// bare content (coloring is TTY-only and off in tests regardless).
	out, err := runMuxcat(t, "nacos", "config", "get", "seed.yaml", "--no-highlight")
	if err != nil {
		t.Fatalf("get --no-highlight failed: %v\n%s", err, out)
	}
	if strings.TrimRight(out, "\n") != "seed: true" {
		t.Fatalf("get --no-highlight output unexpected:\n%s", out)
	}
}

func TestFormatHelpers(t *testing.T) {
	for _, tc := range []struct {
		dataID, format, lexer string
	}{
		{"app.yaml", "yaml", "yaml"},
		{"app.yml", "yaml", "yaml"},
		{"app.json", "json", "json"},
		{"app.xml", "xml", "xml"},
		{"app.html", "html", "html"},
		{"app.properties", "properties", "ini"},
		{"app.toml", "toml", "toml"},
		{"app.txt", "text", ""},
		{"no-suffix", "", ""},
	} {
		if f := inferFormat(tc.dataID); f != tc.format {
			t.Errorf("inferFormat(%q) = %q, want %q", tc.dataID, f, tc.format)
		}
		if l := highlightLexer(tc.format); l != tc.lexer {
			t.Errorf("highlightLexer(%q) = %q, want %q", tc.format, l, tc.lexer)
		}
	}
}

func TestServiceInstanceNamespace(t *testing.T) {
	setupEnv(t)

	// 2.x shapes.
	s2 := newNcServer(t, 2)
	s2.addConn(t, "v2", "--username", testUser, "--password", testPassword)
	out, err := runMuxcat(t, "nacos", "service", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "order-service") || !strings.Contains(out, "pay-service") {
		t.Fatalf("service ls output unexpected:\n%s", out)
	}
	out, err = runMuxcat(t, "nacos", "instance", "ls", "order-service")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"10.0.0.1", "8080", "10.0.0.2", "true", "false"} {
		if !strings.Contains(out, want) {
			t.Fatalf("instance ls output missing %q:\n%s", want, out)
		}
	}
	out, err = runMuxcat(t, "nacos", "namespace", "ls")
	if err != nil {
		t.Fatal(err)
	}
	// The empty namespace id is displayed as public.
	if !strings.Contains(out, "public") || !strings.Contains(out, "staging") {
		t.Fatalf("namespace ls output unexpected:\n%s", out)
	}
	if _, err := runMuxcat(t, "nacos", "service", "show", "order-service"); err != nil {
		t.Fatal(err)
	}
}

func TestServiceInstanceNamespaceV3(t *testing.T) {
	setupEnv(t)
	s3 := newNcServer(t, 3)
	s3.addConn(t, "v3", "--username", testUser, "--password", testPassword)

	out, err := runMuxcat(t, "nacos", "service", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "order-service") || !strings.Contains(out, "DEFAULT_GROUP") {
		t.Fatalf("service ls output unexpected:\n%s", out)
	}
	out, err = runMuxcat(t, "nacos", "instance", "ls", "order-service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "10.0.0.9") || !strings.Contains(out, "9090") {
		t.Fatalf("instance ls output unexpected:\n%s", out)
	}
	out, err = runMuxcat(t, "nacos", "namespace", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "staging") {
		t.Fatalf("namespace ls output unexpected:\n%s", out)
	}
}

func TestNamespaceNsAlias(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 3)
	s.addConn(t, "v3", "--username", testUser, "--password", testPassword)

	// --ns is an alias of --namespace on data commands.
	if _, err := runMuxcat(t, "nacos", "config", "publish", "nsapp.yaml", "--content", "k: v", "--ns", "staging"); err != nil {
		t.Fatal(err)
	}
	if s.published[configKey("nsapp.yaml", "DEFAULT_GROUP", "staging")] != 1 {
		t.Fatalf("--ns did not reach the server as the namespace: %v", s.published)
	}
	out, err := runMuxcat(t, "nacos", "config", "get", "nsapp.yaml", "--ns", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "k: v") {
		t.Fatalf("get with --ns returned unexpected output:\n%s", out)
	}
	// conn add accepts --ns as well: the connection's working namespace is
	// used when a data command passes no namespace flag.
	s.addConn(t, "nsconn", "--username", testUser, "--password", testPassword, "--ns", "staging")
	if _, err := runMuxcat(t, "nacos", "config", "get", "nsapp.yaml", "-c", "nsconn"); err != nil {
		t.Fatalf("get via a --ns connection should pass: %v", err)
	}
	// service ls gained namespace flags; --ns must be forwarded (the fake
	// admin side rejects an empty namespaceId).
	if _, err := runMuxcat(t, "nacos", "service", "ls", "--ns", "staging"); err != nil {
		t.Fatalf("service ls --ns should pass: %v", err)
	}
	// --namespace wins over --ns when both are set.
	if _, err := runMuxcat(t, "nacos", "config", "delete", "nsapp.yaml", "--ns", "ignored", "--namespace", "staging"); err != nil {
		t.Fatal(err)
	}
	if s.deleted[configKey("nsapp.yaml", "DEFAULT_GROUP", "staging")] != 1 {
		t.Fatalf("--namespace should take precedence over --ns: %v", s.deleted)
	}
}

func TestWriteReadonly(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 2)
	s.addConn(t, "ro", "--username", testUser, "--password", testPassword, "--readonly")

	for _, args := range [][]string{
		{"nacos", "config", "publish", "x.yaml", "--content", "a: b"},
		{"nacos", "config", "delete", "seed.yaml"},
	} {
		_, err := runMuxcat(t, args...)
		if e := output.ToError(err); e.Code != output.CodeReadonlyViolation {
			t.Fatalf("%v code = %v, want %s", args, e, output.CodeReadonlyViolation)
		}
	}
	// No write request reached the server.
	if len(s.published) != 0 || len(s.deleted) != 0 {
		t.Fatalf("readonly connection wrote to the server: %v %v", s.published, s.deleted)
	}
	// Reads still work on a readonly connection.
	if _, err := runMuxcat(t, "nacos", "config", "get", "seed.yaml"); err != nil {
		t.Fatalf("readonly get should pass: %v", err)
	}
}

func TestWatchChangeAndError(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 2)
	s.addConn(t, "local", "--username", testUser, "--password", testPassword)

	// The fake listener answers "changed" immediately when the stored md5
	// differs from the subscribed one, and turns to a 500 after a few
	// no-change rounds — watch prints the initial content, then the change,
	// then surfaces the listener error.
	s.mu.Lock()
	s.configs[configKey("watch.yaml", "DEFAULT_GROUP", "public")] = "v: 2"
	s.mu.Unlock()
	out, err := runMuxcat(t, "nacos", "config", "watch", "watch.yaml")
	e := output.ToError(err)
	if e.Code != output.CodeQueryError || !strings.Contains(e.Message, "listener boom") {
		t.Fatalf("watch error = %v", e)
	}
	if !strings.Contains(out, "v: 2") {
		t.Fatalf("watch did not print the current content:\n%s", out)
	}

	// A missing config is watched from empty (no error on the 404).
	s.mu.Lock()
	s.listenerCalls = 0
	s.mu.Unlock()
	out, err = runMuxcat(t, "nacos", "config", "watch", "ghost.yaml")
	if output.ToError(err).Code != output.CodeQueryError {
		t.Fatalf("watch ghost error = %v", err)
	}
	if !strings.Contains(out, "does not exist yet") {
		t.Fatalf("watch missing-config note missing:\n%s", out)
	}
}

func TestConnectFailed(t *testing.T) {
	setupEnv(t)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if out, err := runMuxcat(t, "nacos", "conn", "add", "dead", "--url", deadURL); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_, err := runMuxcat(t, "nacos", "config", "ls")
	e := output.ToError(err)
	if e.Code != output.CodeConnectFailed {
		t.Fatalf("code = %v, want %s", e, output.CodeConnectFailed)
	}
	if got := output.ExitCode(e); got != output.ExitConnect {
		t.Fatalf("exit = %d, want %d", got, output.ExitConnect)
	}
}

func TestClassifyStatus(t *testing.T) {
	// 401/403 map to AUTH_FAILED in both error body shapes.
	spring := []byte(`{"timestamp":1,"status":403,"error":"Forbidden","message":"User not found!"}`)
	envelope := []byte(`{"code":10001,"message":"access denied","data":null}`)
	for _, body := range [][]byte{spring, envelope} {
		e := classifyStatus(403, body)
		if e.Code != output.CodeAuthFailed {
			t.Fatalf("403 code = %s, want AUTH_FAILED", e.Code)
		}
		if !strings.Contains(e.Hint, "username/password") {
			t.Fatalf("403 hint = %q", e.Hint)
		}
	}
	if e := classifyStatus(403, envelope); !strings.Contains(e.Message, "access denied") {
		t.Fatalf("envelope message not extracted: %q", e.Message)
	}
	if e := classifyStatus(403, spring); !strings.Contains(e.Message, "User not found!") {
		t.Fatalf("spring message not extracted: %q", e.Message)
	}
	// 404 maps to QUERY_ERROR with a "not found" message.
	if e := classifyStatus(404, nil); e.Code != output.CodeQueryError || !strings.Contains(e.Message, "not found") {
		t.Fatalf("404 error = %v", e)
	}
	// Other statuses truncate the body.
	long := strings.Repeat("x", 1000)
	if e := classifyStatus(500, []byte(long)); e.Code != output.CodeQueryError || len(e.Message) > 560 {
		t.Fatalf("500 error = %v (len %d)", e.Code, len(e.Message))
	}
}

func TestDecodeData(t *testing.T) {
	// Envelope success unwraps data.
	v, err := decodeData([]byte(`{"code":0,"message":"success","data":"hello"}`))
	if err != nil || v != "hello" {
		t.Fatalf("decodeData success = %v, %v", v, err)
	}
	// Envelope failure becomes QUERY_ERROR with the message.
	_, err = decodeData([]byte(`{"code":300,"message":"bad request"}`))
	if e := output.ToError(err); e.Code != output.CodeQueryError || !strings.Contains(e.Message, "bad request") {
		t.Fatalf("decodeData failure = %v", e)
	}
	// Bare text (v1 config content) passes through.
	v, err = decodeData([]byte("not json {"))
	if err != nil || v != "not json {" {
		t.Fatalf("decodeData bare = %v, %v", v, err)
	}
	// Plain JSON without the envelope markers passes through.
	v, err = decodeData([]byte(`{"count":2,"doms":["a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(map[string]any); !ok {
		t.Fatalf("decodeData plain = %v", v)
	}
}

func TestNoCredentialLeakAcrossCommands(t *testing.T) {
	setupEnv(t)
	s := newNcServer(t, 2)
	s.addConn(t, "local", "--username", testUser, "--password", testPassword)

	for _, args := range [][]string{
		{"nacos", "conn", "ls"},
		{"nacos", "conn", "show", "local"},
		{"nacos", "conn", "test", "local"},
		{"nacos", "config", "ls"},
		{"nacos", "config", "get", "seed.yaml"},
		{"nacos", "service", "ls"},
		{"nacos", "namespace", "ls"},
	} {
		out, err := runMuxcat(t, args...)
		if err != nil {
			t.Fatalf("%v failed: %v", args, err)
		}
		if strings.Contains(out, testPassword) || strings.Contains(out, v2Token) {
			t.Fatalf("%v leaks credentials:\n%s", args, out)
		}
		// JSON mode too.
		env := runJSON(t, args...)
		if s := fmt.Sprint(env); strings.Contains(s, testPassword) || strings.Contains(s, v2Token) {
			t.Fatalf("%v --json leaks credentials: %v", args, env)
		}
	}
}

func TestSchemaValidation(t *testing.T) {
	valid := []byte(`{"version":1,"instances":{"local":{"url":"http://127.0.0.1:8848","version":"auto"}},"connections":{"local":{"instance":"local","username":"u","password":"enc:v1:x","namespace":"staging","readonly":false,"timeout":"5s"}},"defaultConnection":"local"}`)
	if err := schema.Validate(FileName, valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	invalid := []byte(`{"version":1,"instances":{"local":{"url":"http://x","extra":1}}}`)
	if err := schema.Validate(FileName, invalid); err == nil {
		t.Fatal("config with unknown instance field accepted")
	}
	invalidVersion := []byte(`{"version":1,"instances":{"local":{"url":"http://x","version":"4"}}}`)
	if err := schema.Validate(FileName, invalidVersion); err == nil {
		t.Fatal("config with invalid version enum accepted")
	}
	invalidConn := []byte(`{"version":1,"connections":{"local":{"instance":"local","token":"enc:v1:x"}}}`)
	if err := schema.Validate(FileName, invalidConn); err == nil {
		t.Fatal("config with unknown connection field accepted")
	}
}
