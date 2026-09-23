package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravenmk2/muxcat/internal/output"
)

func TestDirMUXCATHomeOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(DirEnv, tmp)
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error: %v", err)
	}
	if dir != tmp {
		t.Fatalf("Dir() = %q, want %q", dir, tmp)
	}
}

func TestDirDefault(t *testing.T) {
	t.Setenv(DirEnv, "")
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config", "muxcat")
	if dir != want {
		t.Fatalf("Dir() = %q, want %q", dir, want)
	}
}

func TestLoadMissingReturnsZeroDoc(t *testing.T) {
	t.Setenv(DirEnv, t.TempDir())
	doc, err := Load(MainFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if doc.Version() != 1 {
		t.Fatalf("Version() = %d, want 1", doc.Version())
	}
	if len(doc) != 1 {
		t.Fatalf("zero doc should only carry version, got %v", doc)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(DirEnv, tmp)
	doc := Doc{"version": float64(1)}
	Set(doc, "props.defaults.output", "json")
	if err := Save(MainFile, doc); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(tmp, MainFile))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !strings.Contains(string(raw), "\n  ") {
		t.Fatalf("expected 2-space indented JSON, got:\n%s", raw)
	}

	got, err := Load(MainFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	v, ok := Get(got, "props.defaults.output")
	if !ok || v != "json" {
		t.Fatalf("Get(props.defaults.output) = %v, %v; want json, true", v, ok)
	}

	// Save again over the same path to verify temp file + rename is repeatable
	Set(got, "props.defaults.color", "never")
	if err := Save(MainFile, got); err != nil {
		t.Fatalf("Save() overwrite error: %v", err)
	}
	again, err := Load(MainFile)
	if err != nil {
		t.Fatalf("Load() after overwrite error: %v", err)
	}
	if v, _ := Get(again, "props.defaults.color"); v != "never" {
		t.Fatalf("Get(props.defaults.color) = %v, want never", v)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(DirEnv, tmp)
	if err := os.WriteFile(filepath.Join(tmp, MainFile), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(MainFile)
	if err == nil {
		t.Fatal("Load() should fail on invalid JSON")
	}
	if code := output.ToError(err).Code; code != output.CodeConfigInvalid {
		t.Fatalf("error code = %s, want %s", code, output.CodeConfigInvalid)
	}
}

func TestGetSetDotPath(t *testing.T) {
	doc := Doc{"version": float64(1)}
	Set(doc, "a.b.c", 42.0)
	v, ok := Get(doc, "a.b.c")
	if !ok || v != 42.0 {
		t.Fatalf("Get(a.b.c) = %v, %v; want 42, true", v, ok)
	}
	if _, ok := Get(doc, "a.b.x"); ok {
		t.Fatal("Get(a.b.x) should miss")
	}
	if _, ok := Get(doc, "a.b.c.d"); ok {
		t.Fatal("Get(a.b.c.d) should miss through scalar")
	}
	// Overwrite an existing intermediate level
	Set(doc, "a.b", "scalar")
	if v, _ := Get(doc, "a.b"); v != "scalar" {
		t.Fatalf("Get(a.b) = %v, want scalar", v)
	}
}

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{MainFile, "defaults.timeout", "props.defaults.timeout"},
		{MainFile, "props.defaults.timeout", "props.defaults.timeout"},
		{MainFile, "props", "props"},
		{MainFile, "version", "version"},
		{"sqlite.json", "instances.local", "instances.local"},
	}
	for _, c := range cases {
		if got := NormalizePath(c.name, c.in); got != c.want {
			t.Errorf("NormalizePath(%q, %q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestParseValue(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"10s", "10s"},
		{"42", float64(42)},
		{"true", true},
		{"null", nil},
		{`"quoted"`, "quoted"},
	}
	for _, c := range cases {
		if got := ParseValue(c.in); got != c.want {
			t.Errorf("ParseValue(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
