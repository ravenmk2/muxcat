package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// TestDegradedResultEnvelope runs a degraded command end-to-end through
// cli.Execute: a failed endpoint status in --json mode must print exactly
// one failure envelope carrying the partial data, and exit with the
// connect-class code.
func TestDegradedResultEnvelope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MUXCAT_HOME", home)
	t.Setenv("NO_COLOR", "1")
	cfg := `{
	  "version": 1,
	  "instances": {"down": {"endpoints": ["127.0.0.1:1"]}},
	  "connections": {"down": {"instance": "down", "timeout": "2s"}},
	  "defaultConnection": "down"
	}`
	if err := os.WriteFile(filepath.Join(home, "etcd.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	oldArgs := os.Args
	os.Args = []string{"muxcat", "etcd", "endpoint", "status", "--json"}
	code := cli.Execute("test")
	os.Args = oldArgs
	_ = w.Close()
	os.Stdout = oldStdout
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if code != output.ExitConnect {
		t.Fatalf("exit code = %d, want %d (out: %s)", code, output.ExitConnect, out)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Columns []string `json:"columns"`
			Rows    [][]any  `json:"rows"`
		} `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &env); err != nil {
		t.Fatalf("stdout should be a single envelope, got %q: %v", out, err)
	}
	if env.OK {
		t.Fatalf("ok should be false: %s", out)
	}
	if env.Error.Code != output.CodeConnectFailed && env.Error.Code != output.CodeTimeout {
		t.Fatalf("error.code = %s, want CONNECT_FAILED or TIMEOUT", env.Error.Code)
	}
	if len(env.Data.Columns) == 0 || len(env.Data.Rows) != 1 {
		t.Fatalf("envelope should carry the partial table: %s", out)
	}
	if errText, _ := env.Data.Rows[0][8].(string); errText == "" {
		t.Fatalf("error column should be filled: %v", env.Data.Rows[0])
	}
}
