package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
)

// TestHelpConvention enforces the help-information convention (see
// docs/architecture.md) on the full command tree: every command has a
// Short, every command group has a Long, and every runnable leaf has an
// Example (or a Long covering the same ground). Help topics are
// non-runnable and exempt.
//
// The test lives in package main because connectors register through
// main.go's blank imports; a tree built without them would silently
// check nothing. The connector-presence assertion keeps the check from
// degenerating back into a vacuous pass.
func TestHelpConvention(t *testing.T) {
	root := cli.NewRoot("test")
	mounted := map[string]bool{}
	for _, c := range root.Commands() {
		mounted[c.Name()] = true
	}
	for _, name := range []string{"mysql", "openobserve", "redis", "sqlite"} {
		if !mounted[name] {
			t.Fatalf("connector %q is not mounted; the convention check would be vacuous", name)
		}
	}

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
	check(root)
}
