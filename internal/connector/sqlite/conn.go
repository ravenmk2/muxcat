package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newConnCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "conn",
		Short: "Manage sqlite connections",
		Long: `Manage sqlite connections. conn add creates a same-named instance
(database file path) and connection (readonly policy) in one
step. The default connection is used when -c/--conn is not
passed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnAddCmd(),
		newConnLsCmd(),
		newConnShowCmd(),
		newConnRmCmd(),
		newConnDefaultCmd(),
		newConnTestCmd(),
	)
	return c
}

func newConnAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a connection (creates an instance of the same name)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat sqlite conn add local --path ./app.db --set-default
  muxcat sqlite conn add shared --path ~/data/team.db
  muxcat sqlite conn add ro --path ./app.db --readonly`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Connections[name]; exists {
				return output.NewError(output.CodeConfigInvalid,
					"connection already exists: "+name, "remove it first with muxcat sqlite conn rm "+name)
			}

			path := cli.FlagString(cmd, "path")
			readonly := cli.FlagBool(cmd, "readonly")
			if path == "" {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"missing required flag --path (usage: muxcat sqlite conn add <name> --path <file>)",
						"--path is required in non-interactive environments")
				}
				p, ro, err := promptAddForm()
				if err != nil {
					return err
				}
				path = p
				if !cmd.Flags().Changed("readonly") {
					readonly = ro
				}
			}
			path = ExpandHome(strings.TrimSpace(path))

			cfg.Instances[name] = Instance{Path: path}
			cfg.Connections[name] = Connection{Instance: name, Readonly: readonly}
			if cli.FlagBool(cmd, "set-default") || cfg.DefaultConnection == "" {
				cfg.DefaultConnection = name
			}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("added connection %s (%s)", name, path),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("path", "", "database file path (supports ~ expansion)")
	c.Flags().Bool("readonly", false, "open read-only (mode=ro)")
	c.Flags().Bool("set-default", false, "set as the default connection")
	return c
}

// promptAddForm fills in missing arguments with a huh form, only on a TTY.
func promptAddForm() (path string, readonly bool, err error) {
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Database file path").
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("path must not be empty")
				}
				return nil
			}).
			Value(&path),
		huh.NewConfirm().
			Title("Read-only mode?").
			Value(&readonly),
	))
	err = form.Run()
	return path, readonly, err
}

func newConnLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all connections",
		Args:  cobra.NoArgs,
		Example: `  muxcat sqlite conn ls
  muxcat sqlite conn ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Connections))
			for n := range cfg.Connections {
				names = append(names, n)
			}
			sort.Strings(names)
			rows := make([][]any, 0, len(names))
			for _, n := range names {
				conn := cfg.Connections[n]
				def := ""
				if n == cfg.DefaultConnection {
					def = "*"
				}
				path, _ := cfg.instancePath(conn)
				rows = append(rows, []any{n, path, conn.Readonly, def})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "path", "readonly", "default"},
				Rows:    rows,
			}, meta("", start, false))
		},
	}
}

func newConnShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show connection details",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat sqlite conn show local
  muxcat sqlite conn show local --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			conn, ok := cfg.Connections[name]
			if !ok {
				return connNotFound(name)
			}
			path, err := cfg.instancePath(conn)
			if err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{Value: map[string]any{
				"name":     name,
				"instance": conn.Instance,
				"path":     path,
				"readonly": conn.Readonly,
				"timeout":  conn.Timeout,
				"default":  name == cfg.DefaultConnection,
			}}, meta(name, start, false))
		},
	}
}

func newConnRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a connection (its instance is removed too when unreferenced)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat sqlite conn rm ro --yes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			conn, ok := cfg.Connections[name]
			if !ok {
				return connNotFound(name)
			}
			if !cli.FlagBool(cmd, "yes") {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"removing a connection requires confirmation", "pass --yes in non-interactive environments")
				}
				confirm := false
				form := huh.NewForm(huh.NewGroup(
					huh.NewConfirm().Title("Remove connection " + name + "?").Value(&confirm),
				))
				if err := form.Run(); err != nil {
					return err
				}
				if !confirm {
					return output.NewError(output.CodeGeneral, "cancelled", "")
				}
			}

			delete(cfg.Connections, name)
			// Remove the same-named instance when no other connection
			// references it.
			referenced := false
			for _, c := range cfg.Connections {
				if c.Instance == conn.Instance {
					referenced = true
					break
				}
			}
			if !referenced {
				delete(cfg.Instances, conn.Instance)
			}
			if cfg.DefaultConnection == name {
				cfg.DefaultConnection = ""
				for n := range cfg.Connections {
					cfg.DefaultConnection = n
					break
				}
			}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: "removed connection " + name,
			}, meta(name, start, false))
		},
	}
}

func newConnDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "default <name>",
		Short:   "Set the default connection",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat sqlite conn default local`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, ok := cfg.Connections[name]; !ok {
				return connNotFound(name)
			}
			cfg.DefaultConnection = name
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: "default connection set to " + name,
			}, meta(name, start, false))
		},
	}
}

func newConnTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Test a connection (open + SELECT 1) and report latency",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat sqlite conn test local
  muxcat sqlite conn test ro --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name, conn, err := resolve(cfg, args[0])
			if err != nil {
				return err
			}
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			db, _, err := openDB(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			var one int
			if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
				return classifyErr(err, "connection test failed")
			}
			latency := time.Since(start).Milliseconds()
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"ok": true, "latency_ms": latency},
				Message: fmt.Sprintf("connection ok (%d ms)", latency),
			}, meta(name, start, false))
		},
	}
}

func connNotFound(name string) *output.Error {
	return output.NewError(output.CodeConnNotFound,
		"connection not found: "+name, "list connections with muxcat sqlite conn ls")
}
