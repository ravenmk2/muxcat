package jenkins

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
		Short: "Manage jenkins connections",
		Long: `Manage jenkins connections. conn add creates a same-named
instance (endpoint url) and connection (credentials, policies) in
one step; one instance can back multiple connections (different
users). API tokens are stored encrypted and never echoed by
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
		Example: `  muxcat jenkins conn add local --url http://127.0.0.1:8080 --username admin --set-default
  muxcat jenkins conn add prod --url https://jenkins.internal --username deploy --token 11abc... --timeout 30s --set-default
  muxcat jenkins conn add ro --url http://127.0.0.1:8080 --username reader --readonly`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Connections[name]; exists {
				return output.NewError(output.CodeConfigInvalid,
					"connection already exists: "+name, "remove it first with muxcat jenkins conn rm "+name)
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
			username := cli.FlagString(cmd, "username")
			token := cli.FlagString(cmd, "token")
			readonly := cli.FlagBool(cmd, "readonly")
			if rawURL == "" {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"missing required flag --url (usage: muxcat jenkins conn add <name> --url <url>)",
						"--url is required in non-interactive environments")
				}
				form, err := promptAddForm()
				if err != nil {
					return err
				}
				rawURL = form.url
				if !cmd.Flags().Changed("username") {
					username = form.username
				}
				if !cmd.Flags().Changed("token") {
					token = form.token
				}
				if !cmd.Flags().Changed("readonly") {
					readonly = form.readonly
				}
			}
			base, err := normalizeURL(rawURL)
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

			encToken := ""
			if token != "" {
				// Only the flag carries a plaintext credential (form input
				// does not); it is encrypted before being written to disk.
				if cmd.Flags().Changed("token") {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
						"Warning: --token passes the credential in plaintext; prefer an interactive prompt or edit "+FileName)
				}
				key, _, err := masterKey()
				if err != nil {
					return err
				}
				enc, err := secret.Encrypt(key, []byte(token))
				if err != nil {
					return err
				}
				encToken = enc
			}

			cfg.Instances[name] = Instance{URL: base}
			cfg.Connections[name] = Connection{
				Instance: name,
				Username: username,
				Token:    encToken,
				Readonly: readonly,
				Timeout:  timeout,
			}
			if cli.FlagBool(cmd, "set-default") || cfg.DefaultConnection == "" {
				cfg.DefaultConnection = name
			}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("added connection %s (%s)", name, base),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("url", "", "base URL of the Jenkins server (e.g. http://127.0.0.1:8080)")
	c.Flags().String("username", "", "Jenkins username")
	c.Flags().String("token", "", "API token (plaintext flag; stored encrypted)")
	c.Flags().Bool("readonly", false, "allow read requests only")
	c.Flags().String("timeout", "", "command timeout for this connection, overrides the global --timeout (e.g. 5s)")
	c.Flags().Bool("set-default", false, "set as the default connection")
	return c
}

// addForm holds the answers of the interactive conn add form.
type addForm struct {
	url      string
	username string
	token    string
	readonly bool
}

// promptAddForm fills in missing arguments with a huh form, only on a TTY.
func promptAddForm() (addForm, error) {
	var form addForm
	f := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("URL").
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("url must not be empty")
				}
				return nil
			}).
			Value(&form.url),
		huh.NewInput().Title("Username (optional)").Value(&form.username),
		huh.NewInput().
			Title("API token (optional)").
			EchoMode(huh.EchoModePassword).
			Value(&form.token),
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
		Example: `  muxcat jenkins conn ls
  muxcat jenkins conn ls --json`,
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
				urlStr := "?"
				if inst, ok := cfg.Instances[conn.Instance]; ok {
					urlStr = inst.URL
				}
				rows = append(rows, []any{n, urlStr, conn.Username, conn.Readonly, def})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "url", "username", "readonly", "default"},
				Rows:    rows,
			}, meta("", start, false))
		},
	}
}

func newConnShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show connection details (the token is never echoed)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat jenkins conn show local
  muxcat jenkins conn show local --json`,
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
				"name":     name,
				"instance": conn.Instance,
				"url":      inst.URL,
				"username": conn.Username,
				"readonly": conn.Readonly,
				"timeout":  conn.Timeout,
				"default":  name == cfg.DefaultConnection,
			}, Syntax: "yaml"}, meta(name, start, false))
		},
	}
}

func newConnRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a connection (its instance is removed too when unreferenced)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat jenkins conn rm ro --yes`,
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
		Example: `  muxcat jenkins conn default prod`,
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
		Short: "Test a connection (auth check + read server version) and report latency",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat jenkins conn test local
  muxcat jenkins conn test prod --json`,
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
			cl, err := newClient(cfg, conn, timeout)
			if err != nil {
				return err
			}
			// Auth check: whoAmI reports the identity the token maps to,
			// which is the surest way to confirm the credential works.
			user := conn.Username
			if r, err := cl.do(cmd.Context(), "GET", "/whoAmI/api/json?tree=authenticated,fullName", nil); err != nil {
				return err
			} else if v, derr := decodeBody(r.body); derr == nil {
				if m, ok := v.(map[string]any); ok {
					if fn, _ := m["fullName"].(string); fn != "" {
						user = fn
					}
				}
			}
			// Version is best-effort: every Jenkins response carries the
			// X-Jenkins header; a missing header must not fail an
			// otherwise healthy test.
			version, note := "", ""
			if r, verr := cl.do(cmd.Context(), "GET", "/api/json?tree=mode", nil); verr == nil {
				version = r.headers["X-Jenkins"]
			}
			if version == "" {
				note = "version header unavailable"
			}
			latency := time.Since(start).Milliseconds()
			value := map[string]any{"ok": true, "user": user, "latency_ms": latency, "version": version}
			message := fmt.Sprintf("connection ok (%d ms, user %s, jenkins %s)", latency, user, version)
			if note != "" {
				value["note"] = note
				message = fmt.Sprintf("connection ok (%d ms, user %s, %s)", latency, user, note)
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   value,
				Message: message,
			}, meta(name, start, false))
		},
	}
}

func connNotFound(name string) *output.Error {
	return output.NewError(output.CodeConnNotFound,
		"connection not found: "+name, "list connections with muxcat jenkins conn ls")
}
