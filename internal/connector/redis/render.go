package redis

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// defaultMaxBytes is the per-value byte cap for --max-bytes; 0 disables
// truncation.
const defaultMaxBytes = 4096

// addBinaryFlags registers the value-rendering flags shared by the data
// commands.
func addBinaryFlags(c *cobra.Command) {
	c.Flags().String("binary", "hex", "encoding for non-UTF-8 values: hex|base64")
	c.Flags().Int("max-bytes", defaultMaxBytes, "maximum bytes kept per value; 0 disables truncation")
}

// addSyntaxFlag registers --syntax on value-returning commands.
func addSyntaxFlag(c *cobra.Command) {
	c.Flags().String("syntax", "", "syntax highlight string values: json|yaml|toml, none disables, empty auto-detects JSON")
}

// resolveSyntax maps --syntax to a chroma lexer name. Empty/auto sniffs
// JSON (first non-space byte is { or [ and the value parses); none and
// undetectable content disable highlighting. The flag value is validated
// even when s carries no detectable syntax, so callers can fail fast.
func resolveSyntax(s, flag string) (string, error) {
	switch flag {
	case "", "auto":
		t := strings.TrimSpace(s)
		if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
			return "json", nil
		}
		return "", nil
	case "none":
		return "", nil
	case "json", "yaml", "toml":
		return flag, nil
	default:
		return "", output.NewError(output.CodeMissingArgument,
			"invalid --syntax value: "+flag, "valid values: json|yaml|toml|none|auto")
	}
}

// checkSyntaxFlag validates --syntax before dialing.
func checkSyntaxFlag(cmd *cobra.Command) error {
	_, err := resolveSyntax("", cli.FlagString(cmd, "syntax"))
	return err
}

// renderOpts resolves and validates the value-rendering flags.
func renderOpts(cmd *cobra.Command) (binary string, maxBytes int, err error) {
	binary = cli.FlagString(cmd, "binary")
	maxBytes, _ = cmd.Flags().GetInt("max-bytes")
	switch binary {
	case "hex", "base64":
	default:
		return "", 0, output.NewError(output.CodeConfigInvalid,
			"invalid --binary value: "+binary, "valid values: hex|base64")
	}
	if maxBytes < 0 {
		return "", 0, output.NewError(output.CodeConfigInvalid,
			"invalid --max-bytes value: must be >= 0", "")
	}
	return binary, maxBytes, nil
}

// renderString renders one raw value: displayable UTF-8 (all runes
// printable, plus \n \t \r) is emitted as-is, other bytes are encoded (hex
// by default, base64 via --binary); values beyond maxBytes (when > 0) are
// truncated first. The bool reports truncation (→ meta.truncated).
func renderString(s string, binary string, maxBytes int) (string, bool) {
	if isDisplayable(s) {
		if maxBytes <= 0 || len(s) <= maxBytes {
			return s, false
		}
		return truncateUTF8(s, maxBytes), true
	}
	b := []byte(s)
	truncated := false
	if maxBytes > 0 && len(b) > maxBytes {
		b = b[:maxBytes]
		truncated = true
	}
	if binary == "base64" {
		return base64.StdEncoding.EncodeToString(b), truncated
	}
	return hex.EncodeToString(b), truncated
}

// isDisplayable reports whether s is valid UTF-8 with every rune printable
// or whitespace (\n, \t, \r); values with control characters (e.g. \x01)
// fall back to binary encoding so invisible bytes never reach the output.
func isDisplayable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsPrint(r) || r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		return false
	}
	return true
}

// truncateUTF8 cuts s to at most max bytes, backing off to a rune boundary
// so the result stays valid UTF-8.
func truncateUTF8(s string, max int) string {
	out := s[:max]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[:len(out)-1]
	}
	return out
}

// reply is the {type, value} data shape of exec/eval results; type ∈
// string/integer/double/boolean/array/map/null. go-redis generic replies
// do not distinguish simple strings from bulk strings, so both report
// "string".
type reply struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// renderReply converts a go-redis generic command result (Do/Eval) into
// the {type, value} shape, applying renderString recursively to every
// string element of arrays and maps. The bool reports whether any element
// was truncated. The covered Go types mirror go-redis's ReadReply: string,
// int64, float64, bool, *big.Int, []any, map[any]any, nil.
func renderReply(v any, binary string, maxBytes int) (reply, bool) {
	switch t := v.(type) {
	case nil:
		return reply{Type: "null"}, false
	case string:
		s, truncated := renderString(t, binary, maxBytes)
		return reply{Type: "string", Value: s}, truncated
	case int64:
		return reply{Type: "integer", Value: t}, false
	case float64:
		return reply{Type: "double", Value: t}, false
	case bool:
		return reply{Type: "boolean", Value: t}, false
	case *big.Int:
		// RESP3 big numbers have no JSON counterpart; emit the decimal form.
		return reply{Type: "integer", Value: t.String()}, false
	case []any:
		items := make([]any, len(t))
		truncated := false
		for i, e := range t {
			r, tr := renderReply(e, binary, maxBytes)
			items[i] = r
			truncated = truncated || tr
		}
		return reply{Type: "array", Value: items}, truncated
	case map[any]any:
		m := make(map[string]any, len(t))
		truncated := false
		for k, e := range t {
			ks, keyTrunc := renderString(fmt.Sprint(k), binary, maxBytes)
			r, valTrunc := renderReply(e, binary, maxBytes)
			m[ks] = r
			truncated = truncated || keyTrunc || valTrunc
		}
		return reply{Type: "map", Value: m}, truncated
	default:
		return reply{Type: "string", Value: fmt.Sprint(v)}, false
	}
}
