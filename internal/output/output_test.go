package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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

func TestHighlight(t *testing.T) {
	out := highlight(`{"a":1}`, "json")
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("highlight should emit ANSI escapes, got %q", out)
	}
	if !strings.Contains(out, `"a"`) {
		t.Fatalf("highlight should keep the content, got %q", out)
	}
	plain := "not json"
	if got := highlight(plain, "no-such-lexer"); got != plain {
		t.Fatalf("unknown lexer should return input unchanged, got %q", got)
	}
}

func TestRenderFallbackSyntax(t *testing.T) {
	res := &Result{Value: map[string]any{"value": `{"a":1}`}, Bare: true, Syntax: "json"}

	var colored bytes.Buffer
	if err := NewRenderer(ModePlain, true).Render(&colored, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(colored.String(), "\x1b[") {
		t.Fatalf("color on + Syntax should highlight, got %q", colored.String())
	}

	var plain bytes.Buffer
	if err := NewRenderer(ModePlain, false).Render(&plain, res); err != nil {
		t.Fatal(err)
	}
	if plain.String() != `{"a":1}`+"\n" {
		t.Fatalf("color off should render bare and unhighlighted, got %q", plain.String())
	}

	// JSON rendering ignores Syntax.
	var js bytes.Buffer
	if err := NewRenderer(ModeJSON, false).Render(&js, res); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(js.String(), "\x1b[") {
		t.Fatalf("json rendering must stay clean, got %q", js.String())
	}
}

// testCellStyle mirrors the mysql connector's declared presentation.
var testCellStyle = &CellStyle{
	NullText:   "NULL",
	BinaryHex:  true,
	DateLayout: "2006-01-02",
	TimeLayout: "2006-01-02 15:04:05.999999",
	Palette: &CellPalette{
		Null:     lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true),
		Number:   lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		String:   lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		Temporal: lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		Binary:   lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
	},
}

// TestFormatCellLegacyDefault covers the nil-style (legacy) path: nil ->
// "", []byte -> string, everything else fmt.Sprint, never colored even
// when color is requested.
func TestFormatCellLegacyDefault(t *testing.T) {
	var s *CellStyle
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"nil renders empty", nil, ""},
		{"string", "hello", "hello"},
		{"int", int64(42), "42"},
		{"bytes as string", []byte("abc"), "abc"},
		{"binary bytes stay raw", []byte{0xDE, 0xAD}, "\xde\xad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.FormatCell(tc.v, "BLOB", false); got != tc.want {
				t.Fatalf("nil-style FormatCell(%v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
	tm := time.Date(2024, 2, 29, 12, 34, 56, 789000000, time.UTC)
	if got, want := s.FormatCell(tm, "DATE", false), fmt.Sprint(tm); got != want {
		t.Fatalf("legacy time.Time = %q, want fmt.Sprint form %q", got, want)
	}
	if got := s.FormatCell(int64(1), "INT", true); strings.Contains(got, "\x1b[") {
		t.Fatalf("legacy path must never color, got %q", got)
	}
}

func TestFormatCell(t *testing.T) {
	dt := time.Date(2024, 2, 29, 12, 34, 56, 789000000, time.UTC)
	cases := []struct {
		name   string
		v      any
		dbType string
		want   string
	}{
		{"nil untyped", nil, "", "NULL"},
		{"nil typed", nil, "VARCHAR", "NULL"},
		{"empty string stays empty", "", "VARCHAR", ""},
		{"string", "hello", "VARCHAR", "hello"},
		{"untyped string", "hello", "", "hello"},
		{"int", int64(42), "BIGINT", "42"},
		{"float", 3.14, "DOUBLE", "3.14"},
		{"bool passthrough", true, "TINYINT", "true"},
		{"decimal bytes keep precision", []byte("123.450"), "DECIMAL", "123.450"},
		{"text bytes", []byte("abc"), "TEXT", "abc"},
		{"untyped bytes as string", []byte("abc"), "", "abc"},
		{"unsigned bigint text not hex", []byte("123"), "UNSIGNED BIGINT", "123"},
		{"binary hex", []byte{0xDE, 0xAD, 0xBE, 0xEF}, "BINARY", "0xDEADBEEF"},
		{"varbinary lowercase type", []byte{0x00, 0xff}, "varbinary", "0x00FF"},
		{"tinyblob", []byte{0x01}, "TINYBLOB", "0x01"},
		{"mediumblob", []byte{0x01}, "MEDIUMBLOB", "0x01"},
		{"longblob", []byte{0x01}, "LONGBLOB", "0x01"},
		{"geometry", []byte{0x01}, "GEOMETRY", "0x01"},
		{"bit", []byte{0x05}, "BIT", "0x05"},
		{"empty blob", []byte{}, "BLOB", "0x"},
		{"datetime with micros", dt, "DATETIME", "2024-02-29 12:34:56.789"},
		{"datetime without micros", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), "DATETIME", "2024-02-29 00:00:00"},
		{"date type drops time", dt, "DATE", "2024-02-29"},
		{"date lowercase type", dt, "date", "2024-02-29"},
		{"timestamp keeps datetime", dt, "TIMESTAMP", "2024-02-29 12:34:56.789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := testCellStyle.FormatCell(tc.v, tc.dbType, false); got != tc.want {
				t.Fatalf("FormatCell(%v, %q, false) = %q, want %q", tc.v, tc.dbType, got, tc.want)
			}
		})
	}
}

