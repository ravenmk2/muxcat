package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Result is what a command hands to a Renderer.
// The tabular carrier (Columns+Rows) is used by table/plain/tsv rendering;
// for JSON mode the envelope data prefers JSONData (e.g. query's rich
// shape), then normalized Columns+Rows, then Value, then Message.
// Bare asks text renderers to print a single-entry Value map as the bare
// value without its label (e.g. redis get prints the value, not "value: x").
// Syntax names a chroma lexer (json, yaml, toml, ...) for syntax
// highlighting in text modes; it only takes effect when color is on.
// Neither Bare nor Syntax affects JSON rendering.
type Result struct {
	Columns  []string
	Rows     [][]any
	JSONData any
	Value    any
	Message  string
	Bare     bool
	Syntax   string
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

func cellString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
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

// tsvRenderer renders tab-separated text with a header row.
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
			cells[i] = cellString(v)
		}
		if _, err := fmt.Fprintln(w, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return nil
}

// plainRenderer renders unadorned aligned text (the non-TTY degraded form).
type plainRenderer struct {
	color bool
}

func (p plainRenderer) Render(w io.Writer, r *Result) error {
	if r.Columns == nil {
		return renderFallback(w, r, p.color)
	}
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = len(c)
	}
	for _, row := range r.Rows {
		for i, v := range row {
			if i < len(widths) && len(cellString(v)) > widths[i] {
				widths[i] = len(cellString(v))
			}
		}
	}
	writeRow := func(cells []string) error {
		parts := make([]string, len(cells))
		for i, c := range cells {
			if i < len(cells)-1 {
				parts[i] = fmt.Sprintf("%-*s", widths[i], c)
			} else {
				parts[i] = c
			}
		}
		_, err := fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
		return err
	}
	if err := writeRow(r.Columns); err != nil {
		return err
	}
	for _, row := range r.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = cellString(v)
		}
		if err := writeRow(cells); err != nil {
			return err
		}
	}
	return nil
}
