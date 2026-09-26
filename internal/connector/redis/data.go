package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// replyResult builds the result for exec/eval replies. The JSON payload is
// always the typed {type, value} shape; text modes render scalar replies as
// the bare value and complex replies as indented JSON. String scalars get
// syntax highlighting per --syntax; complex replies are highlighted as the
// JSON they are rendered in.
func replyResult(r reply, syntaxFlag string) *output.Result {
	res := &output.Result{JSONData: map[string]any{"type": r.Type, "value": r.Value}}
	switch r.Type {
	case "string":
		res.Value = map[string]any{"value": r.Value}
		res.Bare = true
		if s, ok := r.Value.(string); ok {
			res.Syntax, _ = resolveSyntax(s, syntaxFlag)
		}
	case "integer", "double", "boolean", "null":
		res.Value = map[string]any{"value": r.Value}
		res.Bare = true
	default:
		if b, err := json.MarshalIndent(r.Value, "", "  "); err == nil {
			res.Value = string(b)
			res.Syntax = "json"
		}
	}
	return res
}

// addDBFlag registers --db on a keyspace command: it overrides the
// connection's logical database for a single invocation and is never
// persisted. -1 marks "not set" so an explicit --db 0 works.
func addDBFlag(c *cobra.Command) {
	c.Flags().Int("db", -1, "logical database index for this invocation (overrides the connection's db)")
}

// applyDBFlag overrides the connection's db when --db was passed. Commands
// without the flag registered (info, config get) report Changed=false.
func applyDBFlag(cmd *cobra.Command, conn *Connection) error {
	if !cmd.Flags().Changed("db") {
		return nil
	}
	db, _ := cmd.Flags().GetInt("db")
	if db < 0 {
		return output.NewError(output.CodeMissingArgument,
			fmt.Sprintf("invalid --db %d: must be >= 0", db), "")
	}
	conn.DB = &db
	return nil
}

// resolveTarget loads the config and resolves a connection from
// -c/--conn (falling back to defaultConnection), applying the --db override.
func resolveTarget(cmd *cobra.Command) (*Config, string, Connection, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, "", Connection{}, err
	}
	name, conn, err := resolve(cfg, cli.FlagString(cmd, "conn"))
	if err != nil {
		return nil, "", Connection{}, err
	}
	if err := applyDBFlag(cmd, &conn); err != nil {
		return nil, "", Connection{}, err
	}
	return cfg, name, conn, nil
}

// dial builds the timeout context and opens a pinged client for a resolved
// connection.
func dial(cmd *cobra.Command, cfg *Config, conn Connection) (context.Context, context.CancelFunc, *goredis.Client, error) {
	timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	client, err := openClient(ctx, cfg, conn)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return ctx, cancel, client, nil
}

// atLeastArgs reports MISSING_ARGUMENT when fewer than n positional
// arguments are given, mirroring cli.ExactArgs for variable-length forms.
func atLeastArgs(n int, usage, firstName string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return output.NewError(output.CodeMissingArgument,
				fmt.Sprintf("missing required argument <%s> (usage: %s %s)", firstName, cmd.CommandPath(), usage),
				"see "+cmd.CommandPath()+" --help for full usage")
		}
		return nil
	}
}

// parseIntArg parses an integer positional argument.
func parseIntArg(cmd *cobra.Command, name, s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, output.NewError(output.CodeMissingArgument,
			fmt.Sprintf("invalid <%s> %q: not an integer (usage: %s)", name, s, cmd.CommandPath()+" "+strings.TrimPrefix(cmd.Use, name)), "")
	}
	return n, nil
}

func newExecCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "exec <cmd> [args...]",
		Short: "Execute a raw Redis command (pass-through; interception rules apply)",
		Long: `Execute a raw Redis command verbatim. This is the escape hatch for
commands without a structured counterpart (HSET, LPUSH, XADD, ...).

The connector's interception rules apply: a readonly connection allows
read commands only, dangerous commands (FLUSHALL, CONFIG SET, ...)
require a connection with allowDangerous, and SELECT is always blocked
— pass --db n instead (each invocation uses its own connection).

Flags must come before positional arguments; everything after the
command name is passed through (negative arguments like 0 -1 work).`,
		Args: atLeastArgs(1, "<cmd> [args...]", "cmd"),
		Example: `  muxcat redis exec PING
  muxcat redis exec HGETALL user:42
  muxcat redis exec SETEX session:42 3600 payload
  muxcat redis exec ZRANGE board 0 -1 WITHSCORES`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			cmdArgs := make([]any, len(args))
			for i, a := range args {
				cmdArgs[i] = a
			}
			if err := guardCommand(conn, args[0], cmdArgs[1:]...); err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			if err := checkHighlightFlag(cmd); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			v, err := client.Do(ctx, cmdArgs...).Result()
			if errors.Is(err, goredis.Nil) {
				v, err = nil, nil
			}
			if err != nil {
				return classifyErr(err, "command failed")
			}
			r, truncated := renderReply(v, binary, maxBytes)
			return cli.RenderResult(cmd, replyResult(r, highlightFlag(cmd)), meta(cfg, conn, name, start, truncated))
		},
	}
	addBinaryFlags(c)
	addHighlightFlag(c)
	addDBFlag(c)
	// Negative args (e.g. exec ZRANGE board 0 -1) are common; flags must
	// come before positional arguments, everything after is an argument.
	c.Flags().SetInterspersed(false)
	return c
}

func newGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <key>",
		Short: "Get the value of a key (null when the key does not exist)",
		Args:  cli.ExactArgs(1, "<key>", "key"),
		Example: `  muxcat redis get mykey
  muxcat redis get session:42 -c cache
  muxcat redis get blob --binary base64 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "GET"); err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			if err := checkHighlightFlag(cmd); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			var value any
			truncated := false
			v, err := client.Get(ctx, args[0]).Result()
			switch {
			case errors.Is(err, goredis.Nil):
				value = nil
			case err != nil:
				return classifyErr(err, "get failed")
			default:
				value, truncated = renderString(v, binary, maxBytes)
			}
			res := &output.Result{
				Value: map[string]any{"value": value},
				Bare:  true,
			}
			if s, ok := value.(string); ok {
				res.Syntax, _ = resolveSyntax(s, highlightFlag(cmd))
			}
			return cli.RenderResult(cmd, res, meta(cfg, conn, name, start, truncated))
		},
	}
	addBinaryFlags(c)
	addHighlightFlag(c)
	addDBFlag(c)
	return c
}

func newSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set <key> [value]",
		Short: "Set a key (OK, or null when the --nx/--xx condition is not met)",
		Long: `Set a key. The value comes from the positional argument, or from
