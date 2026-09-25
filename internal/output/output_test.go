package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name          string
		flag, def     string
		flagJSON, tty bool
		want          Mode
	}{
		{"json flag wins over output flag", "tsv", "", true, true, ModeJSON},
		{"flag beats default", "tsv", "json", false, true, ModeTSV},
		{"default beats auto", "", "plain", false, true, ModePlain},
		{"auto tty -> table", "", "", false, true, ModeTable},
		{"auto non-tty -> plain", "", "", false, false, ModePlain},
		{"auto default tty", "", "auto", false, true, ModeTable},
	}
	for _, c := range cases {
		got, err := ResolveMode(c.flag, c.flagJSON, c.def, c.tty)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: ResolveMode(%q, %v, %q, %v) = %q, want %q",
				c.name, c.flag, c.flagJSON, c.def, c.tty, got, c.want)
		}
	}
}

func TestResolveModeInvalid(t *testing.T) {
	if _, err := ResolveMode("yaml", false, "", true); err == nil {
		t.Fatal("invalid --output should fail")
	} else if ToError(err).Code != CodeConfigInvalid {
		t.Fatalf("code = %s, want %s", ToError(err).Code, CodeConfigInvalid)
	}
	if _, err := ResolveMode("", false, "yaml", true); err == nil {
		t.Fatal("invalid props.defaults.output should fail")
	}
}

func TestResolveColor(t *testing.T) {
	// NO_COLOR uses presence semantics; the test controls it explicitly.
	orig, had := os.LookupEnv("NO_COLOR")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("NO_COLOR", orig)
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
	})
	cases := []struct {
		name       string
		def        string
		noColor    bool
		tty        bool
		noColorEnv bool
		want       bool
	}{
		{"tty auto", "", false, true, false, true},
		{"non-tty forced off", "always", false, false, false, false},
		{"--no-color flag", "", true, true, false, false},
		{"NO_COLOR env", "", false, true, true, false},
		{"always on tty", "always", false, true, false, true},
		{"never", "never", false, true, false, false},
	}
	for _, c := range cases {
		if c.noColorEnv {
			_ = os.Setenv("NO_COLOR", "1")
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
		if got := ResolveColor(c.def, c.noColor, c.tty); got != c.want {
			t.Errorf("%s: ResolveColor(%q, %v, %v) = %v, want %v",
				c.name, c.def, c.noColor, c.tty, got, c.want)
		}
	}
}

func tableResult() *Result {
	return &Result{
		Columns: []string{"name", "age"},
		Rows:    [][]any{{"alice", 30.0}, {"bob", nil}},
	}
}

