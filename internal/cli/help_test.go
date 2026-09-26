package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
)

func runHelp(t *testing.T, args ...string) string {
	t.Helper()
	t.Setenv("MUXCAT_HOME", t.TempDir())
	// Force color off deterministically: helpTTY() inspects the process
	// stdio, which may be a real terminal on a developer machine.
	t.Setenv("NO_COLOR", "1")
	root := NewRoot("test")
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("help %v: %v", args, err)
	}
	return buf.String()
}

// Help on a non-TTY must contain no ANSI escapes, keep the cobra default
// sections, and include the root emoji.
func TestHelpPlainNonTTY(t *testing.T) {
	out := runHelp(t, "--help")
	if strings.Contains(out, "\x1b") {
		t.Fatalf("non-TTY help must not contain ANSI escapes:\n%q", out)
	}
	if !strings.Contains(out, "🐱") {
		t.Fatalf("root help should contain the 🐱 emoji:\n%s", out)
	}
	for _, section := range []string{"Usage:", "Available Commands:", "Flags:"} {
		if !strings.Contains(out, section) {
			t.Fatalf("help missing section %q:\n%s", section, out)
		}
	}
}

// The first header line ends with the version in parentheses, as plain text.
func TestHelpHeaderVersion(t *testing.T) {
	out := runHelp(t, "--help")
	firstLine, _, _ := strings.Cut(out, "\n")
	want := "🐱 muxcat — universal CLI client for infrastructure backends (test)"
	if firstLine != want {
		t.Fatalf("header line = %q, want %q", firstLine, want)
	}
	if strings.Contains(firstLine, "\x1b") {
		t.Fatalf("version in header must be unstyled: %q", firstLine)
	}
}

// Subcommand help inherits the template and stays plain off-TTY.
func TestSubcommandHelpPlainNonTTY(t *testing.T) {
	out := runHelp(t, "config", "--help")
	if strings.Contains(out, "\x1b") {
		t.Fatalf("non-TTY help must not contain ANSI escapes:\n%q", out)
	}
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "Available Commands:") {
		t.Fatalf("subcommand help missing sections:\n%s", out)
	}
	if !strings.Contains(out, "Global Flags:") {
		t.Fatalf("subcommand help missing inherited flags section:\n%s", out)
	}
}

// The colored branch: with a forced color profile the theme must emit ANSI
// escapes; with color off the style functions must be the identity.
func TestHelpThemeBranches(t *testing.T) {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	theme := newHelpTheme(r)

	if got := theme.headerText("Usage:"); !strings.Contains(got, "\x1b[") {
		t.Fatalf("colored header should contain ANSI escapes: %q", got)
	}
	if got := theme.nameText("sqlite"); !strings.Contains(got, "\x1b[") {
		t.Fatalf("colored name should contain ANSI escapes: %q", got)
	}
	flagsIn := "  -c, --conn string   connection name\n      --json          shortcut"
	got := theme.flags(flagsIn)
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("colored flags should contain ANSI escapes: %q", got)
	}
	// Both the shorthand and the long form get styled.
	if strings.Count(got, "\x1b[") < 4 {
		t.Fatalf("expected multiple styled tokens: %q", got)
	}
}

func TestFlagTokenRegex(t *testing.T) {
	matches := flagTokenRe.FindAllString("  -c, --conn string   text\n      --json", -1)
	if len(matches) != 3 {
		t.Fatalf("matches = %v, want 3", matches)
	}
}

// TestHelpConvention enforces the help-information convention (see
// docs/architecture.md) on the framework tree: every command has a
// Short, every command group has a Long, and every runnable leaf has an
// Example (or a Long covering the same ground). Help topics are
// non-runnable and exempt. This package does not import the connector
// implementations, so the tree here holds cli's own commands only; the
// full tree including connectors is checked by the same convention in
// cmd/muxcat's TestHelpConvention.
func TestHelpConvention(t *testing.T) {
	var check func(c *cobra.Command)
	check = func(c *cobra.Command) {
		if c.Short == "" {
			t.Errorf("%s: missing Short", c.CommandPath())
		}
		switch {
		case c.HasAvailableSubCommands():
			if c.Long == "" {
				t.Errorf("%s: command group missing Long", c.CommandPath())
			}
		case c.Runnable():
			if c.Example == "" && c.Long == "" {
				t.Errorf("%s: leaf command needs an Example (or Long)", c.CommandPath())
			}
		}
		for _, sub := range c.Commands() {
			check(sub)
		}
	}
	root := NewRoot("test")
	check(root)
}

// The help topics are reachable through `muxcat help <topic>` and appear
// under root help's additional-topics section.
func TestHelpTopics(t *testing.T) {
	out := runHelp(t, "--help")
	if !strings.Contains(out, "Additional help topics:") ||
		!strings.Contains(out, "output") || !strings.Contains(out, "errors") {
		t.Fatalf("root help should list the output/errors topics:\n%s", out)
	}
	for _, topic := range []string{"output", "errors"} {
		out := runHelp(t, "help", topic)
		if !strings.Contains(out, "envelope") && !strings.Contains(out, "Exit codes") {
			t.Fatalf("help %s: unexpected content:\n%s", topic, out)
		}
	}
}