--file (binary-safe; "-" reads stdin). Counts as a write: readonly
connections reject it.`,
		Args: cobra.RangeArgs(1, 2),
		Example: `  muxcat redis set mykey hello
  muxcat redis set session:42 data --ttl 30m --nx
  muxcat redis set blob --file ./payload.bin
  gzip -c big.json | muxcat redis set big:gz --file -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			nx, xx := cli.FlagBool(cmd, "nx"), cli.FlagBool(cmd, "xx")
			if nx && xx {
				return output.NewError(output.CodeMissingArgument,
					"--nx and --xx are mutually exclusive", "")
			}
			file := cli.FlagString(cmd, "file")
			var value string
			switch {
			case len(args) == 2 && file != "":
				return output.NewError(output.CodeMissingArgument,
					"value argument and --file are mutually exclusive", "")
			case len(args) == 2:
				value = args[1]
			case file == "-":
				if cli.RuntimeFrom(cmd.Context()).Interactive {
					return output.NewError(output.CodeMissingArgument,
						"--file - reads the value from stdin, but stdin is a terminal",
						"pipe the value in, or pass a file path")
				}
				raw, err := io.ReadAll(os.Stdin)
				if err != nil {
					return output.NewError(output.CodeMissingArgument,
						"cannot read value from stdin: "+err.Error(), "")
				}
				value = string(raw)
			case file != "":
				raw, err := os.ReadFile(file)
				if err != nil {
					return output.NewError(output.CodeMissingArgument,
						"cannot read value file "+file+": "+err.Error(), "")
				}
				value = string(raw)
			default:
				return output.NewError(output.CodeMissingArgument,
					"missing value (usage: "+cmd.CommandPath()+" <key> <value> or --file <path>)", "")
			}
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "SET"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			ttl, _ := cmd.Flags().GetDuration("ttl")
			mode := ""
			if nx {
				mode = "NX"
			} else if xx {
				mode = "XX"
			}
			v, err := client.SetArgs(ctx, args[0], value, goredis.SetArgs{
				Mode: mode,
				TTL:  ttl,
			}).Result()
			var out any
			switch {
			case errors.Is(err, goredis.Nil):
				out = nil
			case err != nil:
				return classifyErr(err, "set failed")
			default:
				out = v
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"value": out},
				Bare:  true,
			}, meta(cfg, conn, name, start, false))
		},
	}
	c.Flags().Duration("ttl", 0, "expiration (e.g. 30s, 5m); 0 keeps the key persistent")
	c.Flags().Bool("nx", false, "set only if the key does not exist")
	c.Flags().Bool("xx", false, "set only if the key already exists")
	c.Flags().String("file", "", "read the value from a file (binary-safe; - reads stdin)")
	addDBFlag(c)
	return c
}

func newDelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "del <key> [key...]",
		Short: "Delete keys; reports how many were removed",
		Args:  atLeastArgs(1, "<key> [key...]", "key"),
		Example: `  muxcat redis del mykey
  muxcat redis del k1 k2 k3 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "DEL"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			n, err := client.Del(ctx, args...).Result()
			if err != nil {
				return classifyErr(err, "del failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"deleted": n},
				Bare:  true,
			}, meta(cfg, conn, name, start, false))
		},
	}
	addDBFlag(c)
	return c
}

// scanTypes are the valid values of SCAN's TYPE filter (Redis 6+).
var scanTypes = map[string]bool{
	"string": true, "list": true, "set": true,
	"zset": true, "hash": true, "stream": true,
}

// classifyScanErr classifies a SCAN failure; with --type set, a server
// syntax error points at the Redis 6.0 requirement of the TYPE filter.
func classifyScanErr(err error, keyType string) *output.Error {
	e := classifyErr(err, "scan failed")
	if keyType != "" && strings.Contains(err.Error(), "syntax error") {
		e.Hint = "SCAN TYPE requires Redis 6.0+; the server is older"
	}
	return e
}

func newScanCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "scan [pattern]",
		Short: "Scan keys matching a pattern (SCAN-based; KEYS is never used)",
		Long: `Scan keys with Redis SCAN (never the blocking KEYS). [pattern] is the
MATCH pattern (default *), --type applies the server-side TYPE filter
(Redis 6+), and --count tunes the per-batch COUNT (pacing only, not
the result set).

