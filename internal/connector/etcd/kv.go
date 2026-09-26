package etcd

import (
	"context"
	"time"

	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

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

// dial builds the timeout context and opens a verified client for a
// resolved connection.
func dial(cmd *cobra.Command, cfg *Config, conn Connection) (context.Context, context.CancelFunc, *clientv3.Client, error) {
	timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	client, err := openClient(ctx, cfg, conn, timeout)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return ctx, cancel, client, nil
}

func newGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <key>",
		Short: "Get the value of a key, or a range of keys with --prefix",
		Long: `Get the value of a key (value is null when the key does not exist).
With --prefix the key is treated as a prefix and all matching keys are
listed (columns key/value/create_rev/mod_rev/version/lease, sorted by
key; --keys-only omits the value column). --rev reads at a historical
revision; --limit caps prefix results (meta.truncated tells you the
listing stopped early).`,
		Args: cli.ExactArgs(1, "<key>", "key"),
		Example: `  muxcat etcd get mykey
  muxcat etcd get /services/ --prefix --limit 20
  muxcat etcd get /services/ --prefix --keys-only
  muxcat etcd get mykey --rev 42 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			prefix := cli.FlagBool(cmd, "prefix")
			keysOnly := cli.FlagBool(cmd, "keys-only")
			if keysOnly && !prefix {
				return output.NewError(output.CodeMissingArgument,
					"--keys-only requires --prefix", "")
			}
			rev, _ := cmd.Flags().GetInt64("rev")
			if rev < 0 {
				return output.NewError(output.CodeMissingArgument,
					"--rev must be >= 0", "")
			}
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			opts := []clientv3.OpOption{clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend)}
			if rev > 0 {
				opts = append(opts, clientv3.WithRev(rev))
			}
			if !prefix {
				resp, err := client.Get(ctx, args[0], opts...)
				if err != nil {
					return classifyErr(err, "get failed")
				}
				var value any
				if resp.Count > 0 {
					value = string(resp.Kvs[0].Value)
				}
				return cli.RenderResult(cmd, &output.Result{
					Value: map[string]any{"value": value},
					Bare:  true,
				}, meta(cfg, conn, name, start, false))
			}

			limit := cli.FlagLimit(cmd)
			if limit > 0 {
				opts = append(opts, clientv3.WithLimit(int64(limit)))
			}
			opts = append(opts, clientv3.WithPrefix())
			resp, err := client.Get(ctx, args[0], opts...)
			if err != nil {
				return classifyErr(err, "get failed")
			}
			columns := []string{"key", "value", "create_rev", "mod_rev", "version", "lease"}
			if keysOnly {
				columns = []string{"key", "create_rev", "mod_rev", "version", "lease"}
			}
			rows := make([][]any, 0, len(resp.Kvs))
			for _, kv := range resp.Kvs {
				row := []any{string(kv.Key)}
				if !keysOnly {
					row = append(row, string(kv.Value))
				}
				row = append(row, kv.CreateRevision, kv.ModRevision, kv.Version, kv.Lease)
				rows = append(rows, row)
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: columns,
				Rows:    rows,
			}, meta(cfg, conn, name, start, resp.More))
		},
	}
	c.Flags().Bool("prefix", false, "treat <key> as a prefix and list all matching keys")
	c.Flags().Bool("keys-only", false, "omit the value column (requires --prefix)")
	c.Flags().Int64("rev", 0, "read at this revision (0 = latest)")
	return c
}

func newPutCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "put <key> <value>",
		Short: "Put a key-value pair (counts as a write)",
		Long: `Put a key-value pair. --lease-id attaches an existing lease; lease
management itself is out of scope. Counts as a write: readonly
connections reject it.`,
		Args: cli.ExactArgs(2, "<key> <value>", "key", "value"),
		Example: `  muxcat etcd put mykey hello
  muxcat etcd put /services/api healthy --lease-id 694d7b1e2b2a0f0a`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			leaseID, _ := cmd.Flags().GetInt64("lease-id")
			if leaseID < 0 {
				return output.NewError(output.CodeMissingArgument,
					"--lease-id must be >= 0", "")
			}
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "put"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			var opts []clientv3.OpOption
			if leaseID > 0 {
				opts = append(opts, clientv3.WithLease(clientv3.LeaseID(leaseID)))
			}
			if _, err := client.Put(ctx, args[0], args[1], opts...); err != nil {
				return classifyErr(err, "put failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"value": "OK"},
				Bare:  true,
			}, meta(cfg, conn, name, start, false))
		},
	}
	c.Flags().Int64("lease-id", 0, "attach the key to this existing lease ID (0 = no lease)")
	return c
}

func newDelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "del <key>",
		Short: "Delete a key, or a whole prefix with --prefix (dangerous)",
		Long: `Delete a key and report how many keys were removed. With --prefix
every key under the prefix is deleted: that is a dangerous operation
and requires a connection with allowDangerous. Counts as a write:
readonly connections reject it.`,
		Args: cli.ExactArgs(1, "<key>", "key"),
		Example: `  muxcat etcd del mykey
  muxcat etcd del /services/ --prefix -c admin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			prefix := cli.FlagBool(cmd, "prefix")
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "del"); err != nil {
				return err
			}
			if prefix {
				if err := guardDangerous(conn, "del --prefix"); err != nil {
					return err
				}
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			var opts []clientv3.OpOption
			if prefix {
				opts = append(opts, clientv3.WithPrefix())
			}
			resp, err := client.Delete(ctx, args[0], opts...)
			if err != nil {
				return classifyErr(err, "del failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"deleted": resp.Deleted},
				Bare:  true,
			}, meta(cfg, conn, name, start, false))
		},
	}
	c.Flags().Bool("prefix", false, "delete every key under the prefix (requires allowDangerous)")
	return c
}