func TestFormatCellColor(t *testing.T) {
	cases := []struct {
		name   string
		v      any
		dbType string
	}{
		{"null", nil, ""},
		{"number", int64(1), "INT"},
		{"string", "s", "VARCHAR"},
		{"datetime", time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC), "DATETIME"},
		{"binary", []byte{0x01}, "BLOB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain := testCellStyle.FormatCell(tc.v, tc.dbType, false)
			colored := testCellStyle.FormatCell(tc.v, tc.dbType, true)
			if !strings.Contains(colored, "\x1b[") {
				t.Fatalf("colored cell should emit ANSI escapes, got %q", colored)
			}
			if ansi.Strip(colored) != plain {
				t.Fatalf("strip(FormatCell color) = %q, want %q", ansi.Strip(colored), plain)
			}
		})
	}

	// A style without a palette never colors, even when color is on.
	noPalette := &CellStyle{NullText: "NULL"}
	if got := noPalette.FormatCell(nil, "", true); got != "NULL" {
		t.Fatalf("nil-palette FormatCell = %q, want plain NULL", got)
	}
}

func TestIsBinaryType(t *testing.T) {
	for _, typ := range []string{"BINARY", "varbinary", "TinyBlob", "BLOB", "MEDIUMBLOB", "LONGBLOB", "GEOMETRY", "BIT"} {
		if !IsBinaryType(typ) {
			t.Fatalf("IsBinaryType(%q) = false, want true", typ)
		}
	}
	for _, typ := range []string{"", "VARCHAR", "TEXT", "UNSIGNED BIGINT", "INT", "DECIMAL", "JSON"} {
		if IsBinaryType(typ) {
			t.Fatalf("IsBinaryType(%q) = true, want false", typ)
		}
	}
}

func TestBinaryCellsToHex(t *testing.T) {
	rows := [][]any{{[]byte{0xAB, 0xCD}, "s", nil, int64(1), time.Unix(0, 0).UTC()}}
	out := BinaryCellsToHex(rows)
	if out[0][0] != "0xABCD" {
		t.Fatalf("binary cell = %v, want 0xABCD", out[0][0])
	}
	if out[0][1] != "s" || out[0][2] != nil || out[0][3] != int64(1) {
		t.Fatalf("non-binary cells must pass through: %v", out[0])
	}
	if _, ok := out[0][4].(time.Time); !ok {
		t.Fatalf("time.Time must stay structured, got %T", out[0][4])
	}
	if b, ok := rows[0][0].([]byte); !ok || b[0] != 0xAB {
		t.Fatal("input rows must not be mutated")
	}
}