Default mode iterates to completion, capped by --limit (meta.truncated
tells you the scan stopped early). With --cursor n it runs exactly one
round from that cursor and returns the next cursor in the envelope's
meta.cursor (0 means the iteration is complete); cursors are valid
across invocations but carry no snapshot semantics.`,
		Args: cobra.MaximumNArgs(1),
		Example: `  muxcat redis scan 'user:*' --limit 20
  muxcat redis scan --type hash --count 500
  muxcat redis scan --cursor 0 --json        # one round; next cursor in meta.cursor
  muxcat redis scan --db 2 'session:*'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			pattern := "*"
			if len(args) == 1 {
				pattern = args[0]
			}
			keyType := strings.ToLower(cli.FlagString(cmd, "type"))
			if keyType != "" && !scanTypes[keyType] {
				return output.NewError(output.CodeMissingArgument,
					fmt.Sprintf("invalid --type %q (valid: string, list, set, zset, hash, stream)", keyType), "")
			}
			count, _ := cmd.Flags().GetInt64("count")
			if count <= 0 {
				return output.NewError(output.CodeMissingArgument,
					"--count must be > 0", "")
			}
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "SCAN"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			oneRound := func(cursor uint64) ([]string, uint64, error) {
				if keyType != "" {
					return client.ScanType(ctx, cursor, pattern, count, keyType).Result()
				}
				return client.Scan(ctx, cursor, pattern, count).Result()
			}

			// Single-round mode: one SCAN call from --cursor; the next
			// cursor rides in meta (0 marks a completed iteration).
			if cmd.Flags().Changed("cursor") {
				cursor, _ := cmd.Flags().GetUint64("cursor")
				batch, next, err := oneRound(cursor)
				if err != nil {
					return classifyScanErr(err, keyType)
				}
				rows := make([][]any, 0, len(batch))
				for _, k := range batch {
					rows = append(rows, []any{k})
				}
				var trunc bool
				rows, trunc = applyLimit(rows, cli.FlagLimit(cmd))
				m := meta(cfg, conn, name, start, trunc)
				m.Cursor = &next
				return cli.RenderResult(cmd, &output.Result{
					Columns: []string{"key"},
					Rows:    rows,
				}, m)
			}

			limit := cli.FlagLimit(cmd)
			rows := make([][]any, 0)
			truncated := false
			var cursor uint64
		scan:
			for {
				var batch []string
				batch, cursor, err = oneRound(cursor)
				if err != nil {
					return classifyScanErr(err, keyType)
				}
				for _, k := range batch {
					if limit > 0 && len(rows) >= limit {
						truncated = true
						break scan
					}
					rows = append(rows, []any{k})
				}
				if cursor == 0 {
					break
				}
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"key"},
				Rows:    rows,
			}, meta(cfg, conn, name, start, truncated))
		},
	}
	c.Flags().String("type", "", "server-side TYPE filter: string, list, set, zset, hash, stream")
	c.Flags().Int64("count", 100, "SCAN COUNT per batch (affects pacing, not the result set)")
	c.Flags().Uint64("cursor", 0, "single-round mode: run one SCAN from this cursor; the next cursor is returned in meta")
	addDBFlag(c)
	return c
}

func newTypeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "type <key>",
		Short:   "Report the type of a key (none when the key does not exist)",
		Args:    cli.ExactArgs(1, "<key>", "key"),
		Example: `  muxcat redis type mykey`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "TYPE"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			t, err := client.Type(ctx, args[0]).Result()
			if err != nil {
				return classifyErr(err, "type failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"value": t},
				Bare:  true,
			}, meta(cfg, conn, name, start, false))
		},
	}
	addDBFlag(c)
	return c
}

func newTTLCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ttl <key>",
		Short: "Report a key's TTL (-1 no expiry, -2 no such key)",
		Args:  cli.ExactArgs(1, "<key>", "key"),
		Example: `  muxcat redis ttl mykey
  muxcat redis ttl mykey --ms`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			ms := cli.FlagBool(cmd, "ms")
			if ms {
				err = guardCommand(conn, "PTTL")
			} else {
				err = guardCommand(conn, "TTL")
			}
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			var d time.Duration
			if ms {
				d, err = client.PTTL(ctx, args[0]).Result()
			} else {
				d, err = client.TTL(ctx, args[0]).Result()
			}
			if err != nil {
				return classifyErr(err, "ttl failed")
			}
			// go-redis passes the -1/-2 sentinels through unscaled.
			var value int64
			switch {
			case d < 0:
				value = int64(d)
			case ms:
				value = d.Milliseconds()
			default:
				value = int64(d / time.Second)
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"value": value},
				Bare:  true,
			}, meta(cfg, conn, name, start, false))
		},
	}
	c.Flags().Bool("ms", false, "use PTTL (milliseconds) instead of TTL (seconds)")
	addDBFlag(c)
	return c
}

