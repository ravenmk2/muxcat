package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/output"
)

// partialCmd builds a command with the output flags resolveRenderer reads.
func partialCmd(jsonOut bool) (*cobra.Command, *bytes.Buffer) {
	c := &cobra.Command{Use: "partial-test"}
	c.Flags().String("output", "auto", "")
	c.Flags().Bool("json", jsonOut, "")
	c.Flags().Bool("no-color", false, "")
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetContext(context.Background())
	return c, buf
}

// TestRenderPartial covers the degraded-result contract: json mode attaches
// the payload to the returned error (nothing rendered, the failure envelope
// carries it); text modes render the partial result and pass the error
// through unchanged.
func TestRenderPartial(t *testing.T) {
	res := &output.Result{
		Columns: []string{"endpoint", "error"},
		Rows:    [][]any{{"127.0.0.1:1", "boom"}},
	}
	cause := output.NewError(output.CodeConnectFailed, "1 of 1 endpoints failed", "")

	cmd, buf := partialCmd(true)
	err := RenderPartial(cmd, res, output.Meta{}, cause)
	if err == nil {
		t.Fatal("json mode should return the error")
	}
	e := output.ToError(err)
	if e.Code != output.CodeConnectFailed {
		t.Fatalf("code = %s, want %s", e.Code, output.CodeConnectFailed)
	}
	payload, ok := e.Data.(map[string]any)
	if !ok || payload["columns"] == nil || payload["rows"] == nil {
		t.Fatalf("json mode should attach the payload to the error: %+v", e.Data)
	}
	if buf.Len() != 0 {
		t.Fatalf("json mode should not render, got %q", buf.String())
	}

	cmd, buf = partialCmd(false)
	err = RenderPartial(cmd, res, output.Meta{}, cause)
	if err != cause {
		t.Fatalf("text mode should pass the error through, got %v", err)
	}
	if !strings.Contains(buf.String(), "127.0.0.1:1") || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("text mode should render the partial result, got %q", buf.String())
	}

	// a generic error is normalized and still carries the payload
	cmd, _ = partialCmd(true)
	err = RenderPartial(cmd, res, output.Meta{}, errors.New("plain failure"))
	if e := output.ToError(err); e.Code != output.CodeGeneral || e.Data == nil {
		t.Fatalf("generic error should wrap as GENERAL with Data: %+v", e)
	}
}
