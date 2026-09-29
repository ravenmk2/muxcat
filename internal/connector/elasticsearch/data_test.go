package elasticsearch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ravenmk2/muxcat/internal/output"
)

// esDataServer is a fake Elasticsearch for the data commands (search,
// index, doc, cluster). It records search bodies and can answer hits.total
// in either shape.
type esDataServer struct {
	*httptest.Server
	mu            sync.Mutex
	totalAsNumber bool
	lastSearch    []byte
}

func (s *esDataServer) searchBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSearch
}

func newEsDataServer(t *testing.T) *esDataServer {
	t.Helper()
	s := &esDataServer{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case p == "/logs/_search" && r.Method == "POST":
			raw, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.lastSearch = raw
			s.mu.Unlock()
			total := `{"value":1500,"relation":"gte"}`
			if s.totalAsNumber {
				total = `1500`
			}
			writeJSON(w, `{"took":5,"timed_out":false,"hits":{"total":`+total+`,"hits":[`+
				`{"_index":"logs","_id":"1","_score":1.5,"_source":{"level":"error","message":"boom","latency":120,"tags":["a","b"]}},`+
				`{"_index":"logs","_id":"2","_score":1.0,"_source":{"level":"info","user":"alice","meta":{"k":1}}}`+
				`]}}`)
		case p == "/badidx/_search" && r.Method == "POST":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"parsing_exception","reason":"bad query syntax"},"status":400}`))
		case p == "/_cat/indices" && r.Method == "GET":
			writeJSON(w, `[{"health":"green","status":"open","index":"app-logs","docs.count":"1200","store.size":"1.2mb","pri":"1","rep":"1"},`+
				`{"health":"yellow","status":"open","index":"metrics","docs.count":"42","store.size":"10kb","pri":"2","rep":"0"}]`)
		case p == "/app-logs" && r.Method == "GET":
			writeJSON(w, `{"app-logs":{"aliases":{"logs":{},"current":{}},"mappings":{"properties":{"@timestamp":{"type":"date"},"level":{"type":"keyword"},"message":{"type":"text"}}},"settings":{"index":{"number_of_shards":"2","number_of_replicas":"1"}}}}`)
		case p == "/app-logs/_doc/42" && r.Method == "GET":
			writeJSON(w, `{"_index":"app-logs","_id":"42","_version":3,"found":true,"_source":{"level":"error","message":"boom"}}`)
		case p == "/app-logs/_doc/nope" && r.Method == "GET":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_index":"app-logs","_id":"nope","found":false}`))
		case p == "/_cluster/health" && r.Method == "GET":
			writeJSON(w, `{"cluster_name":"docker-cluster","status":"green","timed_out":false,"number_of_nodes":3,"number_of_data_nodes":3,"active_primary_shards":12,"active_shards":24,"relocating_shards":0,"initializing_shards":0,"unassigned_shards":0,"delayed_unassigned_shards":0,"number_of_pending_tasks":0,"number_of_in_flight_fetch":0,"task_max_waiting_in_queue_millis":0,"active_shards_percent_as_number":100.0}`)
		case p == "/_cat/nodes" && r.Method == "GET":
			writeJSON(w, `[{"name":"node-1","ip":"10.0.0.1","node.role":"dimr","master":"*","version":"8.15.0","heap.percent":"53","ram.percent":"97","cpu":"4","load_1m":"0.52"},`+
				`{"name":"node-2","ip":"10.0.0.2","node.role":"dimr","master":"-","version":"8.15.0","heap.percent":"41","ram.percent":"95","cpu":"7","load_1m":"1.05"}]`)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"index_not_found_exception","reason":"no such index"},"status":404}`))
		}
	})
	s.Server = httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

func (s *esDataServer) addConn(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"es", "conn", "add", name, "--url", s.URL}, extra...)
	if out, err := runMuxcat(t, args...); err != nil {
		t.Fatalf("conn add failed: %v\n%s", err, out)
	}
}

func envData(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope data missing or not an object: %v", env)
	}
	return data
}

func tableRows(t *testing.T, env map[string]any) ([]string, []any) {
	t.Helper()
	data := envData(t, env)
	cols := make([]string, 0)
	for _, c := range data["columns"].([]any) {
		cols = append(cols, c.(string))
	}
	return cols, data["rows"].([]any)
}

func TestSearchMatchAll(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "es", "search", "logs")
	if err != nil {
		t.Fatalf("search failed: %v\n%s", err, out)
	}

	var body map[string]any
	if err := json.Unmarshal(s.searchBody(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["query"].(map[string]any)["match_all"]; !ok {
		t.Fatalf("expected match_all query: %s", s.searchBody())
	}
	if body["size"].(float64) != 10 || body["from"].(float64) != 0 {
		t.Fatalf("default pagination wrong: %s", s.searchBody())
	}

	// Text mode: the table header is _id, _score, then the sorted union of
	// _source keys; composite values render as compact JSON.
	header := strings.Fields(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	wantCols := []string{"_id", "_score", "latency", "level", "message", "meta", "tags", "user"}
	if strings.Join(header, ",") != strings.Join(wantCols, ",") {
		t.Fatalf("columns = %v, want %v", header, wantCols)
	}
	if !strings.Contains(out, `["a","b"]`) || !strings.Contains(out, `{"k":1}`) {
		t.Fatalf("composite cells not rendered as compact JSON:\n%s", out)
	}

	// JSON mode carries the raw parsed response.
	env := runJSON(t, "es", "search", "logs")
	data := envData(t, env)
	if data["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["_id"] != "1" {
		t.Fatalf("json data is not the raw response: %v", data)
	}
	if env["meta"].(map[string]any)["truncated"] != true {
		t.Fatalf("expected truncated meta (2 < 1500): %v", env["meta"])
	}
}

// searchTable flattens hits: _id/_score first, sorted _source union,
// composites as compact JSON, missing keys as empty cells.
func TestSearchTable(t *testing.T) {
	sr, err := parseSearchResponse([]byte(`{"took":5,"timed_out":false,"hits":{"total":{"value":1500,"relation":"gte"},"hits":[` +
		`{"_id":"1","_score":1.5,"_source":{"level":"error","latency":120,"tags":["a","b"]}},` +
		`{"_id":"2","_score":1.0,"_source":{"level":"info","user":"alice","meta":{"k":1}}}` +
		`]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if sr.total != 1500 || sr.relation != "gte" || sr.took != 5 {
		t.Fatalf("parsed totals wrong: %+v", sr)
	}
	cols, rows := searchTable(sr.hits)
	wantCols := []string{"_id", "_score", "latency", "level", "meta", "tags", "user"}
	if strings.Join(cols, ",") != strings.Join(wantCols, ",") {
		t.Fatalf("columns = %v, want %v", cols, wantCols)
	}
	row1 := rows[0]
	if row1[0] != "1" || row1[1] != 1.5 || row1[2] != float64(120) || row1[5] != `["a","b"]` {
		t.Fatalf("row1 wrong: %v", row1)
	}
	row2 := rows[1]
	if row2[4] != `{"k":1}` || row2[2] != "" {
		t.Fatalf("row2 wrong: %v", row2)
	}
}

// parseHitsTotal tolerates both hits.total shapes.
func TestParseHitsTotalDualShape(t *testing.T) {
	if n, rel := parseHitsTotal(json.RawMessage(`1500`)); n != 1500 || rel != "eq" {
		t.Fatalf("bare number = %d, %s", n, rel)
	}
	if n, rel := parseHitsTotal(json.RawMessage(`{"value":1500,"relation":"gte"}`)); n != 1500 || rel != "gte" {
		t.Fatalf("object = %d, %s", n, rel)
	}
}

// The source-column union is capped at maxSourceColumns.
func TestSearchTableColumnCap(t *testing.T) {
	src := map[string]any{}
	for i := 0; i < maxSourceColumns+5; i++ {
		src[string(rune('a'+i))] = i
	}
	cols, _ := searchTable([]searchHit{{id: "1", source: src}})
	if len(cols) != 2+maxSourceColumns {
		t.Fatalf("columns not capped: %d", len(cols))
	}
}

func TestSearchQueryMode(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	if _, err := runMuxcat(t, "es", "search", "logs", "-q", "level:error",
		"--size", "5", "--from", "10", "--sort", "@timestamp:desc", "--sort", "level"); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(s.searchBody(), &body); err != nil {
		t.Fatal(err)
	}
	if body["query"].(map[string]any)["query_string"].(map[string]any)["query"] != "level:error" {
		t.Fatalf("query_string wrong: %s", s.searchBody())
	}
	if body["size"].(float64) != 5 || body["from"].(float64) != 10 {
		t.Fatalf("pagination wrong: %s", s.searchBody())
	}
	sortClauses := body["sort"].([]any)
	if sortClauses[0].(map[string]any)["@timestamp"].(map[string]any)["order"] != "desc" ||
		sortClauses[1].(map[string]any)["level"].(map[string]any)["order"] != "asc" {
		t.Fatalf("sort clauses wrong: %s", s.searchBody())
	}

	if _, err := runMuxcat(t, "es", "search", "logs", "--sort", "field:sideways"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("bad sort order code = %v", output.ToError(err))
	}
	if _, err := runMuxcat(t, "es", "search", "logs", "--size", "-1"); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("negative size code = %v", output.ToError(err))
	}
}

