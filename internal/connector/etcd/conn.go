package etcd

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
	"github.com/ravenmk2/muxcat/internal/secret"
)

func newConnCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "conn",
		Short: "Manage etcd connections",
		Long: `Manage etcd connections. conn add creates a same-named instance
(endpoints, TLS material) and connection (credentials, policies) in
one step; one instance can back multiple connections (e.g. admin +
readonly user). Passwords are stored encrypted and never echoed by
ls/show. The default connection is used when -c/--conn is not
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
		Example: `  muxcat etcd conn add local --endpoints 127.0.0.1:2379 --set-default
  muxcat etcd conn add prod --endpoints h1.example.com:2379,h2.example.com:2379 --tls --cacert ./ca.pem
  muxcat etcd conn add ro --endpoints 127.0.0.1:2379 --username alice --password s3cret --readonly`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Connections[name]; exists {
				return output.NewError(output.CodeConfigInvalid,
					"connection already exists: "+name, "remove it first with muxcat etcd conn rm "+name)
			}
			// Refuse to overwrite a same-named instance while another
			// connection references it; an unreferenced (orphan) instance
			// may be replaced, treating the add as a rebuild.
			if _, instExists := cfg.Instances[name]; instExists {
				for n, c := range cfg.Connections {
					if c.Instance == name {
						return output.NewError(output.CodeConfigInvalid,
							fmt.Sprintf("instance %q already exists and is referenced by connection %q", name, n),
							"choose another name, or remove connection "+n+" first")
					}
				}
			}

			endpointsFlag := strings.TrimSpace(cli.FlagString(cmd, "endpoints"))
			username := cli.FlagString(cmd, "username")
			password := cli.FlagString(cmd, "password")
			tlsOn := cli.FlagBool(cmd, "tls")
			readonly := cli.FlagBool(cmd, "readonly")
			if endpointsFlag == "" {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"missing required flag --endpoints (usage: muxcat etcd conn add <name> --endpoints <host:port>[,<host:port>...])",
						"--endpoints is required in non-interactive environments")
				}
				form, err := promptAddForm()
				if err != nil {
					return err
				}
				endpointsFlag = form.endpoints
				if !cmd.Flags().Changed("username") {
					username = form.username
				}
				if !cmd.Flags().Changed("password") {
					password = form.password
				}
				if !cmd.Flags().Changed("tls") {
					tlsOn = form.tls
				}
				if !cmd.Flags().Changed("readonly") {
					readonly = form.readonly
				}
			}
			endpoints, err := parseEndpoints(endpointsFlag)
			if err != nil {
				return err
			}
			timeout := cli.FlagString(cmd, "timeout")
			if timeout != "" {
				if d, err := time.ParseDuration(timeout); err != nil || d <= 0 {
					return output.NewError(output.CodeConfigInvalid,
						"invalid --timeout value: "+timeout, "examples: 5s, 1m (must be > 0)")
				}
			}
			cacert := cli.FlagString(cmd, "cacert")
			cert := cli.FlagString(cmd, "cert")
			key := cli.FlagString(cmd, "key")
			if !tlsOn && (cacert != "" || cert != "" || key != "") {
				return output.NewError(output.CodeConfigInvalid,
					"--cacert/--cert/--key require --tls", "")
			}
			if (cert == "") != (key == "") {
				return output.NewError(output.CodeConfigInvalid,
					"client TLS requires both --cert and --key", "")
			}

			encPassword := ""
			if password != "" {
				// Only the flag carries a plaintext credential (form input
				// does not); it is encrypted before being written to disk.
				if cmd.Flags().Changed("password") {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
						"Warning: --password passes the credential in plaintext; prefer an interactive prompt or edit "+FileName)
				}
				mk, _, err := masterKey()
				if err != nil {
					return err
				}
				enc, err := secret.Encrypt(mk, []byte(password))
				if err != nil {
					return err
				}
				encPassword = enc
			}

			cfg.Instances[name] = Instance{
				Endpoints: endpoints,
				TLS:       tlsOn,
				CACert:    cacert,
				Cert:      cert,
				Key:       key,
			}
			conn := Connection{
				Instance:       name,
				Username:       username,
				Password:       encPassword,
				Readonly:       readonly,
				AllowDangerous: cli.FlagBool(cmd, "allow-dangerous"),
				Timeout:        timeout,
			}
			cfg.Connections[name] = conn
			if cli.FlagBool(cmd, "set-default") || cfg.DefaultConnection == "" {
				cfg.DefaultConnection = name
			}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("added connection %s (%s)", name, endpointsText(cfg.Instances[name])),
			}, meta(cfg, conn, name, start, false))
		},
	}
	c.Flags().String("endpoints", "", "comma-separated endpoint list (host:port[,host:port...])")
	c.Flags().String("username", "", "auth username (empty = no auth)")
	c.Flags().String("password", "", "password (plaintext flag; stored encrypted)")
	c.Flags().Bool("tls", false, "enable TLS")
	c.Flags().String("cacert", "", "CA certificate file (requires --tls)")
	c.Flags().String("cert", "", "client certificate file (requires --tls and --key)")
	c.Flags().String("key", "", "client key file (requires --tls and --cert)")
	c.Flags().Bool("readonly", false, "allow read commands only")
	c.Flags().Bool("allow-dangerous", false, "allow dangerous operations (del --prefix)")
	c.Flags().String("timeout", "", "command timeout for this connection, overrides the global --timeout (e.g. 5s)")
	c.Flags().Bool("set-default", false, "set as the default connection")
	return c
}

// addForm holds the answers of the interactive conn add form.
type addForm struct {
	endpoints string
	username  string
	password  string
	tls       bool
	readonly  bool
}

// promptAddForm fills in missing arguments with a huh form, only on a TTY.
func promptAddForm() (addForm, error) {
	var form addForm
	f := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Endpoints (comma-separated host:port)").
			Validate(func(s string) error {
				if _, err := parseEndpoints(s); err != nil {
					return errors.New(output.ToError(err).Message)
				}
				return nil
			}).
			Value(&form.endpoints),
		huh.NewInput().Title("Username (optional)").Value(&form.username),
		huh.NewInput().
			Title("Password (optional)").
			EchoMode(huh.EchoModePassword).
			Value(&form.password),
		huh.NewConfirm().Title("TLS?").Value(&form.tls),
		huh.NewConfirm().Title("Read-only?").Value(&form.readonly),
	))
	if err := f.Run(); err != nil {
		return form, err
	}
	return form, nil
}

func newConnLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all connections",
		Args:  cobra.NoArgs,
		Example: `  muxcat etcd conn ls
  muxcat etcd conn ls --json`,
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
				endpoints, user, tlsOn := "?", conn.Username, false
				if inst, ok := cfg.Instances[conn.Instance]; ok {
					endpoints = endpointsText(inst)
					tlsOn = inst.TLS
				}
				rows = append(rows, []any{n, endpoints, user, tlsOn, conn.Readonly, def})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "endpoints", "user", "tls", "readonly", "default"},
				Rows:    rows,
			}, meta(cfg, Connection{}, "", start, false))
		},
	}
}

func newConnShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show connection details (the password is never echoed)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat etcd conn show local
  muxcat etcd conn show local --json`,
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
			inst, err := cfg.instanceOf(conn)
			if err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{Value: map[string]any{
				"name":           name,
				"instance":       conn.Instance,
				"endpoints":      inst.Endpoints,
				"tls":            inst.TLS,
				"username":       conn.Username,
				"readonly":       conn.Readonly,
				"allowDangerous": conn.AllowDangerous,
				"timeout":        conn.Timeout,
				"default":        name == cfg.DefaultConnection,
			}}, meta(cfg, conn, name, start, false))
		},
	}
}

func newConnRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a connection (its instance is removed too when unreferenced)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat etcd conn rm ro --yes`,
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
			}, meta(cfg, conn, name, start, false))
		},
	}
}

func newConnDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "default <name>",
		Short:   "Set the default connection",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat etcd conn default prod`,
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
			}, meta(cfg, cfg.Connections[name], name, start, false))
		},
	}
}

func newConnTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Test a connection (maintenance Status) and report latency and version",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat etcd conn test local
  muxcat etcd conn test prod --json`,
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
			client, err := openClient(ctx, cfg, conn, timeout)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			inst, err := cfg.instanceOf(conn)
			if err != nil {
				return err
			}
			status, err := client.Status(ctx, inst.Endpoints[0])
			if err != nil {
				return classifyErr(err, "connection test failed")
			}
			latency := time.Since(start).Milliseconds()
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"ok": true, "latency_ms": latency, "version": status.Version},
				Message: fmt.Sprintf("connection ok (%d ms, etcd %s)", latency, status.Version),
			}, meta(cfg, conn, name, start, false))
		},
	}
}

func connNotFound(name string) *output.Error {
	return output.NewError(output.CodeConnNotFound,
		"connection not found: "+name, "list connections with muxcat etcd conn ls")
}
