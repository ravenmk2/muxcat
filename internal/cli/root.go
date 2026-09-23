// Package cli assembles muxcat's cobra command tree: root, the config
// group, connector registration and mounting, and the unified error exit.
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
)

// Runtime is the command runtime environment, stored in the command context.
type Runtime struct {
	// TTY is true if and only if both stdin and stdout are terminals.
	TTY bool
	// Interactive indicates interactive prompts are allowed; globally
	// disabled when not on a TTY.
	Interactive bool
}

type runtimeKey struct{}

// WithRuntime stores the runtime environment in a context.
func WithRuntime(ctx context.Context, rt *Runtime) context.Context {
	return context.WithValue(ctx, runtimeKey{}, rt)
}

// RuntimeFrom extracts the runtime environment; when absent it returns a
// safe non-TTY default.
func RuntimeFrom(ctx context.Context) *Runtime {
	if rt, ok := ctx.Value(runtimeKey{}).(*Runtime); ok {
		return rt
	}
	return &Runtime{}
}

// NewRoot builds the root command and mounts all subcommands.
func NewRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "muxcat",
		Short: "Universal CLI client for infrastructure backends",
		// The first line carries the version in plain text, without styling.
		Long:          "🐱 muxcat — universal CLI client for infrastructure backends (" + version + ")",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
			cmd.SetContext(WithRuntime(cmd.Context(), &Runtime{TTY: tty, Interactive: tty}))
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	pf := root.PersistentFlags()
	pf.StringP("conn", "c", "", "connection name (a connection of a connector instance)")
	pf.String("output", "auto", "output mode: auto|table|plain|tsv|json")
	pf.Bool("json", false, "shortcut for --output json; takes precedence over --output")
	pf.Duration("timeout", DefaultTimeout, "command timeout")
	pf.Int("limit", DefaultLimit, "maximum number of rows returned by queries")
	pf.Bool("yes", false, "skip confirmation prompts and assume yes")
	pf.Bool("no-color", false, "disable colored output")

	root.AddCommand(newConfigCmd())
	root.AddCommand(newConnectorCmd())
	for _, c := range connector.Commands() {
		root.AddCommand(c)
	}
	registerHelpTemplate(root)
	return root
}

// Execute runs the root command and returns the exit code. It is the
// program's single error exit: structured errors returned by commands are
// rendered here uniformly (envelope to stdout with --json, plain text to
// stderr otherwise) and mapped to exit codes by error code.
func Execute(version string) int {
	root := NewRoot(version)
	if err := root.Execute(); err != nil {
		return exitError(root, err)
	}
	return output.ExitOK
}

func exitError(root *cobra.Command, err error) int {
	e := output.ToError(err)
	jsonOut, _ := root.PersistentFlags().GetBool("json")
	if jsonOut {
		_ = output.WriteEnvelope(os.Stdout, output.Failure(e, output.Meta{}))
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", e.Message)
		if e.Hint != "" {
			fmt.Fprintf(os.Stderr, "Hint: %s\n", e.Hint)
		}
	}
	return output.ExitCode(e)
}

// ExactArgs validates the positional argument count; a missing argument
// yields MISSING_ARGUMENT + hint, guaranteeing non-TTY runs fail fast
// instead of waiting for input.
func ExactArgs(n int, usage string, names ...string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return output.NewError(output.CodeMissingArgument,
				fmt.Sprintf("missing required argument <%s> (usage: %s %s)", names[len(args)], cmd.CommandPath(), usage),
				"see "+cmd.CommandPath()+" --help for full usage")
		}
		if len(args) > n {
			return output.NewError(output.CodeMissingArgument,
				fmt.Sprintf("too many arguments (usage: %s %s)", cmd.CommandPath(), usage),
				"see "+cmd.CommandPath()+" --help for full usage")
		}
		return nil
	}
}