func TestSearchFlagConflicts(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	f := filepath.Join(t.TempDir(), "dsl.json")
	if err := os.WriteFile(f, []byte(`{"query":{"match_all":{}},"size":100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runMuxcat(t, "es", "search", "logs", "-q", "x", "--file", f); output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("query+file code = %v", output.ToError(err))
	}
	for _, extra := range [][]string{{"--size", "50"}, {"--from", "1"}, {"--sort", "level:asc"}} {
		args := append([]string{"es", "search", "logs", "--file", f}, extra...)
		if _, err := runMuxcat(t, args...); output.ToError(err).Code != output.CodeConfigInvalid {
			t.Fatalf("%v with --file code = %v", extra, output.ToError(err))
		}
	}

	// --file passes the body through verbatim.
	if _, err := runMuxcat(t, "es", "search", "logs", "--file", f); err != nil {
		t.Fatal(err)
	}
	if string(s.searchBody()) != `{"query":{"match_all":{}},"size":100}` {
		t.Fatalf("file body not passed verbatim: %s", s.searchBody())
	}
}

// A bare-number hits.total (7.x rest_total_hits_as_int) parses the same.
func TestSearchTotalBareNumber(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.totalAsNumber = true
	s.addConn(t, "local")

	env := runJSON(t, "es", "search", "logs")
	total := envData(t, env)["hits"].(map[string]any)["total"]
	if total != float64(1500) {
		t.Fatalf("bare-number total not preserved in JSONData: %v", total)
	}
	if env["meta"].(map[string]any)["truncated"] != true {
		t.Fatalf("expected truncated meta: %v", env["meta"])
	}
}

func TestSearchServerError(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	_, err := runMuxcat(t, "es", "search", "badidx", "-q", "broken")
	if e := output.ToError(err); e.Code != output.CodeQueryError ||
		!strings.Contains(e.Message, "bad query syntax") {
		t.Fatalf("search error = %v", e)
	}
}

func TestIndexLs(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	env := runJSON(t, "es", "index", "ls")
	cols, rows := tableRows(t, env)
	if strings.Join(cols, ",") != "name,health,status,docs,size,pri,rep" {
		t.Fatalf("columns = %v", cols)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	row1 := rows[0].([]any)
	// docs.count/pri/rep arrive as strings and are converted to numbers.
	if row1[0] != "app-logs" || row1[1] != "green" || row1[3] != float64(1200) ||
		row1[4] != "1.2mb" || row1[5] != float64(1) || row1[6] != float64(1) {
		t.Fatalf("row1 wrong: %v", row1)
	}
}

func TestIndexShow(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	env := runJSON(t, "es", "index", "show", "app-logs")
	data := envData(t, env)
	// JSONData is the raw body.
	if _, ok := data["app-logs"]; !ok {
		t.Fatalf("json data is not the raw body: %v", data)
	}
	// Text Value is the summary.
	out, err := runMuxcat(t, "es", "index", "show", "app-logs")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"shards", "replicas", "@timestamp", "current", "logs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("index show output missing %q:\n%s", want, out)
		}
	}
}

func TestDocGet(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	out, err := runMuxcat(t, "es", "doc", "get", "app-logs", "42")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"_index: app-logs", "_id: 42", "_version: 3", `"level": "error"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("doc get output missing %q:\n%s", want, out)
		}
	}
	env := runJSON(t, "es", "doc", "get", "app-logs", "42")
	if envData(t, env)["_source"].(map[string]any)["message"] != "boom" {
		t.Fatalf("json data wrong: %v", env)
	}

	// 404: QUERY_ERROR with a document-not-found hint.
	_, err = runMuxcat(t, "es", "doc", "get", "app-logs", "nope")
	if e := output.ToError(err); e.Code != output.CodeQueryError ||
		!strings.Contains(e.Hint, "document not found") {
		t.Fatalf("doc 404 = %v", e)
	}
}

