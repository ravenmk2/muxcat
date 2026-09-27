package nacos

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/ravenmk2/muxcat/internal/output"
)

// cellStyle is the nacos connector's cell presentation: value-based
// coloring for the semantic columns of the list tables.
var cellStyle = &output.CellStyle{ColumnStyle: columnStyle}

var (
	grayStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	greenStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	redStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// typeColors maps a config format to its cell color (ANSI 16).
var typeColors = map[string]lipgloss.Style{
	"yaml":       greenStyle,
	"json":       lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
	"xml":        lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
	"properties": lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
	"toml":       lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
}

// columnStyle colors the semantic cells of the list tables: the default
// group fades to gray, config types get per-format colors, and the
// healthy/enabled flags go green/red. Only active when color is on;
// anything unrecognized falls through to the default formatting.
func columnStyle(column string, v any, color bool) (string, bool) {
	if !color {
		return "", false
	}
	switch column {
	case "group":
		if s, ok := v.(string); ok && s == "DEFAULT_GROUP" {
			return grayStyle.Render(s), true
		}
	case "type":
		if s, ok := v.(string); ok {
			if st, ok := typeColors[s]; ok {
				return st.Render(s), true
			}
		}
	case "healthy", "enabled":
		if b, ok := v.(bool); ok {
			if b {
				return greenStyle.Render("true"), true
			}
			return redStyle.Render("false"), true
		}
	}
	return "", false
}
