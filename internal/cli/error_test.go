package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ravenmk2/muxcat/internal/output"
)

// The colored branch highlights the Error:/Hint: labels; the plain branch
// stays byte-identical to the pre-color contract.
func TestRenderError(t *testing.T) {
	e := &output.Error{Code: output.CodeConnectFailed, Message: "boom", Hint: "try again"}

	plain := &bytes.Buffer{}
	renderError(plain, e, false)
	if got, want := plain.String(), "Error: boom\nHint: try again\n"; got != want {
		t.Fatalf("plain renderError = %q, want %q", got, want)
	}

	colored := &bytes.Buffer{}
	renderError(colored, e, true)
	out := colored.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("colored renderError must contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "Error:") || !strings.Contains(out, "Hint:") {
		t.Fatalf("colored renderError must keep the labels: %q", out)
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "try again") {
		t.Fatalf("colored renderError must keep message bodies: %q", out)
	}

	noHint := &bytes.Buffer{}
	renderError(noHint, &output.Error{Message: "boom"}, false)
	if got, want := noHint.String(), "Error: boom\n"; got != want {
		t.Fatalf("renderError without hint = %q, want %q", got, want)
	}
}
