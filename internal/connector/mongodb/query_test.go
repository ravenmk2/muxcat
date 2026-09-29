package mongodb

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/schema"
)

func TestResolveJSONInput(t *testing.T) {
	setupEnv(t)

	// Positional argument wins.
	cmd := newQueryCmd()
	got, err := resolveJSONInput(cmd, []string{`{"a":1}`}, "filter")
	if err != nil || got != `{"a":1}` {
		t.Fatalf("positional: got=%q err=%v", got, err)
	}

	// --file reads a file.
	f := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(f, []byte(`{"b":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = newQueryCmd()
	if err := cmd.Flags().Set("file", f); err != nil {
		t.Fatal(err)
	}
	got, err = resolveJSONInput(cmd, nil, "filter")
	if err != nil || got != `{"b":2}` {
		t.Fatalf("--file: got=%q err=%v", got, err)
	}

	// Positional argument and --file together are rejected.
	cmd = newQueryCmd()
	if err := cmd.Flags().Set("file", f); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJSONInput(cmd, []string{`{"a":1}`}, "filter"); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("arg + --file: err=%v", err)
	}

	// --file - reads stdin.
	stubStdinTTY(t, true) // must not matter; - forces stdin
	cmd = newQueryCmd()
	if err := cmd.Flags().Set("file", "-"); err != nil {
		t.Fatal(err)
	}
	cmd.SetIn(strings.NewReader(`{"c":3}`))
	got, err = resolveJSONInput(cmd, nil, "filter")
	if err != nil || got != `{"c":3}` {
		t.Fatalf("--file -: got=%q err=%v", got, err)
	}

	// No input on a TTY is MISSING_ARGUMENT with a usage hint.
	stubStdinTTY(t, true)
	cmd = newQueryCmd()
	if _, err := resolveJSONInput(cmd, nil, "filter"); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("TTY without input: err=%v", err)
	} else if output.ToError(err).Hint == "" {
		t.Fatal("MISSING_ARGUMENT should carry a hint")
	}

	// No argument with piped stdin reads it.
	stubStdinTTY(t, false)
	cmd = newQueryCmd()
	cmd.SetIn(strings.NewReader(`{"d":4}`))
	got, err = resolveJSONInput(cmd, nil, "filter")
	if err != nil || got != `{"d":4}` {
		t.Fatalf("piped stdin: got=%q err=%v", got, err)
	}
}

func TestParseFilter(t *testing.T) {
	d, err := parseFilter("")
	if err != nil || len(d) != 0 {
		t.Fatalf("empty filter: d=%v err=%v", d, err)
	}
	d, err = parseFilter(`{"_id":{"$oid":"0123456789abcdef01234567"},"ts":{"$date":"2024-01-01T00:00:00Z"}}`)
	if err != nil || len(d) != 2 {
		t.Fatalf("ExtJSON filter: d=%v err=%v", d, err)
	}
	for _, bad := range []string{"{nope", `[1,2]`} {
		if _, err := parseFilter(bad); err == nil ||
			output.ToError(err).Code != output.CodeConfigInvalid {
			t.Fatalf("filter %q: err=%v, want CONFIG_INVALID", bad, err)
		}
	}
}

func TestParsePipeline(t *testing.T) {
	p, err := parsePipeline(`[{"$match":{"status":"ok"}},{"$limit":5}]`)
	if err != nil || len(p) != 2 {
		t.Fatalf("valid pipeline: p=%v err=%v", p, err)
	}
	// Write stages are rejected anywhere in the stage list.
	for _, s := range []string{`[{"$out":"x"}]`, `[{"$match":{}},{"$merge":{"into":"t"}}]`} {
		if _, err := parsePipeline(s); err == nil ||
			output.ToError(err).Code != output.CodeReadonlyViolation {
			t.Fatalf("pipeline %s: err=%v, want READONLY_VIOLATION", s, err)
		}
	}
	for _, bad := range []string{`{"$match":{}}`, `["nope"]`, `{nope`} {
		if _, err := parsePipeline(bad); err == nil ||
			output.ToError(err).Code != output.CodeConfigInvalid {
			t.Fatalf("pipeline %q: err=%v, want CONFIG_INVALID", bad, err)
		}
	}
}

func TestParseSorts(t *testing.T) {
	d, err := parseSorts([]string{"age"})
	if err != nil || len(d) != 1 || d[0].Key != "age" || d[0].Value != int32(1) {
		t.Fatalf("bare field: d=%v err=%v", d, err)
	}
	d, err = parseSorts([]string{"age:desc", "name:asc"})
	if err != nil || len(d) != 2 || d[0].Value != int32(-1) || d[1].Value != int32(1) {
		t.Fatalf("asc/desc: d=%v err=%v", d, err)
	}
	for _, bad := range []string{"age:sideways", ":asc"} {
		if _, err := parseSorts([]string{bad}); err == nil ||
			output.ToError(err).Code != output.CodeConfigInvalid {
			t.Fatalf("sort %q: err=%v, want CONFIG_INVALID", bad, err)
		}
	}
}

func TestDocsTable(t *testing.T) {
	oid1 := bson.NewObjectID()
	oid2 := bson.NewObjectID()
	docs := []bson.D{
		{{Key: "_id", Value: oid1}, {Key: "name", Value: "a"}, {Key: "age", Value: int32(30)}, {Key: "tags", Value: bson.A{"x"}}},
		{{Key: "_id", Value: oid2}, {Key: "name", Value: "b"}, {Key: "active", Value: true}, {Key: "score", Value: 9.5}},
	}
	columns, rows, extra := docsTable(docs)
	want := []string{"_id", "name", "age", "tags", "active", "score"}
	if strings.Join(columns, ",") != strings.Join(want, ",") {
		t.Fatalf("columns = %v, want %v", columns, want)
	}
	if extra != 0 {
		t.Fatalf("extra = %d, want 0", extra)
	}
	if rows[0][0] != oid1.Hex() || rows[0][1] != "a" || rows[0][2] != "30" ||
		rows[0][3] != `["x"]` || rows[0][4] != "" || rows[0][5] != "" {
		t.Fatalf("row0 = %v", rows[0])
	}
	if rows[1][4] != "true" || rows[1][5] != "9.5" {
		t.Fatalf("row1 = %v", rows[1])
	}

	// A document without _id drops the _id column.
	columns, _, _ = docsTable([]bson.D{
		{{Key: "_id", Value: oid1}},
		{{Key: "name", Value: "x"}},
	})
	if columns[0] == "_id" {
		t.Fatalf("_id column should be dropped when not in every doc: %v", columns)
	}

	// The column cap drops extra fields.
	wide := bson.D{{Key: "_id", Value: oid1}}
	for i := 0; i < maxDocColumns+5; i++ {
		wide = append(wide, bson.E{Key: "f" + strings.Repeat("x", i+1), Value: int32(i)})
	}
	columns, _, extra = docsTable([]bson.D{wide})
	if len(columns) != maxDocColumns+1 || extra != 5 {
		t.Fatalf("cap: columns=%d extra=%d", len(columns), extra)
	}
}

func TestCellString(t *testing.T) {
	ts := bson.NewDateTimeFromTime(time.Date(2024, 2, 29, 12, 34, 56, 0, time.UTC))
	cases := []struct {
		v    any
		want string
		ok   bool
	}{
		{nil, "null", true},
		{"s", "s", true},
		{true, "true", true},
		{int32(7), "7", true},
		{int64(8), "8", true},
		{1.25, "1.25", true},
		{ts, "2024-02-29T12:34:56Z", true},
		{bson.NewObjectID(), "", true}, // hex checked separately
		{bson.A{1}, "", false},
		{bson.D{{Key: "x", Value: 1}}, "", false},
	}
	for _, tc := range cases {
		got, ok := cellString(tc.v)
		if ok != tc.ok || (tc.want != "" && got != tc.want) {
			t.Fatalf("cellString(%v) = %q,%v want %q,%v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
}

// renderTestCmd builds a bare command with the output flags renderDocs
// needs, writing to a buffer.
func renderTestCmd(jsonOut bool) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.Flags().String("output", "", "")
	cmd.Flags().Bool("json", jsonOut, "")
	cmd.Flags().Bool("no-color", true, "")
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	return cmd, buf
}

func TestDocsMessage(t *testing.T) {
	if got := docsMessage(2, false, 0); got != "2 documents" {
		t.Fatalf("plain: %q", got)
	}
	if got := docsMessage(1000, true, 0); got != "1000 documents (truncated)" {
		t.Fatalf("truncated: %q", got)
	}
	if got := docsMessage(3, false, 4); got != "3 documents, +4 more fields, use --json" {
		t.Fatalf("capped: %q", got)
	}
	if got := docsMessage(3, true, 4); got != "3 documents (truncated), +4 more fields, use --json" {
		t.Fatalf("truncated+capped: %q", got)
	}
}

func TestRenderDocs(t *testing.T) {
	setupEnv(t)
	docs := []bson.D{
		{{Key: "_id", Value: bson.NewObjectID()}, {Key: "name", Value: "a"}},
		{{Key: "_id", Value: bson.NewObjectID()}, {Key: "name", Value: "b"}},
	}

	cmd, buf := renderTestCmd(false)
	if err := renderDocs(cmd, "local", docs, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "_id") || !strings.Contains(buf.String(), "name") ||
		!strings.Contains(buf.String(), "a") || !strings.Contains(buf.String(), "b") {
		t.Fatalf("table output: %q", buf.String())
	}

	// JSON mode yields a valid envelope whose data is the document array.
	cmd, buf = renderTestCmd(true)
	if err := renderDocs(cmd, "local", docs, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK   bool             `json:"ok"`
		Data []map[string]any `json:"data"`
		Meta struct {
			Connector string `json:"connector"`
			Truncated bool   `json:"truncated"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\n%s", err, buf.String())
	}
	if !env.OK || len(env.Data) != 2 || env.Data[0]["name"] != "a" ||
		env.Meta.Connector != "mongodb" || !env.Meta.Truncated {
		t.Fatalf("envelope = %+v", env)
	}
	id, ok := env.Data[0]["_id"].(map[string]any)
	if !ok {
		t.Fatalf("_id should be an Extended JSON object: %v", env.Data[0]["_id"])
	}
	if hex, ok := id["$oid"].(string); !ok || len(hex) != 24 {
		t.Fatalf("_id.$oid should be a 24-char hex string: %v", id)
	}
}

func TestResolveDB(t *testing.T) {
	setupEnv(t)
	cmd := newQueryCmd()
	if _, err := resolveDB(cmd, Connection{}); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("no db: err=%v, want MISSING_ARGUMENT", err)
	}
	if got, err := resolveDB(cmd, Connection{Database: "app"}); err != nil || got != "app" {
		t.Fatalf("connection database: got=%q err=%v", got, err)
	}
	if err := cmd.Flags().Set("db", "other"); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveDB(cmd, Connection{Database: "app"}); err != nil || got != "other" {
		t.Fatalf("--db overrides: got=%q err=%v", got, err)
	}
}

func TestSchemaValidationReadonly(t *testing.T) {
	valid := []byte(`{
	  "version": 1,
	  "instances": {"local": {"hosts": ["127.0.0.1:27017"]}},
	  "connections": {"local": {"instance": "local", "readonly": true}}
	}`)
	if err := schema.Validate("mongodb.json", valid); err != nil {
		t.Fatalf("readonly doc rejected: %v", err)
	}
	extra := []byte(`{"version": 1, "connections": {"local": {"instance": "local", "writable": false}}}`)
	if err := schema.Validate("mongodb.json", extra); err == nil {
		t.Fatal("doc with an unknown connection field should be rejected")
	}
}

func TestConnAddReadonly(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--readonly")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Connections["ro"].Readonly {
		t.Fatal("readonly flag should persist")
	}
	out, err := runMuxcat(t, "mongodb", "conn", "ls")
	if err != nil || !strings.Contains(out, "readonly") || !strings.Contains(out, "true") {
		t.Fatalf("conn ls should show the readonly column: err=%v out=%q", err, out)
	}
	out, err = runMuxcat(t, "mongodb", "conn", "show", "ro")
	if err != nil || !strings.Contains(out, "readonly") {
		t.Fatalf("conn show should show readonly: err=%v out=%q", err, out)
	}
}
