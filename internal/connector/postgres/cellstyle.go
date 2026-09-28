package postgres

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/ravenmk2/muxcat/internal/output"
)

// cellStyle is the postgres connector's declared cell presentation,
// aligned with the mysql connector's contract: a NULL marker, 0x hex
// binary (bytea), psql-like datetime layouts (timestamptz keeps its
// timezone offset), and per-type coloring on a color TTY.
var cellStyle = &output.CellStyle{
	NullText:     "NULL",
	BinaryHex:    true,
	DateLayout:   "2006-01-02",
	TimeLayout:   "2006-01-02 15:04:05.999999",
	TimeTZLayout: "2006-01-02 15:04:05.999999 -07:00",
	Palette: &output.CellPalette{
		Null:     lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true),
		Number:   lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		String:   lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		Temporal: lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		Binary:   lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
	},
}
