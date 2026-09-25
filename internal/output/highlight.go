package output

import (
	"bytes"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/muesli/termenv"
)

// highlight colorizes s with the named chroma lexer, choosing a formatter
// from the terminal's color profile and a style matching its background.
// s is returned unchanged when the lexer is unknown or highlighting fails.
func highlight(s, lexerName string) string {
	lexer := lexers.Get(lexerName)
	if lexer == nil {
		return s
	}
	var formatter chroma.Formatter
	switch termenv.ColorProfile() {
	case termenv.TrueColor:
		formatter = formatters.Get("terminal16m")
	case termenv.ANSI256:
		formatter = formatters.Get("terminal256")
	default:
		formatter = formatters.Get("terminal")
	}
	styleName := "github"
	if termenv.HasDarkBackground() {
		styleName = "github-dark"
	}
	it, err := lexer.Tokenise(nil, s)
	if err != nil {
		return s
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, styles.Get(styleName), it); err != nil {
		return s
	}
	return strings.TrimRight(buf.String(), "\n")
}