func TestClusterHealth(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	env := runJSON(t, "es", "cluster", "health")
	data := envData(t, env)
	if data["status"] != "green" || data["cluster_name"] != "docker-cluster" ||
		data["number_of_nodes"] != float64(3) || data["active_shards"] != float64(24) ||
		data["unassigned_shards"] != float64(0) || data["timed_out"] != false {
		t.Fatalf("health value wrong: %v", data)
	}
	out, err := runMuxcat(t, "es", "cluster", "health")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cluster docker-cluster is green (3 nodes, 24 active shards)") {
		t.Fatalf("unexpected message:\n%s", out)
	}
}

func TestClusterNodes(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "local")

	env := runJSON(t, "es", "cluster", "nodes")
	cols, rows := tableRows(t, env)
	if strings.Join(cols, ",") != "name,ip,role,master,version,heap%,ram%,cpu,load_1m" {
		t.Fatalf("columns = %v", cols)
	}
	row1 := rows[0].([]any)
	if row1[0] != "node-1" || row1[1] != "10.0.0.1" || row1[3] != "*" ||
		row1[5] != float64(53) || row1[8] != 0.52 {
		t.Fatalf("row1 wrong: %v", row1)
	}
}

// All batch-2 commands are reads and must work on readonly connections.
func TestReadonlyReadCommands(t *testing.T) {
	setupEnv(t)
	s := newEsDataServer(t)
	s.addConn(t, "ro", "--readonly")

	for _, args := range [][]string{
		{"es", "search", "logs", "-c", "ro"},
		{"es", "index", "ls", "-c", "ro"},
		{"es", "index", "show", "app-logs", "-c", "ro"},
		{"es", "doc", "get", "app-logs", "42", "-c", "ro"},
		{"es", "cluster", "health", "-c", "ro"},
		{"es", "cluster", "nodes", "-c", "ro"},
	} {
		if out, err := runMuxcat(t, args...); err != nil {
			t.Fatalf("%v failed on a readonly connection: %v\n%s", args, err, out)
		}
	}
}
