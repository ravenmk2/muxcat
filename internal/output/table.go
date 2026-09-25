package output

import (
	"fmt"
	"io"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
)

// tableRenderer renders a bordered table with a colored header via the
// lipgloss table package.
type tableRenderer struct {
	color bool
}

func (t *tableRenderer) Render(w io.Writer, r *Result) error {
	if r.Columns == nil {
		return renderFallback(w, r, t.color)
	}
	rows := make([][]string, len(r.Rows))
	for i, row := range r.Rows {
		cells := make([]string, len(row))
		for j, v := range row {
			cells[j] = cellString(v)
		}
		rows[i] = cells
	}
	tbl := table.New().
		Border(lipgloss.NormalBorder()).
		Headers(r.Columns...).
		Rows(rows...)
	if t.color {
		headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
		tbl = tbl.StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return headerStyle
			}
			return lipgloss.NewStyle()
		})
	}
	_, err := fmt.Fprintln(w, tbl)
	return err
}
