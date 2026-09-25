package sqlite

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/ravenmk2/muxcat/internal/output"
)

// cellStyle is the sqlite connector's declared cell presentation. It
// matches the mysql connector's content today (NULL marker, 0x hex BLOB,
// cli datetime layouts, per-type coloring) but is a deliberately separate
// instance so the two can evolve independently.
var cellStyle = &output.CellStyle{
	NullText:   "NULL",
	BinaryHex:  true,
	DateLayout: "2006-01-02",
	TimeLayout: "2006-01-02 15:04:05.999999",
	Palette: &output.CellPalette{
		Null:     lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true),
		Number:   lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		String:   lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		Temporal: lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		Binary:   lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
	},
}
