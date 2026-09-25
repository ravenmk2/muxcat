package output

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// termWidth returns the terminal width for table auto-truncation; a
// variable so tests can stub it.
var termWidth = func() (int, bool) {
	w, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil || w <= 0 {
		return 0, false
	}
	return w, true
}

// minColumnWidth is the lower bound a table column is never compressed
// below (the column name's own width still wins when wider).
const minColumnWidth = 8

// tableRenderer renders a bordered table with a colored header via the
// lipgloss table package. Cells are formatted via the Result's attached
// CellStyle (legacy default otherwise); when the terminal width is known,
// over-wide tables are compressed to fit.
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
			cells[j] = r.formatCell(v, j, t.color)
		}
		rows[i] = cells
	}
	if width, ok := termWidth(); ok {
		truncateColumns(r.Columns, rows, width)
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

// truncateColumns compresses columns (widest first, down to a per-column
// minimum) so the bordered table fits the terminal width, cutting cells
// with an ANSI-aware truncate and a … tail. Purely presentational: the
// data is untouched, and plain/tsv/json outputs are never truncated.
func truncateColumns(headers []string, rows [][]string, termW int) {
	// Border chars (n+1) plus the lipgloss table's default cell padding
	// (1 space each side, 2n).
	overhead := 3*len(headers) + 1
	natural := make([]int, len(headers))
	for j, h := range headers {
		natural[j] = lipgloss.Width(h)
	}
	for _, row := range rows {
		for j, cell := range row {
			if j < len(natural) && lipgloss.Width(cell) > natural[j] {
				natural[j] = lipgloss.Width(cell)
			}
		}
	}
	target := make([]int, len(headers))
	copy(target, natural)
	for sumWidths(target)+overhead > termW {
		// Shrink the widest column that is still above its minimum.
		widest, width := -1, 0
		for j, n := range target {
			if minW := max(minColumnWidth, lipgloss.Width(headers[j])); n > minW && n > width {
				widest, width = j, n
			}
		}
		if widest < 0 {
			break // everything is at its minimum; accept the overflow
		}
		target[widest]--
	}
	for _, row := range rows {
		for j, cell := range row {
			if j < len(target) && lipgloss.Width(cell) > target[j] {
				row[j] = ansi.Truncate(cell, target[j], "…")
			}
		}
	}
}

func sumWidths(widths []int) int {
	total := 0
	for _, w := range widths {
		total += w
	}
	return total
}
