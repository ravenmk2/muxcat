package redis

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
		Short: "Manage redis connections",
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
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Connections[name]; exists {
				return output.NewError(output.CodeConfigInvalid,
					"connection already exists: "+name, "remove it first with muxcat redis conn rm "+name)
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

			host := strings.TrimSpace(cli.FlagString(cmd, "host"))
			port, _ := cmd.Flags().GetInt("port")
			db, _ := cmd.Flags().GetInt("db")
			username := cli.FlagString(cmd, "username")
			password := cli.FlagString(cmd, "password")
			tlsOn := cli.FlagBool(cmd, "tls")
			readonly := cli.FlagBool(cmd, "readonly")
			if host == "" {
				if !cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"missing required flag --host (usage: muxcat redis conn add <name> --host <host>)",
						"--host is required in non-interactive environments")
				}
				form, err := promptAddForm()
				if err != nil {
					return err
				}
				host = form.host
				if !cmd.Flags().Changed("port") {
					port = form.port
				}
				if !cmd.Flags().Changed("db") {
					db = form.db
				}
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
			if port < 1 || port > 65535 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid port: %d", port), "valid range: 1-65535")
			}
			if db < 0 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid db: %d", db), "db must be >= 0")
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

			cfg.Instances[name] = Instance{
				Host: host,
				Port: port,
				TLS:  tlsOn,
			}
			conn := Connection{
				Instance:       name,
				Username:       username,
				Password:       encPassword,
				Readonly:       readonly,
				AllowDangerous: cli.FlagBool(cmd, "allow-dangerous"),
				Timeout:        timeout,
			}
			if db != 0 {
				conn.DB = &db
			}
			cfg.Connections[name] = conn
			if cli.FlagBool(cmd, "set-default") || cfg.DefaultConnection == "" {
				cfg.DefaultConnection = name
			}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("added connection %s (%s:%d, db %d)", name, host, port, db),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("host", "", "server host")
	c.Flags().Int("port", 6379, "server port")
	c.Flags().Int("db", 0, "logical database number")
	c.Flags().String("username", "", "ACL username (empty = default user)")
	c.Flags().String("password", "", "password (plaintext flag; stored encrypted)")
	c.Flags().Bool("tls", false, "enable TLS")
	c.Flags().Bool("readonly", false, "allow read commands only")
	c.Flags().Bool("allow-dangerous", false, "allow dangerous commands (FLUSHALL, CONFIG SET, ...)")
	c.Flags().String("timeout", "", "command timeout for this connection, overrides the global --timeout (e.g. 5s)")
	c.Flags().Bool("set-default", false, "set as the default connection")
	return c
}

// addForm holds the answers of the interactive conn add form.
type addForm struct {
	host     string
	port     int
	db       int
	username string
	password string
	tls      bool
	readonly bool
}

// promptAddForm fills in missing arguments with a huh form, only on a TTY.
func promptAddForm() (addForm, error) {
	var form addForm
	portStr, dbStr := "6379", "0"
	f := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Host").
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("host must not be empty")
				}
				return nil
			}).
			Value(&form.host),
		huh.NewInput().
			Title("Port").
			Validate(intRange("port", 1, 65535)).
			Value(&portStr),
		huh.NewInput().
			Title("DB").
			Validate(intRange("db", 0, 1<<30)).
			Value(&dbStr),
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
	form.port, _ = strconv.Atoi(portStr)
	form.db, _ = strconv.Atoi(dbStr)
	return form, nil
}

// intRange validates a numeric huh input.
func intRange(name string, min, max int) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < min || n > max {
			return fmt.Errorf("%s must be an integer in [%d, %d]", name, min, max)
		}
		return nil
	}
}

func newConnLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all connections",
		Args:  cobra.NoArgs,
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
				addrStr, user, db, tlsOn := "?", "", 0, false
				if inst, ok := cfg.Instances[conn.Instance]; ok {
					addrStr = addr(inst)
					db = effectiveDB(inst, conn)
					tlsOn = inst.TLS
					user = effectiveUsername(inst, conn)
				}
				rows = append(rows, []any{n, addrStr, user, db, tlsOn, conn.Readonly, def})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "addr", "user", "db", "tls", "readonly", "default"},
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
				"host":           inst.Host,
				"port":           inst.Port,
				"db":             effectiveDB(inst, conn),
				"tls":            inst.TLS,
				"username":       effectiveUsername(inst, conn),
				"readonly":       conn.Readonly,
				"allowDangerous": conn.AllowDangerous,
				"timeout":        conn.Timeout,
				"default":        name == cfg.DefaultConnection,
			}}, meta(name, start, false))
		},
	}
}

func newConnRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a connection (its instance is removed too when unreferenced)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
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
		Use:   "default <name>",
		Short: "Set the default connection",
		Args:  cli.ExactArgs(1, "<name>", "name"),
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
		Short: "Test a connection (PING + read redis_version) and report latency",
		Args:  cli.ExactArgs(1, "<name>", "name"),
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
			client, err := openClient(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			// INFO may be denied for least-privilege ACL users; degrade to
			// an empty version with a note instead of failing the test.
			version, note := "", ""
			info, err := client.Info(ctx, "server").Result()
			switch {
			case err == nil:
				version, _ = parseInfo(info)["server"].(map[string]any)["redis_version"].(string)
			case isNoPerm(err):
				note = "INFO not permitted for this user"
			default:
				return classifyErr(err, "connection test failed")
			}
			latency := time.Since(start).Milliseconds()
			value := map[string]any{"ok": true, "latency_ms": latency, "version": version}
			message := fmt.Sprintf("connection ok (%d ms, redis %s)", latency, version)
			if note != "" {
				value["note"] = note
				message = fmt.Sprintf("connection ok (%d ms, %s)", latency, note)
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
		"connection not found: "+name, "list connections with muxcat redis conn ls")
}