// parseInfo parses INFO output into {section: {field: value}}; section
// names are lowercased.
func parseInfo(text string) map[string]any {
	sections := map[string]any{}
	section := "default"
	fields := map[string]any{}
	flush := func() {
		if len(fields) > 0 {
			sections[section] = fields
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			flush()
			section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "#")))
			fields = map[string]any{}
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[k] = v
		}
	}
	flush()
	return sections
}

func newInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info [section]",
		Short: "Read server INFO (default sections, or one section)",
		Args:  cobra.MaximumNArgs(1),
		Example: `  muxcat redis info
  muxcat redis info memory --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "INFO"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			var text string
			if len(args) == 1 {
				text, err = client.Info(ctx, args[0]).Result()
			} else {
				text, err = client.Info(ctx).Result()
			}
			if err != nil {
				return classifyErr(err, "info failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: parseInfo(text),
			}, meta(cfg, conn, name, start, false))
		},
	}
}

func newDBSizeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "dbsize",
		Short: "Report the number of keys in the current database",
		Args:  cobra.NoArgs,
		Example: `  muxcat redis dbsize
  muxcat redis dbsize --db 3`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "DBSIZE"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			n, err := client.DBSize(ctx).Result()
			if err != nil {
				return classifyErr(err, "dbsize failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"value": n},
				Bare:  true,
			}, meta(cfg, conn, name, start, false))
		},
	}
	addDBFlag(c)
	return c
}

func newHGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hget <key> <field>",
		Short: "Get a hash field (null when missing)",
		Args:  cli.ExactArgs(2, "<key> <field>", "key", "field"),
		Example: `  muxcat redis hget user:42 name
  muxcat redis hget user:42 avatar --binary base64`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "HGET"); err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			var value any
			truncated := false
			v, err := client.HGet(ctx, args[0], args[1]).Result()
			switch {
			case errors.Is(err, goredis.Nil):
				value = nil
			case err != nil:
				return classifyErr(err, "hget failed")
			default:
				value, truncated = renderString(v, binary, maxBytes)
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"value": value},
				Bare:  true,
			}, meta(cfg, conn, name, start, truncated))
		},
	}
	addBinaryFlags(c)
	addDBFlag(c)
	return c
}

// applyLimit caps rows at limit (when > 0) and reports truncation.
func applyLimit(rows [][]any, limit int) ([][]any, bool) {
	if limit > 0 && len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

func newHGetAllCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hgetall <key>",
		Short: "Get all fields and values of a hash (sorted by field)",
		Args:  cli.ExactArgs(1, "<key>", "key"),
		Example: `  muxcat redis hgetall user:42
  muxcat redis hgetall user:42 --limit 50 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "HGETALL"); err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			m, err := client.HGetAll(ctx, args[0]).Result()
			if err != nil {
				return classifyErr(err, "hgetall failed")
			}
			fields := make([]string, 0, len(m))
			for f := range m {
				fields = append(fields, f)
			}
			sort.Strings(fields)
			rows := make([][]any, 0, len(fields))
			truncated := false
			for _, f := range fields {
				v, tr := renderString(m[f], binary, maxBytes)
				truncated = truncated || tr
				rows = append(rows, []any{f, v})
			}
			var rowTrunc bool
			rows, rowTrunc = applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"field", "value"},
				Rows:    rows,
			}, meta(cfg, conn, name, start, truncated || rowTrunc))
		},
	}
	addBinaryFlags(c)
	addDBFlag(c)
	return c
}

func newLRangeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lrange <key> <start> <stop>",
		Short: "Get a range of list elements (index column = start + offset)",
		Args:  cli.ExactArgs(3, "<key> <start> <stop>", "key", "start", "stop"),
		Example: `  muxcat redis lrange queue 0 -1
  muxcat redis lrange queue 0 9 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			startIdx, err := parseIntArg(cmd, "start", args[1])
			if err != nil {
				return err
			}
			stopIdx, err := parseIntArg(cmd, "stop", args[2])
			if err != nil {
				return err
			}
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "LRANGE"); err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			vals, err := client.LRange(ctx, args[0], startIdx, stopIdx).Result()
			if err != nil {
				return classifyErr(err, "lrange failed")
			}
			rows := make([][]any, 0, len(vals))
			truncated := false
			for i, v := range vals {
				rv, tr := renderString(v, binary, maxBytes)
				truncated = truncated || tr
				rows = append(rows, []any{startIdx + int64(i), rv})
			}
			var rowTrunc bool
			rows, rowTrunc = applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"index", "value"},
				Rows:    rows,
			}, meta(cfg, conn, name, start, truncated || rowTrunc))
		},
	}
	addBinaryFlags(c)
	addDBFlag(c)
	// Negative ranges (lrange queue 0 -1) are the Redis idiom; flags must
	// come before positional arguments, everything after is an argument.
	c.Flags().SetInterspersed(false)
	return c
}

func newSMembersCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "smembers <key>",
		Short: "Get all members of a set (sorted for stable output)",
		Args:  cli.ExactArgs(1, "<key>", "key"),
		Example: `  muxcat redis smembers tags
  muxcat redis smembers tags --limit 100 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "SMEMBERS"); err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			members, err := client.SMembers(ctx, args[0]).Result()
			if err != nil {
				return classifyErr(err, "smembers failed")
			}
			sort.Strings(members)
			rows := make([][]any, 0, len(members))
			truncated := false
			for _, m := range members {
				rv, tr := renderString(m, binary, maxBytes)
				truncated = truncated || tr
				rows = append(rows, []any{rv})
			}
			var rowTrunc bool
			rows, rowTrunc = applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"member"},
				Rows:    rows,
			}, meta(cfg, conn, name, start, truncated || rowTrunc))
		},
	}
	addBinaryFlags(c)
	addDBFlag(c)
	return c
}

func newZRangeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "zrange <key> <start> <stop>",
		Short: "Get a range of sorted set members with scores",
		Args:  cli.ExactArgs(3, "<key> <start> <stop>", "key", "start", "stop"),
		Example: `  muxcat redis zrange board 0 9
  muxcat redis zrange board 0 -1 --rev --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			startIdx, err := parseIntArg(cmd, "start", args[1])
			if err != nil {
				return err
			}
			stopIdx, err := parseIntArg(cmd, "stop", args[2])
			if err != nil {
				return err
			}
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			rev := cli.FlagBool(cmd, "rev")
			if rev {
				err = guardCommand(conn, "ZREVRANGE")
			} else {
				err = guardCommand(conn, "ZRANGE")
			}
			if err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			var zs []goredis.Z
			if rev {
				zs, err = client.ZRevRangeWithScores(ctx, args[0], startIdx, stopIdx).Result()
			} else {
				zs, err = client.ZRangeWithScores(ctx, args[0], startIdx, stopIdx).Result()
			}
			if err != nil {
				return classifyErr(err, "zrange failed")
			}
			rows := make([][]any, 0, len(zs))
			truncated := false
			for _, z := range zs {
				rv, tr := renderString(fmt.Sprint(z.Member), binary, maxBytes)
				truncated = truncated || tr
				rows = append(rows, []any{rv, z.Score})
			}
			var rowTrunc bool
			rows, rowTrunc = applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"member", "score"},
				Rows:    rows,
			}, meta(cfg, conn, name, start, truncated || rowTrunc))
		},
	}
	c.Flags().Bool("rev", false, "reverse order (highest score first)")
	addBinaryFlags(c)
	addDBFlag(c)
	// Negative ranges (zrange board 0 -1) are the Redis idiom; flags must
	// come before positional arguments, everything after is an argument.
	c.Flags().SetInterspersed(false)
	return c
}

func newEvalCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "eval [script]",
		Short: "Run a Lua script (from an argument or --file; counts as a write)",
		Long: `Run a Lua script (EVAL). The script comes from the positional
