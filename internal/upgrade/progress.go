package upgrade

import (
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/bubbles/progress"
)

// Bar is a single-line download progress display written to w (stderr in
// practice, keeping stdout clean for piping). When disabled it is a no-op.
type Bar struct {
	w        io.Writer
	model    progress.Model
	enabled  bool
	last     time.Time
	finished bool
}

// NewBar builds a progress bar. enabled should be false for non-TTY or JSON
// output; color controls the gradient fill (off keeps the bar unstyled).
func NewBar(w io.Writer, enabled, color bool) *Bar {
	opts := []progress.Option{progress.WithWidth(40)}
	if color {
		opts = append(opts, progress.WithGradient("#7D56F4", "#04B575"))
	}
	return &Bar{w: w, model: progress.New(opts...), enabled: enabled}
}

// Update renders the bar, throttled to ~15fps unless the download is done.
// total 0 (unknown length) falls back to a plain byte counter.
func (b *Bar) Update(received, total int64) {
	if !b.enabled || b.finished {
		return
	}
	done := total > 0 && received >= total
	if !done && time.Since(b.last) < 66*time.Millisecond {
		return
	}
	b.last = time.Now()
	if total > 0 {
		_, _ = fmt.Fprintf(b.w, "\r%s", b.model.ViewAs(float64(received)/float64(total)))
		return
	}
	_, _ = fmt.Fprintf(b.w, "\rdownloaded %s", humanBytes(received))
}

// Finish terminates the bar line with a newline.
func (b *Bar) Finish() {
	if b.enabled && !b.finished {
		b.finished = true
		_, _ = fmt.Fprintln(b.w)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB"} {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, suffix)
		}
	}
	return fmt.Sprintf("%.1f TB", v/unit)
}
