package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Result is what a command hands to a Renderer.
// The tabular carrier (Columns+Rows) is used by table/plain/tsv rendering;
// for JSON mode the envelope data prefers JSONData (e.g. query's rich
// shape), then normalized Columns+Rows, then Value, then Message.
// ColumnTypes optionally carries the columns' DatabaseTypeName() values,
// parallel to Columns; it feeds type-aware formatting (binary detection,
// DATE layout) when a CellStyle is attached.
// CellStyle is the connector's opt-in cell presentation; nil means the
// legacy default formatting (nil -> "", []byte -> string, no coloring).
// Bare asks text renderers to print a single-entry Value map as the bare
// value without its label (e.g. redis get prints the value, not "value: x").
// Syntax names a chroma lexer (json, yaml, toml, ...) for syntax
// highlighting in text modes; it only takes effect when color is on.
// Neither Bare nor Syntax affects JSON rendering.
type Result struct {
	Columns     []string
	ColumnTypes []string
	Rows        [][]any
	JSONData    any
	Value       any
	Message     string
	Bare        bool
	Syntax      string
	CellStyle   *CellStyle
}

// Payload returns the normalized payload of the Result, used by JSON
// rendering and the envelope's data field.
func (r *Result) Payload() any {
	switch {
	case r.JSONData != nil:
		return r.JSONData
	case r.Columns != nil:
		rows := r.Rows
		if rows == nil {
			rows = [][]any{}
		}
		return map[string]any{"columns": r.Columns, "rows": rows}
	case r.Value != nil:
		return r.Value
	case r.Message != "":
		return map[string]string{"message": r.Message}
	default:
		return nil
	}
}

// Renderer renders a command result to w.
type Renderer interface {
	Render(w io.Writer, r *Result) error
}

// NewRenderer constructs a Renderer for the given mode. color only affects
// table mode.
func NewRenderer(mode Mode, color bool) Renderer {
	switch mode {
	case ModeJSON:
		return jsonRenderer{}
	case ModeTSV:
		return tsvRenderer{color: color}
	case ModeTable:
		return &tableRenderer{color: color}
	default:
		return plainRenderer{color: color}
	}
}

// columnType returns the db type name of column j, "" when untyped.
func (r *Result) columnType(j int) string {
	if j < len(r.ColumnTypes) {
		return r.ColumnTypes[j]
	}
	return ""
}

// formatCell formats a tabular cell via the attached CellStyle, or the
// legacy default when none is attached (nil receiver).
func (r *Result) formatCell(v any, j int, color bool) string {
	return r.CellStyle.FormatCell(v, r.columnType(j), color)
}

// cellString formats an untyped non-tabular value (legacy default).
func cellString(v any) string {
	return legacyCell(v)
}

// renderFallback handles non-tabular carriers (Value/Message); shared by
// tsv/plain/table. When color is on and Syntax names a lexer, the text is
// syntax-highlighted before writing.
func renderFallback(w io.Writer, r *Result, color bool) error {
	var s string
	switch {
	case r.Message != "":
		s = r.Message
	case r.Value != nil:
		if m, ok := r.Value.(map[string]any); ok {
			if r.Bare && len(m) == 1 {
				for _, v := range m {
					s = cellString(v)
				}
			} else {
				var b strings.Builder
				for _, k := range sortedKeys(m) {
					fmt.Fprintf(&b, "%s: %v\n", k, m[k])
				}
				s = strings.TrimRight(b.String(), "\n")
			}
		} else {
			s = cellString(r.Value)
		}
	default:
		return nil
	}
	if color && r.Syntax != "" && s != "" {
		s = highlight(s, r.Syntax)
	}
	_, err := fmt.Fprintln(w, s)
	return err
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jsonRenderer renders the normalized payload (without the envelope; the
// envelope is wrapped by the command layer).
type jsonRenderer struct{}

func (jsonRenderer) Render(w io.Writer, r *Result) error {
	data, err := json.MarshalIndent(r.Payload(), "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// tsvRenderer renders tab-separated text with a header row. Cells are
// formatted type-aware but never colored.
type tsvRenderer struct {
	color bool
}

func (t tsvRenderer) Render(w io.Writer, r *Result) error {
	if r.Columns == nil {
		return renderFallback(w, r, t.color)
	}
	if _, err := fmt.Fprintln(w, strings.Join(r.Columns, "\t")); err != nil {
		return err
	}
	for _, row := range r.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = r.formatCell(v, i, false)
		}
		if _, err := fmt.Fprintln(w, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return nil
}

// plainRenderer renders unadorned aligned text (the non-TTY degraded form).
// Widths are measured ANSI-aware (lipgloss.Width) so colored cells align.
type plainRenderer struct {
	color bool
}

func (p plainRenderer) Render(w io.Writer, r *Result) error {
	if r.Columns == nil {
		return renderFallback(w, r, p.color)
	}
	rows := make([][]string, len(r.Rows))
	for i, row := range r.Rows {
		cells := make([]string, len(row))
		for j, v := range row {
			cells[j] = r.formatCell(v, j, p.color)
		}
		rows[i] = cells
	}
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = lipgloss.Width(c)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && lipgloss.Width(cell) > widths[i] {
				widths[i] = lipgloss.Width(cell)
			}
		}
	}
	writeRow := func(cells []string) error {
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = c
			if i < len(cells)-1 && i < len(widths) {
				parts[i] += strings.Repeat(" ", widths[i]-lipgloss.Width(c))
			}
		}
		_, err := fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
		return err
	}
	if err := writeRow(r.Columns); err != nil {
		return err
	}
	for _, row := range rows {
		if err := writeRow(row); err != nil {
			return err
		}
	}
	return nil
}
