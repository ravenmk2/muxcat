package amqp

import (
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
		Short: "Manage amqp connections",
		Long: `Manage amqp connections. conn add creates a same-named
instance (broker url + protocol) and connection (credentials,
vhost, policies) in one step; one instance can back multiple
connections (different users or vhosts). Passwords are stored
encrypted and never echoed by ls/show. The default connection is
used when -c/--conn is not passed.`,
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
		Example: `  muxcat amqp conn add local --url amqp://127.0.0.1:5672 --username admin --set-default
  muxcat amqp conn add prod --url amqps://broker.internal:5671 --protocol 1.0 --username deploy --password s3cr3t --vhost /prod --set-default
  muxcat amqp conn add legacy --url amqp://10.0.0.8:5672 --protocol 0.9.1 --username guest --readonly`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Connections[name]; exists {
				return output.NewError(output.CodeConfigInvalid,
					"connection already exists: "+name, "remove it first with muxcat amqp conn rm "+name)
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

			rawURL := strings.TrimSpace(cli.FlagString(cmd, "url"))
			protocol := cli.FlagString(cmd, "protocol")
			username := cli.FlagString(cmd, "username")
			password := cli.FlagString(cmd, "password")
			vhost := cli.FlagString(cmd, "vhost")
			readonly := cli.FlagBool(cmd, "readonly")
			if rawURL == "" {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"missing required flag --url (usage: muxcat amqp conn add <name> --url <url>)",
						"--url is required in non-interactive environments")
				}
				form, err := promptAddForm()
				if err != nil {
					return err
				}
				rawURL = form.url
				if !cmd.Flags().Changed("protocol") {
					protocol = form.protocol
				}
				if !cmd.Flags().Changed("username") {
					username = form.username
				}
				if !cmd.Flags().Changed("password") {
					password = form.password
				}
				if !cmd.Flags().Changed("vhost") {
					vhost = form.vhost
				}
				if !cmd.Flags().Changed("readonly") {
					readonly = form.readonly
				}
			}
			base, err := normalizeURL(rawURL)
			if err != nil {
				return err
			}
			if perr := validateProtocol(protocol); perr != nil {
				return perr
			}
			timeout := cli.FlagString(cmd, "timeout")
			if timeout != "" {
				if d, err := time.ParseDuration(timeout); err != nil || d <= 0 {
					return output.NewError(output.CodeConfigInvalid,
						"invalid --timeout value: "+timeout, "examples: 5s, 1m (must be > 0)")
				}
			}
			tlsSkipVerify := cli.FlagBool(cmd, "tls-skip-verify")
			if tlsSkipVerify && !strings.HasPrefix(base, "amqps://") {
				return output.NewError(output.CodeConfigInvalid,
					"--tls-skip-verify requires an amqps:// url",
					"certificate verification can only be skipped on TLS connections")
			}

			encPassword := ""
			if password != "" {
				// Only the flag carries a plaintext credential (form input
				// does not); it is encrypted before being written to disk.
				if cmd.Flags().Changed("password") {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
						"Warning: --password passes the credential in plaintext; prefer an interactive prompt or edit "+FileName)
				}
				key, _, err := masterKey()
				if err != nil {
					return err
				}
				enc, err := secret.Encrypt(key, []byte(password))
				if err != nil {
					return err
				}
				encPassword = enc
			}

			cfg.Instances[name] = Instance{URL: base, Protocol: protocol}
			cfg.Connections[name] = Connection{
				Instance:      name,
				Username:      username,
				Password:      encPassword,
				Vhost:         vhost,
				Readonly:      readonly,
				Timeout:       timeout,
				TLSSkipVerify: tlsSkipVerify,
			}
			if cli.FlagBool(cmd, "set-default") || cfg.DefaultConnection == "" {
				cfg.DefaultConnection = name
			}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("added connection %s (%s, protocol %s)", name, base, Instance{Protocol: protocol}.protocol()),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("url", "", "broker URL (e.g. amqp://127.0.0.1:5672 or amqps://broker.example.com:5671)")
	c.Flags().String("protocol", "", `wire protocol: "0.9.1" | "1.0" (default "1.0"; 1.0 requires RabbitMQ >= 4.0)`)
	c.Flags().String("username", "", "AMQP username")
	c.Flags().String("password", "", "password (plaintext flag; stored encrypted)")
	c.Flags().String("vhost", "", `default vhost for this connection (default "/")`)
	c.Flags().Bool("readonly", false, "allow read operations only")
	c.Flags().String("timeout", "", "command timeout for this connection, overrides the global --timeout (e.g. 5s)")
	c.Flags().Bool("tls-skip-verify", false, "skip TLS certificate verification (amqps only; insecure)")
	c.Flags().Bool("set-default", false, "set as the default connection")
	return c
}

