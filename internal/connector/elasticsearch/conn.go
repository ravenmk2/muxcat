package elasticsearch

import (
	"encoding/json"
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
		Short: "Manage elasticsearch connections",
		Long: `Manage elasticsearch connections. conn add creates a same-named
instance (endpoint url) and connection (credentials, policies)
in one step; one instance can back multiple connections (different
users or API keys). Credentials are stored encrypted and never
echoed by ls/show. The default connection is used when -c/--conn
is not passed.`,
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
		Example: `  muxcat elasticsearch conn add local --url http://127.0.0.1:9200 --set-default
  muxcat elasticsearch conn add prod --url https://es.example.com:9200 --username elastic --password s3cret --insecure-skip-verify
  muxcat elasticsearch conn add ro --url https://es.example.com:9200 --apikey <base64-id:key> --readonly`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Connections[name]; exists {
				return output.NewError(output.CodeConfigInvalid,
					"connection already exists: "+name, "remove it first with muxcat elasticsearch conn rm "+name)
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
			apiKey := cli.FlagString(cmd, "apikey")
			readonly := cli.FlagBool(cmd, "readonly")
			skipVerify := cli.FlagBool(cmd, "insecure-skip-verify")
			if rawURL == "" {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"missing required flag --url (usage: muxcat elasticsearch conn add <name> --url <url>)",
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
				if !cmd.Flags().Changed("apikey") {
					apiKey = form.apiKey
				}
				if !cmd.Flags().Changed("readonly") {
					readonly = form.readonly
				}
			}
			base, err := normalizeURL(rawURL)
			if err != nil {
				return err
			}
			// Basic auth and API key auth are mutually exclusive.
			if apiKey != "" && (username != "" || password != "") {
				return output.NewError(output.CodeConfigInvalid,
					"--apikey and --username/--password are mutually exclusive",
					"an API key replaces basic auth; keep only one of them")
			}
			timeout := cli.FlagString(cmd, "timeout")
			if timeout != "" {
				if d, err := time.ParseDuration(timeout); err != nil || d <= 0 {
					return output.NewError(output.CodeConfigInvalid,
						"invalid --timeout value: "+timeout, "examples: 5s, 1m (must be > 0)")
				}
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
			encAPIKey := ""
			if apiKey != "" {
				if cmd.Flags().Changed("apikey") {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
						"Warning: --apikey passes the credential in plaintext; prefer an interactive prompt or edit "+FileName)
				}
				key, _, err := masterKey()
				if err != nil {
					return err
				}
				enc, err := secret.Encrypt(key, []byte(apiKey))
				if err != nil {
					return err
				}
				encAPIKey = enc
			}

			cfg.Instances[name] = Instance{URL: base}
			cfg.Connections[name] = Connection{
				Instance:           name,
				Username:           username,
				Password:           encPassword,
				APIKey:             encAPIKey,
				Readonly:           readonly,
				Timeout:            timeout,
				InsecureSkipVerify: skipVerify,
			}
			if cli.FlagBool(cmd, "set-default") || cfg.DefaultConnection == "" {
				cfg.DefaultConnection = name
			}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("added connection %s (%s, auth %s)", name, base, cfg.Connections[name].authKind()),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("url", "", "base URL of the Elasticsearch server (e.g. http://127.0.0.1:9200)")
	c.Flags().String("username", "", "basic auth username")
	c.Flags().String("password", "", "password (plaintext flag; stored encrypted)")
	c.Flags().String("apikey", "", "API key, the base64(id:api_key) string (plaintext flag; stored encrypted; mutually exclusive with --username/--password)")
	c.Flags().Bool("insecure-skip-verify", false, "skip TLS certificate verification (insecure; for self-signed servers)")
	c.Flags().Bool("readonly", false, "allow read requests only")
	c.Flags().String("timeout", "", "command timeout for this connection, overrides the global --timeout (e.g. 5s)")
	c.Flags().Bool("set-default", false, "set as the default connection")
	return c
}

// addForm holds the answers of the interactive conn add form.
type addForm struct {
	url      string
	username string
	password string
	apiKey   string
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
			Title("Password (optional)").
			EchoMode(huh.EchoModePassword).
			Value(&form.password),
		huh.NewInput().
			Title("API key (optional; mutually exclusive with username/password)").
			EchoMode(huh.EchoModePassword).
			Value(&form.apiKey),
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
		Example: `  muxcat elasticsearch conn ls
  muxcat elasticsearch conn ls --json`,
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
				tlsOn := false
				if inst, ok := cfg.Instances[conn.Instance]; ok {
					urlStr = inst.URL
					tlsOn = strings.HasPrefix(inst.URL, "https://")
				}
				rows = append(rows, []any{n, urlStr, conn.Username, conn.authKind(), tlsOn, conn.InsecureSkipVerify, conn.Readonly, def})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "url", "user", "auth", "tls", "insecure", "readonly", "default"},
				Rows:    rows,
			}, meta("", start, false))
		},
	}
}

func newConnShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show connection details (credentials are never echoed)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat elasticsearch conn show local
  muxcat elasticsearch conn show local --json`,
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
				"name":               name,
				"instance":           conn.Instance,
				"url":                inst.URL,
				"username":           conn.Username,
				"auth":               conn.authKind(),
				"readonly":           conn.Readonly,
				"timeout":            conn.Timeout,
				"insecureSkipVerify": conn.InsecureSkipVerify,
				"default":            name == cfg.DefaultConnection,
			}}, meta(name, start, false))
		},
	}
}

func newConnRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a connection (its instance is removed too when unreferenced)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat elasticsearch conn rm ro --yes`,
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
		Example: `  muxcat elasticsearch conn default prod`,
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
		Use:   "test [name]",
		Short: "Test a connection (auth check + read cluster info) and report latency",
		Args:  cobra.MaximumNArgs(1),
		Example: `  muxcat elasticsearch conn test
  muxcat es conn test prod --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			sel := ""
			if len(args) > 0 {
				sel = args[0]
			}
			name, conn, err := resolve(cfg, sel)
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
			// GET / is the root info endpoint: auth check + version probe.
			r, err := cl.do(cmd.Context(), "GET", "/", nil)
			if err != nil {
				return err
			}
			var root struct {
				ClusterName string `json:"cluster_name"`
				Version     struct {
					Number string `json:"number"`
				} `json:"version"`
			}
			_ = json.Unmarshal(r.body, &root)
			latency := time.Since(start).Milliseconds()
			value := map[string]any{
				"ok":           true,
				"latency_ms":   latency,
				"version":      root.Version.Number,
				"cluster_name": root.ClusterName,
			}
			message := fmt.Sprintf("connection ok (%d ms, elasticsearch %s, cluster %s)", latency, root.Version.Number, root.ClusterName)
			// X-Elastic-Product is present on Elasticsearch 7.14+; its absence
			// means a fork (e.g. OpenSearch) or a very old server — degrade to
			// a warning instead of failing an otherwise healthy probe.
			if !strings.EqualFold(r.headers["X-Elastic-Product"], "Elasticsearch") {
				warning := "X-Elastic-Product header missing: not an Elasticsearch server (7.14+) — possibly a fork or an old version"
				value["warning"] = warning
				message = fmt.Sprintf("connection ok (%d ms, version %s, cluster %s; %s)", latency, root.Version.Number, root.ClusterName, warning)
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
		"connection not found: "+name, "list connections with muxcat elasticsearch conn ls")
}