argument or --file; --key entries map to KEYS[] and --arg entries to
ARGV[], both repeatable and order-preserving. Counts as a write:
readonly connections reject it.`,
		Args: cobra.MaximumNArgs(1),
		Example: `  muxcat redis eval "return redis.call('GET', KEYS[1])" --key mykey
  muxcat redis eval --file ./rotate.lua --key k1 --key k2 --arg 10`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			file := cli.FlagString(cmd, "file")
			var script string
			switch {
			case len(args) == 1 && file != "":
				return output.NewError(output.CodeMissingArgument,
					"script argument and --file are mutually exclusive", "")
			case len(args) == 1:
				script = args[0]
			case file != "":
				raw, err := os.ReadFile(file)
				if err != nil {
					return output.NewError(output.CodeMissingArgument,
						"cannot read script file "+file+": "+err.Error(), "")
				}
				script = string(raw)
			default:
				return output.NewError(output.CodeMissingArgument,
					"missing script (usage: "+cmd.CommandPath()+" <script> or --file <path>)", "")
			}

			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "EVAL"); err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			if err := checkHighlightFlag(cmd); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			keys, _ := cmd.Flags().GetStringArray("key")
			argv, _ := cmd.Flags().GetStringArray("arg")
			argVals := make([]any, len(argv))
			for i, a := range argv {
				argVals[i] = a
			}
			v, err := client.Eval(ctx, script, keys, argVals...).Result()
			if errors.Is(err, goredis.Nil) {
				v, err = nil, nil
			}
			if err != nil {
				return classifyErr(err, "eval failed")
			}
			r, truncated := renderReply(v, binary, maxBytes)
			return cli.RenderResult(cmd, replyResult(r, highlightFlag(cmd)), meta(cfg, conn, name, start, truncated))
		},
	}
	c.Flags().String("file", "", "read the script from a file (alternative to the script argument)")
	c.Flags().StringArray("key", nil, "KEYS[] entry; repeatable, order preserved")
	c.Flags().StringArray("arg", nil, "ARGV[] entry; repeatable, order preserved")
	addBinaryFlags(c)
	addHighlightFlag(c)
	addDBFlag(c)
	return c
}

func newConfigGroupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Read server configuration",
		Long: `Read server configuration. Only CONFIG GET is exposed (and only it
passes the interception rules); CONFIG SET and friends are blocked
unless the connection sets allowDangerous, and are then reachable via
muxcat redis exec.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newConfigGetCmd())
	return c
}

func newConfigGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get [pattern]",
		Short: "Read server configuration parameters (CONFIG GET; pattern defaults to *)",
		Args:  cobra.MaximumNArgs(1),
		Example: `  muxcat redis config get 'maxmemory*'
  muxcat redis config get --json   # credentials (requirepass, ...) are masked as ***`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			pattern := "*"
			if len(args) == 1 {
				pattern = args[0]
			}
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardCommand(conn, "CONFIG", "GET"); err != nil {
				return err
			}
			binary, maxBytes, err := renderOpts(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			m, err := client.ConfigGet(ctx, pattern).Result()
			if err != nil {
				return classifyErr(err, "config get failed")
			}
			fields := make([]string, 0, len(m))
			for f := range m {
				fields = append(fields, f)
			}
			sort.Strings(fields)
			rows := make([][]any, 0, len(fields))
			truncated := false
			for _, f := range fields {
				v, tr := renderString(maskConfigValue(f, m[f]), binary, maxBytes)
				truncated = truncated || tr
				rows = append(rows, []any{f, v})
			}
			var rowTrunc bool
			rows, rowTrunc = applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"field", "value"},
				Rows:    rows,
			}, meta(cfg, conn, name, start, truncated || rowTrunc))
		},
	}
	addBinaryFlags(c)
	return c
}

// sensitiveConfigFields are CONFIG GET parameters whose values are
// credentials; they are masked in output.
var sensitiveConfigFields = map[string]bool{
	"requirepass": true,
	"masterauth":  true,
}

// maskConfigValue masks credential-bearing config values. An empty value
// stays empty so it remains possible to tell whether a credential is set.
func maskConfigValue(field, value string) string {
	if sensitiveConfigFields[field] && value != "" {
		return "***"
	}
	return value
}