// addForm holds the answers of the interactive conn add form.
type addForm struct {
	url      string
	protocol string
	username string
	password string
	vhost    string
	readonly bool
}

// promptAddForm fills in missing arguments with a huh form, only on a TTY.
func promptAddForm() (addForm, error) {
	form := addForm{protocol: Protocol10, vhost: "/"}
	f := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("URL (amqp:// or amqps://)").
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("url must not be empty")
				}
				return nil
			}).
			Value(&form.url),
		huh.NewSelect[string]().
			Title("Protocol").
			Options(
				huh.NewOption("AMQP 1.0 (RabbitMQ >= 4.0)", Protocol10),
				huh.NewOption("AMQP 0.9.1", Protocol091),
			).
			Value(&form.protocol),
		huh.NewInput().Title("Username (optional)").Value(&form.username),
		huh.NewInput().
			Title("Password (optional)").
			EchoMode(huh.EchoModePassword).
			Value(&form.password),
		huh.NewInput().Title(`Vhost (default "/")`).Value(&form.vhost),
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
		Example: `  muxcat amqp conn ls
  muxcat amqp conn ls --json`,
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
				urlStr, protocol := "?", "?"
				if inst, ok := cfg.Instances[conn.Instance]; ok {
					urlStr = inst.URL
					protocol = inst.protocol()
				}
				vhost := conn.Vhost
				if vhost == "" {
					vhost = "/"
				}
				rows = append(rows, []any{n, urlStr, protocol, conn.Username, vhost, conn.Readonly, def})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "url", "protocol", "username", "vhost", "readonly", "default"},
				Rows:    rows,
			}, meta("", start, false))
		},
	}
}

func newConnShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show connection details (the password is never echoed)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp conn show local
  muxcat amqp conn show local --json`,
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
			vhost := conn.Vhost
			if vhost == "" {
				vhost = "/"
			}
			return cli.RenderResult(cmd, &output.Result{Value: map[string]any{
				"name":          name,
				"instance":      conn.Instance,
				"url":           inst.URL,
				"protocol":      inst.protocol(),
				"username":      conn.Username,
				"vhost":         vhost,
				"readonly":      conn.Readonly,
				"timeout":       conn.Timeout,
				"tlsSkipVerify": conn.TLSSkipVerify,
				"default":       name == cfg.DefaultConnection,
			}, Syntax: "yaml"}, meta(name, start, false))
		},
	}
}

func newConnRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a connection (its instance is removed too when unreferenced)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp conn rm legacy --yes`,
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
		Example: `  muxcat amqp conn default prod`,
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
	c := &cobra.Command{
		Use:   "test <name>",
		Short: "Test a connection (real handshake + server properties) and report latency",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp conn test local
  muxcat amqp conn test prod --vhost /prod --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForConn(cmd, args[0], "")
			if err != nil {
				return err
			}
			defer env.close()
			product, version := env.session.ServerInfo()
			latency := time.Since(start).Milliseconds()
			value := map[string]any{
				"ok": true, "latency_ms": latency,
				"product": product, "version": version,
				"protocol": env.inst.protocol(), "vhost": env.vhost,
			}
			message := fmt.Sprintf("connection ok (%d ms, %s %s, protocol %s, vhost %s)",
				latency, product, version, env.inst.protocol(), env.vhost)
			return cli.RenderResult(cmd, &output.Result{
				Value:   value,
				Message: message,
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	return c
}

func connNotFound(name string) *output.Error {
	return output.NewError(output.CodeConnNotFound,
		"connection not found: "+name, "list connections with muxcat amqp conn ls")
}