func TestJSONRendererSnapshot(t *testing.T) {
	var buf bytes.Buffer
	if err := NewRenderer(ModeJSON, false).Render(&buf, tableResult()); err != nil {
		t.Fatal(err)
	}
	want := `{
  "columns": [
    "name",
    "age"
  ],
  "rows": [
    [
      "alice",
      30
    ],
    [
      "bob",
      null
    ]
  ]
}
`
	if buf.String() != want {
		t.Fatalf("json render mismatch:\ngot:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestTSVRendererSnapshot(t *testing.T) {
	var buf bytes.Buffer
	if err := NewRenderer(ModeTSV, false).Render(&buf, tableResult()); err != nil {
		t.Fatal(err)
	}
	want := "name\tage\nalice\t30\nbob\t\n"
	if buf.String() != want {
		t.Fatalf("tsv render mismatch:\ngot:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestPlainRendererSnapshot(t *testing.T) {
	var buf bytes.Buffer
	if err := NewRenderer(ModePlain, false).Render(&buf, tableResult()); err != nil {
		t.Fatal(err)
	}
	want := "name   age\nalice  30\nbob\n"
	if buf.String() != want {
		t.Fatalf("plain render mismatch:\ngot:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestTableRendererSnapshot(t *testing.T) {
	var buf bytes.Buffer
	if err := NewRenderer(ModeTable, false).Render(&buf, tableResult()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// lipgloss emits no ANSI escapes without a TTY
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("table render should not emit ANSI escapes without color:\n%q", out)
	}
	for _, frag := range []string{"name", "age", "alice", "bob"} {
		if !strings.Contains(out, frag) {
			t.Fatalf("table render missing %q:\n%s", frag, out)
		}
	}
	if !strings.Contains(out, "│") {
		t.Fatalf("table render should have borders:\n%s", out)
	}
}

func TestRenderFallbackMessage(t *testing.T) {
	for _, mode := range []Mode{ModeTSV, ModePlain, ModeTable} {
		var buf bytes.Buffer
		if err := NewRenderer(mode, false).Render(&buf, &Result{Message: "done"}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if buf.String() != "done\n" {
			t.Fatalf("%s message render = %q, want %q", mode, buf.String(), "done\n")
		}
	}
}

func TestRenderFallbackBare(t *testing.T) {
	cases := []struct {
		name string
		res  *Result
		want string
	}{
		{"single-entry map renders bare", &Result{Value: map[string]any{"value": "hello"}, Bare: true}, "hello\n"},
		{"nil value renders empty line", &Result{Value: map[string]any{"value": nil}, Bare: true}, "\n"},
		{"multi-entry map ignores bare", &Result{Value: map[string]any{"a": 1, "b": 2}, Bare: true}, "a: 1\nb: 2\n"},
		{"without bare the label stays", &Result{Value: map[string]any{"value": "hello"}}, "value: hello\n"},
		{"scalar value unaffected", &Result{Value: "raw", Bare: true}, "raw\n"},
	}
	for _, tc := range cases {
		for _, mode := range []Mode{ModeTSV, ModePlain, ModeTable} {
			var buf bytes.Buffer
			if err := NewRenderer(mode, false).Render(&buf, tc.res); err != nil {
				t.Fatalf("%s/%s: %v", tc.name, mode, err)
			}
			if buf.String() != tc.want {
				t.Fatalf("%s/%s render = %q, want %q", tc.name, mode, buf.String(), tc.want)
			}
		}
	}
	// JSON rendering is unaffected by Bare.
	var buf bytes.Buffer
	res := &Result{Value: map[string]any{"value": "hello"}, Bare: true}
	if err := NewRenderer(ModeJSON, false).Render(&buf, res); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data["value"] != "hello" {
		t.Fatalf("json payload = %v, want value hello", data)
	}
}

func TestEnvelopeSuccessShape(t *testing.T) {
	var buf bytes.Buffer
	env := Success(map[string]any{"n": 1}, Meta{Connector: "sqlite", Connection: "local", ElapsedMS: 3, Truncated: true})
	if err := WriteEnvelope(&buf, env); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["ok"] != true {
		t.Fatalf("ok = %v", m["ok"])
	}
	meta := m["meta"].(map[string]any)
	if meta["connector"] != "sqlite" || meta["connection"] != "local" ||
		meta["elapsed_ms"] != 3.0 || meta["truncated"] != true {
		t.Fatalf("meta = %v", meta)
	}
	if _, hasErr := m["error"]; hasErr {
		t.Fatal("success envelope should omit error")
	}
}

func TestEnvelopeErrorShape(t *testing.T) {
	var buf bytes.Buffer
	env := Failure(NewError(CodeMissingArgument, "missing required argument", "see --help"), Meta{})
	if err := WriteEnvelope(&buf, env); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["ok"] != false {
		t.Fatalf("ok = %v", m["ok"])
	}
	e := m["error"].(map[string]any)
	if e["code"] != CodeMissingArgument || e["message"] != "missing required argument" || e["hint"] != "see --help" {
		t.Fatalf("error = %v", e)
	}
	if _, hasData := m["data"]; hasData {
		t.Fatal("error envelope should omit data")
	}
	// the fixed meta shape must be present
	meta := m["meta"].(map[string]any)
	for _, k := range []string{"connector", "connection", "elapsed_ms", "truncated"} {
		if _, ok := meta[k]; !ok {
			t.Fatalf("meta missing key %q: %v", k, meta)
		}
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{CodeMissingArgument, ExitUsage},
		{CodeConnNotFound, ExitUsage},
		{CodeConfigInvalid, ExitUsage},
		{CodeUnsupportedOperation, ExitUsage},
		{CodeConnectorUnknown, ExitUsage},
		{CodeConnectFailed, ExitConnect},
		{CodeTimeout, ExitConnect},
		{CodeAuthFailed, ExitAuth},
		{CodeQueryError, ExitExec},
		{CodeReadonlyViolation, ExitExec},
		{CodeKeyUnavailable, ExitGeneral},
	}
	for _, c := range cases {
		if got := ExitCode(NewError(c.code, "x", "")); got != c.want {
			t.Errorf("ExitCode(%s) = %d, want %d", c.code, got, c.want)
		}
	}
	if got := ExitCode(errors.New("plain error")); got != ExitGeneral {
		t.Errorf("ExitCode(plain) = %d, want %d", got, ExitGeneral)
	}
}
