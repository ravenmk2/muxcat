// Package connector maintains the connector registry: name → cobra command
// tree factory. Each connector implementation package registers itself in
// init(); the root command mounts all registered connectors when assembled.
package connector

import (
	"sort"

	"github.com/spf13/cobra"
)

// Factory builds the cobra command tree of a connector.
type Factory func() *cobra.Command

var factories = map[string]Factory{}

// Register registers a connector. Registering the same name twice panics:
// it is a programming error and should surface during development.
func Register(name string, f Factory) {
	if _, dup := factories[name]; dup {
		panic("connector: duplicate registration " + name)
	}
	factories[name] = f
}

// Names returns the names of all registered connectors, sorted.
func Names() []string {
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Commands builds the command trees of all registered connectors,
// in the same order as Names().
func Commands() []*cobra.Command {
	names := Names()
	cmds := make([]*cobra.Command, 0, len(names))
	for _, name := range names {
		cmds = append(cmds, factories[name]())
	}
	return cmds
}
