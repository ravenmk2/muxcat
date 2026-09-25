package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// the bare value and complex replies as indented JSON.
func replyResult(r reply) *output.Result {
	res := &output.Result{JSONData: map[string]any{"type": r.Type, "value": r.Value}}
	switch r.Type {
	case "string", "integer", "double", "boolean", "null":
		res.Value = map[string]any{"value": r.Value}
		res.Bare = true
	default:
		if b, err := json.MarshalIndent(r.Value, "", "  "); err == nil {
			res.Value = string(b)
		}
	}
	return res
}

// resolveTarget loads the config and resolves a connection from
// -c/--conn (falling back to defaultConnection).
func resolveTarget(cmd *cobra.Command) (*Config, string, Connection, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, "", Connection{}, err
	}
	name, conn, err := resolve(cfg, cli.FlagString(cmd, "conn"))
	if err != nil {
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
		Args:  atLeastArgs(1, "<cmd> [args...]", "cmd"),
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
			return cli.RenderResult(cmd, replyResult(r), meta(name, start, truncated))
		},
	}
	addBinaryFlags(c)
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
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"value": value},
				Bare:  true,
			}, meta(name, start, truncated))
		},
	}
	addBinaryFlags(c)
	return c
}

func newSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a key (OK, or null when the --nx/--xx condition is not met)",
		Args:  cli.ExactArgs(2, "<key> <value>", "key", "value"),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			nx, xx := cli.FlagBool(cmd, "nx"), cli.FlagBool(cmd, "xx")
			if nx && xx {
				return output.NewError(output.CodeMissingArgument,
					"--nx and --xx are mutually exclusive", "")
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
			v, err := client.SetArgs(ctx, args[0], args[1], goredis.SetArgs{
				Mode: mode,
				TTL:  ttl,
			}).Result()
			var value any
			switch {
			case errors.Is(err, goredis.Nil):
				value = nil
			case err != nil:
				return classifyErr(err, "set failed")
			default:
				value = v
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"value": value},
				Bare:  true,
			}, meta(name, start, false))
		},
	}
	c.Flags().Duration("ttl", 0, "expiration (e.g. 30s, 5m); 0 keeps the key persistent")
	c.Flags().Bool("nx", false, "set only if the key does not exist")
	c.Flags().Bool("xx", false, "set only if the key already exists")
	return c
}

func newDelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "del <key> [key...]",
		Short: "Delete keys; reports how many were removed",
		Args:  atLeastArgs(1, "<key> [key...]", "key"),
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
			}, meta(name, start, false))
		},
	}
}

func newKeysCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keys [pattern]",
		Short: "List keys matching a pattern (SCAN-based; KEYS is never used)",
		Args:  cobra.MaximumNArgs(1),
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
			if err := guardCommand(conn, "SCAN"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			limit := cli.FlagLimit(cmd)
			rows := make([][]any, 0)
			truncated := false
			var cursor uint64
		scan:
			for {
				var batch []string
				batch, cursor, err = client.Scan(ctx, cursor, pattern, 100).Result()
				if err != nil {
					return classifyErr(err, "scan failed")
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
			}, meta(name, start, truncated))
		},
	}
}

func newTypeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "type <key>",
		Short: "Report the type of a key (none when the key does not exist)",
		Args:  cli.ExactArgs(1, "<key>", "key"),
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
			}, meta(name, start, false))
		},
	}
}

func newTTLCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ttl <key>",
		Short: "Report a key's TTL (-1 no expiry, -2 no such key)",
		Args:  cli.ExactArgs(1, "<key>", "key"),
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
			}, meta(name, start, false))
		},
	}
	c.Flags().Bool("ms", false, "use PTTL (milliseconds) instead of TTL (seconds)")
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
			}, meta(name, start, false))
		},
	}
}

func newDBSizeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dbsize",
		Short: "Report the number of keys in the current database",
		Args:  cobra.NoArgs,
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
			}, meta(name, start, false))
		},
	}
}

func newHGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hget <key> <field>",
		Short: "Get a hash field (null when missing)",
		Args:  cli.ExactArgs(2, "<key> <field>", "key", "field"),
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
			}, meta(name, start, truncated))
		},
	}
	addBinaryFlags(c)
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
			}, meta(name, start, truncated || rowTrunc))
		},
	}
	addBinaryFlags(c)
	return c
}

func newLRangeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lrange <key> <start> <stop>",
		Short: "Get a range of list elements (index column = start + offset)",
		Args:  cli.ExactArgs(3, "<key> <start> <stop>", "key", "start", "stop"),
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
			}, meta(name, start, truncated || rowTrunc))
		},
	}
	addBinaryFlags(c)
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
			}, meta(name, start, truncated || rowTrunc))
		},
	}
	addBinaryFlags(c)
	return c
}

func newZRangeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "zrange <key> <start> <stop>",
		Short: "Get a range of sorted set members with scores",
		Args:  cli.ExactArgs(3, "<key> <start> <stop>", "key", "start", "stop"),
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
			}, meta(name, start, truncated || rowTrunc))
		},
	}
	c.Flags().Bool("rev", false, "reverse order (highest score first)")
	addBinaryFlags(c)
	// Negative ranges (zrange board 0 -1) are the Redis idiom; flags must
	// come before positional arguments, everything after is an argument.
	c.Flags().SetInterspersed(false)
	return c
}

func newEvalCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "eval [script]",
		Short: "Run a Lua script (from an argument or --file; counts as a write)",
		Args:  cobra.MaximumNArgs(1),
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
			return cli.RenderResult(cmd, replyResult(r), meta(name, start, truncated))
		},
	}
	c.Flags().String("file", "", "read the script from a file (alternative to the script argument)")
	c.Flags().StringArray("key", nil, "KEYS[] entry; repeatable, order preserved")
	c.Flags().StringArray("arg", nil, "ARGV[] entry; repeatable, order preserved")
	addBinaryFlags(c)
	return c
}

func newConfigGroupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Read server configuration",
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
				v, tr := renderString(m[f], binary, maxBytes)
				truncated = truncated || tr
				rows = append(rows, []any{f, v})
			}
			var rowTrunc bool
			rows, rowTrunc = applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"field", "value"},
				Rows:    rows,
			}, meta(name, start, truncated || rowTrunc))
		},
	}
	addBinaryFlags(c)
	return c
}
