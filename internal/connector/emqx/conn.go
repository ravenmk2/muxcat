package emqx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
		Short: "Manage emqx connections",
		Long: `Manage emqx connections. conn add creates a same-named
instance (endpoint url) and connection (credentials, policies) in one
step; one instance can back multiple connections (different users or
API keys). A connection needs at least one complete credential pair:
a dashboard username/password pair and/or an API key/secret pair.
password and apiSecret are stored encrypted and never echoed by
ls/show; apiKey is treated like a username. The default connection is
used when -c/--conn is not passed.

Quickstart:
  1. muxcat emqx conn add local --url http://127.0.0.1:18083 --username admin --set-default
  2. muxcat emqx conn ls
  3. muxcat emqx conn test local`,
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
		Example: `  muxcat emqx conn add local --url http://127.0.0.1:18083 --username admin --set-default
  muxcat emqx conn add prod --url https://emqx.internal:18083 --api-key KEY --api-secret SECRET --timeout 30s
  muxcat emqx conn add ro --url http://127.0.0.1:18083 --username viewer --readonly`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Connections[name]; exists {
				return output.NewError(output.CodeConfigInvalid,
					"connection already exists: "+name, "remove it first with muxcat emqx conn rm "+name)
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
			password := cli.FlagString(cmd, "password")
			apiKey := cli.FlagString(cmd, "api-key")
			apiSecret := cli.FlagString(cmd, "api-secret")
			readonly := cli.FlagBool(cmd, "readonly")
			if rawURL == "" {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"missing required flag --url (usage: muxcat emqx conn add <name> --url <url>)",
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
				if !cmd.Flags().Changed("password") {
					password = form.password
				}
				if !cmd.Flags().Changed("api-key") {
					apiKey = form.apiKey
				}
				if !cmd.Flags().Changed("api-secret") {
					apiSecret = form.apiSecret
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
			// At least one complete credential pair is required.
			if _, _, err := credPairsOf(Connection{
				Username: username, Password: password,
				APIKey: apiKey, APISecret: apiSecret,
			}); err != nil {
				return err
			}

			// Only the flags carry plaintext credentials (form input does
			// not); they are encrypted before being written to disk.
			encPassword, err := encryptFlag(cmd, "password", password)
			if err != nil {
				return err
			}
			encAPISecret, err := encryptFlag(cmd, "api-secret", apiSecret)
			if err != nil {
				return err
			}

			cfg.Instances[name] = Instance{URL: base}
			cfg.Connections[name] = Connection{
				Instance:  name,
				Username:  username,
				Password:  encPassword,
				APIKey:    apiKey,
				APISecret: encAPISecret,
				Readonly:  readonly,
				Timeout:   timeout,
			}
			if cli.FlagBool(cmd, "set-default") || cfg.DefaultConnection == "" {
				cfg.DefaultConnection = name
			}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("added connection %s (%s, %s)", name, base, authLabel(cfg.Connections[name])),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("url", "", "base URL of the EMQX HTTP API (server root, typically the dashboard port, e.g. http://127.0.0.1:18083)")
	c.Flags().String("username", "", "dashboard username (with --password, a complete dashboard pair)")
	c.Flags().String("password", "", "dashboard password (plaintext flag; stored encrypted)")
	c.Flags().String("api-key", "", "API key (with --api-secret, a complete API key pair; stored unencrypted, like a username)")
	c.Flags().String("api-secret", "", "API secret (plaintext flag; stored encrypted)")
	c.Flags().Bool("readonly", false, "allow read operations only")
	c.Flags().String("timeout", "", "command timeout for this connection, overrides the global --timeout (e.g. 5s)")
	c.Flags().Bool("set-default", false, "set as the default connection")
	return c
}

// encryptFlag encrypts a plaintext credential flag value into an enc:v1:
// blob, warning on stderr that a plaintext flag was used.
func encryptFlag(cmd *cobra.Command, flag, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if cmd.Flags().Changed(flag) {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"Warning: --"+flag+" passes the credential in plaintext; prefer an interactive prompt or edit "+FileName)
	}
	key, _, err := masterKey()
	if err != nil {
		return "", err
	}
	enc, err := secret.Encrypt(key, []byte(value))
	if err != nil {
		return "", err
	}
	return enc, nil
}

// addForm holds the answers of the interactive conn add form.
type addForm struct {
	url       string
	username  string
	password  string
	apiKey    string
	apiSecret string
	readonly  bool
}

// promptAddForm fills in missing arguments with a huh form, only on a
// TTY. The credential-kind selector decides which pair to prompt for
// (dashboard, API key, or both).
func promptAddForm() (addForm, error) {
	var form addForm
	kind := "dashboard"
	if err := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("URL").
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("url must not be empty")
				}
				return nil
			}).
			Value(&form.url),
		huh.NewSelect[string]().
			Title("Credentials").
			Options(
				huh.NewOption("dashboard (username + password)", "dashboard"),
				huh.NewOption("apikey (apiKey + apiSecret)", "apikey"),
				huh.NewOption("both", "both"),
			).
			Value(&kind),
	)).Run(); err != nil {
		return form, err
	}
	var groups []*huh.Group
	if kind == "dashboard" || kind == "both" {
		groups = append(groups, huh.NewGroup(
			huh.NewInput().Title("Dashboard username").Value(&form.username),
			huh.NewInput().
				Title("Dashboard password").
				EchoMode(huh.EchoModePassword).
				Value(&form.password),
		))
	}
	if kind == "apikey" || kind == "both" {
		groups = append(groups, huh.NewGroup(
			huh.NewInput().Title("API key").Value(&form.apiKey),
			huh.NewInput().
				Title("API secret").
				EchoMode(huh.EchoModePassword).
				Value(&form.apiSecret),
		))
	}
	groups = append(groups, huh.NewGroup(
		huh.NewConfirm().Title("Read-only?").Value(&form.readonly),
	))
	if err := huh.NewForm(groups...).Run(); err != nil {
		return form, err
	}
	return form, nil
}

func newConnLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all connections",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx conn ls
  muxcat emqx conn ls --json`,
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
				rows = append(rows, []any{n, urlStr, authLabel(conn), conn.Username, conn.Readonly, def})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "url", "auth", "username", "readonly", "default"},
				Rows:    rows,
			}, meta("", start, false))
		},
	}
}

func newConnShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show connection details (password and apiSecret are never echoed)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat emqx conn show local
  muxcat emqx conn show local --json`,
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
				"auth":     authLabel(conn),
				"username": conn.Username,
				"apiKey":   conn.APIKey,
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
		Example: `  muxcat emqx conn rm ro --yes`,
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
		Example: `  muxcat emqx conn default prod`,
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
		Short: "Test a connection (each configured credential pair is verified) and report latency",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat emqx conn test local
  muxcat emqx conn test prod --json`,
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
			dashboard, apikey, err := credPairsOf(conn)
			if err != nil {
				return err
			}
			auth := map[string]any{}
			version, edition := "", ""
			// Each configured pair is verified once: the API key pair with
			// HTTP Basic, the dashboard pair with a login + bearer JWT.
			if apikey {
				v, e, err := cl.probeWithBasic(cmd.Context())
				if err != nil {
					return err
				}
				auth["apiKey"] = "ok"
				if v != "" {
					version = v
				}
				if e != "" {
					edition = e
				}
			} else {
				auth["apiKey"] = "not configured"
			}
			if dashboard {
				v, e, err := cl.probeWithDashboard(cmd.Context())
				if err != nil {
					return err
				}
				auth["dashboard"] = "ok (" + cl.username + ")"
				if v != "" {
					version = v
				}
				if e != "" {
					edition = e
				}
			} else {
				auth["dashboard"] = "not configured"
			}
			latency := time.Since(start).Milliseconds()
			value := map[string]any{
				"ok":         true,
				"auth":       auth,
				"version":    version,
				"edition":    edition,
				"latency_ms": latency,
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   value,
				Message: fmt.Sprintf("connection ok (%d ms, emqx %s %s)", latency, version, edition),
			}, meta(name, start, false))
		},
	}
}

// probeWithBasic verifies the API key pair: GET /api/v5/nodes with HTTP
// Basic; returns the server version and edition from the first node.
func (c *client) probeWithBasic(ctx context.Context) (version, edition string, err error) {
	r, err := c.sendBasic(ctx, http.MethodGet, "/api/v5/nodes", nil, nil)
	if err != nil {
		return "", "", err
	}
	return nodeVersionEdition(r.body)
}

// probeWithDashboard verifies the dashboard pair: a login (bearer JWT)
// plus GET /api/v5/nodes with the token; returns the server version (the
// login response carries it too) and edition from the first node.
func (c *client) probeWithDashboard(ctx context.Context) (version, edition string, err error) {
	if err := c.login(ctx); err != nil {
		return "", "", err
	}
	version = c.version
	r, err := c.sendBearer(ctx, http.MethodGet, "/api/v5/nodes", nil, nil)
	if err != nil {
		return "", "", err
	}
	v, e, err := nodeVersionEdition(r.body)
	if err != nil {
		return "", "", err
	}
	if version == "" {
		version = v
	}
	return version, e, nil
}

// nodeVersionEdition reads version and edition from the first entry of a
// GET /api/v5/nodes response.
func nodeVersionEdition(body []byte) (version, edition string, err error) {
	var nodes []map[string]any
	if err := decodeJSON(body, &nodes); err != nil {
		return "", "", err
	}
	if len(nodes) == 0 {
		return "", "", output.NewError(output.CodeQueryError, "GET /api/v5/nodes returned no nodes", "")
	}
	return strOf(nodes[0], "version"), strOf(nodes[0], "edition"), nil
}

func connNotFound(name string) *output.Error {
	return output.NewError(output.CodeConnNotFound,
		"connection not found: "+name, "list connections with muxcat emqx conn ls")
}