// TestPlainRendererANSIWidth verifies colored plain output stays aligned:
// stripping the ANSI escapes must yield the uncolored rendering.
func TestPlainRendererANSIWidth(t *testing.T) {
	res := &Result{
		Columns:     []string{"n", "v"},
		ColumnTypes: []string{"BIGINT", "VARBINARY"},
		CellStyle:   testCellStyle,
		Rows: [][]any{
			{int64(1), []byte{0xAB}},
			{int64(10000), []byte{0x01, 0x02}},
			{nil, nil},
		},
	}
	var colored, plain bytes.Buffer
	if err := NewRenderer(ModePlain, true).Render(&colored, res); err != nil {
		t.Fatal(err)
	}
	if err := NewRenderer(ModePlain, false).Render(&plain, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(colored.String(), "\x1b[") {
		t.Fatalf("colored plain render should emit ANSI escapes:\n%q", colored.String())
	}
	if ansi.Strip(colored.String()) != plain.String() {
		t.Fatalf("ANSI-stripped colored render mismatch:\ngot:\n%q\nwant:\n%q",
			ansi.Strip(colored.String()), plain.String())
	}
	if !strings.Contains(plain.String(), "0xAB") || !strings.Contains(plain.String(), "NULL") {
		t.Fatalf("plain render missing hex/NULL:\n%q", plain.String())
	}
}

func stubTermWidth(t *testing.T, width int, ok bool) {
	t.Helper()
	orig := termWidth
	termWidth = func() (int, bool) { return width, ok }
	t.Cleanup(func() { termWidth = orig })
}

func wideResult() *Result {
	return &Result{
		Columns:     []string{"id", "payload"},
		ColumnTypes: []string{"BIGINT", "VARBINARY"},
		CellStyle:   testCellStyle,
		Rows: [][]any{
			{int64(1), bytes.Repeat([]byte{0xAB}, 40)}, // 82-cell hex string
			{int64(2), []byte{0x01}},
		},
	}
}

func TestTableTruncation(t *testing.T) {
	stubTermWidth(t, 40, true)
	var buf bytes.Buffer
	if err := NewRenderer(ModeTable, false).Render(&buf, wideResult()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Fatalf("line width %d exceeds terminal width 40: %q", w, line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("truncated cell should carry …:\n%s", out)
	}
	if !strings.Contains(out, "0xAB") || !strings.Contains(out, "0x01") {
		t.Fatalf("hex content should survive truncation:\n%s", out)
	}

	// A narrow terminal below the column minimums must not panic; columns
	// keep their floor and the overflow is accepted.
	stubTermWidth(t, 15, true)
	buf.Reset()
	if err := NewRenderer(ModeTable, false).Render(&buf, wideResult()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "payload") {
		t.Fatalf("headers are never truncated:\n%s", buf.String())
	}

	// Unknown terminal width: no truncation at all.
	stubTermWidth(t, 0, false)
	buf.Reset()
	if err := NewRenderer(ModeTable, false).Render(&buf, wideResult()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "…") {
		t.Fatalf("unknown width must not truncate:\n%s", buf.String())
	}
}

// TestTableTruncationColored verifies ANSI-styled cells are measured and
// cut by display width, still fitting the terminal.
func TestTableTruncationColored(t *testing.T) {
	stubTermWidth(t, 40, true)
	var buf bytes.Buffer
	if err := NewRenderer(ModeTable, true).Render(&buf, wideResult()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatal("colored table should emit ANSI escapes")
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Fatalf("colored line width %d exceeds terminal width 40: %q", w, line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("truncated cell should carry …:\n%s", out)
	}
}
