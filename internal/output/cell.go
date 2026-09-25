package output

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// CellStyle controls how tabular cells are formatted in text modes. It is
// a per-connector opt-in: a nil *CellStyle on Result means the legacy
// default formatting.
type CellStyle struct {
	NullText   string       // rendering for SQL NULL, e.g. "NULL"; legacy default is ""
	BinaryHex  bool         // binary-typed []byte -> 0x uppercase hex; legacy default false (raw string conversion)
	DateLayout string       // Go layout for time.Time on DATE columns, e.g. "2006-01-02"
	TimeLayout string       // Go layout for other time.Time, e.g. "2006-01-02 15:04:05.999999"
	Palette    *CellPalette // per-category lipgloss styles; nil = no cell coloring
}

// CellPalette holds the per-category styles applied when color is on.
type CellPalette struct {
	Null, Number, String, Temporal, Binary lipgloss.Style
}

// FormatCell renders a result cell for text output. dbType is the column's
// DatabaseTypeName() ("" = untyped); color enables the palette. A nil
// receiver yields the legacy default: nil -> "", []byte -> string,
// everything else fmt.Sprint, never colored.
func (s *CellStyle) FormatCell(v any, dbType string, color bool) string {
	if s == nil {
		return legacyCell(v)
	}
	// A zero-value Style renders its input unchanged, so a nil palette (or
	// color off) naturally degrades to plain text.
	pal := CellPalette{}
	if color && s.Palette != nil {
		forceColorProfile()
		pal = *s.Palette
	}
	switch val := v.(type) {
	case nil:
		return pal.Null.Render(s.NullText)
	case time.Time:
		layout := s.TimeLayout
		if strings.EqualFold(dbType, "DATE") && s.DateLayout != "" {
			layout = s.DateLayout
		}
		if layout == "" {
			return fmt.Sprint(val)
		}
		return pal.Temporal.Render(val.Format(layout))
	case []byte:
		if s.BinaryHex && IsBinaryType(dbType) {
			return pal.Binary.Render(BinaryHex(val))
		}
		return pal.String.Render(string(val))
	case string:
		return pal.String.Render(val)
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return pal.Number.Render(fmt.Sprint(val))
	default:
		return fmt.Sprint(val)
	}
}

// legacyCell is the default cell formatting without an attached CellStyle.
func legacyCell(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

// forceProfileOnce guards forceColorProfile.
var forceProfileOnce sync.Once

// forceColorProfile forces lipgloss's default renderer to TrueColor so the
// already-made color decision cannot be vetoed by stdout-based detection
// (same pattern as cli.renderError). Palette styles consult the renderer's
// profile at Render time, so this also covers styles built earlier.
func forceColorProfile() {
	forceProfileOnce.Do(func() {
		lipgloss.DefaultRenderer().SetColorProfile(termenv.TrueColor)
	})
}

// binaryTypes are the DatabaseTypeName() tokens whose []byte values render
// as 0x hex (the official mysql cli's --binary-as-hex semantics).
var binaryTypes = map[string]bool{
	"BINARY": true, "VARBINARY": true,
	"TINYBLOB": true, "BLOB": true, "MEDIUMBLOB": true, "LONGBLOB": true,
	"GEOMETRY": true, "BIT": true,
}

// IsBinaryType reports whether a database type name is a binary type.
// Multi-token names (e.g. "UNSIGNED BIGINT") are checked token by token.
func IsBinaryType(dbType string) bool {
	for _, tok := range strings.Fields(strings.ToUpper(dbType)) {
		if binaryTypes[tok] {
			return true
		}
	}
	return false
}

// BinaryHex renders binary bytes as an uppercase 0x hex literal, matching
// the official mysql cli's --binary-as-hex output.
func BinaryHex(b []byte) string {
	return "0x" + strings.ToUpper(hex.EncodeToString(b))
}

// BinaryCellsToHex returns a copy of rows with every []byte cell converted
// to its 0x hex string, for JSON output (raw []byte would base64-encode,
// and lossy string conversion corrupts non-UTF-8 data into �). It is an
// opt-in helper: connectors call it explicitly for their JSON rows.
func BinaryCellsToHex(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		cells := make([]any, len(row))
		for j, v := range row {
			if b, ok := v.([]byte); ok {
				cells[j] = BinaryHex(b)
			} else {
				cells[j] = v
			}
		}
		out[i] = cells
	}
	return out
}
