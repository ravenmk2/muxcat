package mqtt

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
		Short: "Manage mqtt connections",
		Long: `Manage mqtt connections. conn add creates a same-named
instance (broker url + protocol version) and connection
(credentials, client id, policies) in one step; one instance can
back multiple connections (different users). Passwords are stored
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
		Example: `  muxcat mqtt conn add local --url mqtt://127.0.0.1:1883 --set-default
  muxcat mqtt conn add prod --url mqtts://broker.internal:8883 --protocol-version 5 --username deploy --password s3cr3t --set-default
  muxcat mqtt conn add ws --url ws://broker.internal:8083/mqtt --readonly`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Connections[name]; exists {
				return output.NewError(output.CodeConfigInvalid,
					"connection already exists: "+name, "remove it first with muxcat mqtt conn rm "+name)
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
			protocolVersion, _ := cmd.Flags().GetInt("protocol-version")
			username := cli.FlagString(cmd, "username")
			password := cli.FlagString(cmd, "password")
			clientIDFlag := cli.FlagString(cmd, "client-id")
			readonly := cli.FlagBool(cmd, "readonly")
			if rawURL == "" {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"missing required flag --url (usage: muxcat mqtt conn add <name> --url <url>)",
						"--url is required in non-interactive environments")
				}
				form, err := promptAddForm()
				if err != nil {
					return err
				}
				rawURL = form.url
				if !cmd.Flags().Changed("protocol-version") {
					protocolVersion = form.protocolVersion
				}
				if !cmd.Flags().Changed("username") {
					username = form.username
				}
				if !cmd.Flags().Changed("password") {
					password = form.password
				}
				if !cmd.Flags().Changed("readonly") {
					readonly = form.readonly
				}
			}
			base, err := normalizeURL(rawURL)
			if err != nil {
				return err
			}
			if perr := validateProtocolVersion(protocolVersion); perr != nil {
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
			if tlsSkipVerify && !isTLSScheme(schemeOf(Instance{URL: base})) {
				return output.NewError(output.CodeConfigInvalid,
					"--tls-skip-verify requires an mqtts:// or wss:// url",
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

			cfg.Instances[name] = Instance{URL: base, ProtocolVersion: protocolVersion}
			cfg.Connections[name] = Connection{
				Instance:      name,
				Username:      username,
				Password:      encPassword,
				ClientID:      clientIDFlag,
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
				Message: fmt.Sprintf("added connection %s (%s, protocol %s)", name, base,
					protocolName(Instance{ProtocolVersion: protocolVersion}.protocolVersion())),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("url", "", "broker URL (mqtt://host:1883, mqtts://host:8883, ws://host:8083/mqtt, wss://...)")
	c.Flags().Int("protocol-version", 0, "MQTT protocol version: 3 (3.1.1, default) | 5 (5.0)")
	c.Flags().String("username", "", "MQTT username")
	c.Flags().String("password", "", "password (plaintext flag; stored encrypted)")
	c.Flags().String("client-id", "", "MQTT client id (default: random muxcat-<pid>-<rand> per session)")
	c.Flags().Bool("readonly", false, "allow read operations only")
	c.Flags().String("timeout", "", "command timeout for this connection, overrides the global --timeout (e.g. 5s)")
	c.Flags().Bool("tls-skip-verify", false, "skip TLS certificate verification (mqtts/wss only; insecure)")
	c.Flags().Bool("set-default", false, "set as the default connection")
	return c
}

// protocolName renders a protocol version for display ("3" -> "3.1.1").
func protocolName(v int) string {
	if v == ProtocolV5 {
		return "5.0"
	}
	return "3.1.1"
}

// addForm holds the answers of the interactive conn add form.
type addForm struct {
	url             string
	protocolVersion int
	username        string
	password        string
	readonly        bool
}

// promptAddForm fills in missing arguments with a huh form, only on a TTY.
func promptAddForm() (addForm, error) {
	form := addForm{protocolVersion: ProtocolV3}
	f := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("URL (mqtt://, mqtts://, ws:// or wss://)").
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("url must not be empty")
				}
				return nil
			}).
			Value(&form.url),
		huh.NewSelect[int]().
			Title("Protocol version").
			Options(
				huh.NewOption("MQTT 3.1.1", ProtocolV3),
				huh.NewOption("MQTT 5.0", ProtocolV5),
			).
			Value(&form.protocolVersion),
		huh.NewInput().Title("Username (optional)").Value(&form.username),
		huh.NewInput().
			Title("Password (optional)").
			EchoMode(huh.EchoModePassword).
			Value(&form.password),
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
		Example: `  muxcat mqtt conn ls
  muxcat mqtt conn ls --json`,
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
					protocol = protocolName(inst.protocolVersion())
				}
				rows = append(rows, []any{n, urlStr, protocol, conn.Username, conn.Readonly, def})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "url", "protocol", "username", "readonly", "default"},
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
		Example: `  muxcat mqtt conn show local
  muxcat mqtt conn show local --json`,
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
				"name":          name,
				"instance":      conn.Instance,
				"url":           inst.URL,
				"protocol":      protocolName(inst.protocolVersion()),
				"username":      conn.Username,
				"clientId":      conn.ClientID,
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
		Example: `  muxcat mqtt conn rm legacy --yes`,
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
		Example: `  muxcat mqtt conn default prod`,
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
		Short: "Test a connection (real dial + CONNACK) and report latency",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat mqtt conn test local
  muxcat mqtt conn test prod --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForConn(cmd, args[0], "")
			if err != nil {
				return err
			}
			defer env.close()
			latency := time.Since(start).Milliseconds()
			protocol := protocolName(env.inst.protocolVersion())
			value := map[string]any{
				"ok": true, "latency_ms": latency,
				"protocol": protocol, "transport": schemeOf(env.inst),
			}
			message := fmt.Sprintf("connection ok (%d ms, MQTT %s over %s)",
				latency, protocol, schemeOf(env.inst))
			return cli.RenderResult(cmd, &output.Result{
				Value:   value,
				Message: message,
			}, meta(env.name, start, false))
		},
	}
}

func connNotFound(name string) *output.Error {
	return output.NewError(output.CodeConnNotFound,
		"connection not found: "+name, "list connections with muxcat mqtt conn ls")
}
