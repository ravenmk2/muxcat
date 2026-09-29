package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newIndexesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "indexes",
		Short: "Manage indexes",
		Long: `Manage indexes of a collection: list them (indexes ls), create one
from an Extended JSON keys document (indexes create), or drop one by
name (indexes drop, confirmation required). create/drop are write
operations and are refused on readonly connections.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newIndexesLsCmd(),
		newIndexesCreateCmd(),
		newIndexesDropCmd(),
	)
	return c
}

func newIndexesLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls <collection>",
		Short: "List indexes of a collection",
		Args:  cli.ExactArgs(1, "<collection>", "collection"),
		Example: `  muxcat mongodb indexes ls users
  muxcat mongodb indexes ls users --db shop --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			client, err := connect(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = client.Disconnect(context.Background()) }()
			coll, err := collectionOf(client, cmd, conn, args[0])
			if err != nil {
				return err
			}

			cur, err := coll.Indexes().List(ctx)
			if err != nil {
				return classifyErr(err, "list indexes failed")
			}
			defer func() { _ = cur.Close(ctx) }()
			docs := make([]bson.D, 0)
			for cur.Next(ctx) {
				var d bson.D
				if err := cur.Decode(&d); err != nil {
					return classifyErr(err, "failed to read results")
				}
				docs = append(docs, d)
			}
			if err := cur.Err(); err != nil {
				return classifyErr(err, "failed to read results")
			}
			rows := make([][]any, 0, len(docs))
			for _, d := range docs {
				rows = append(rows, indexRow(d))
			}
			jsonData, err := relaxedJSON(docs)
			if err != nil {
				return output.NewError(output.CodeGeneral, "failed to encode the index list: "+err.Error(), "")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "keys", "unique", "sparse", "ttl"},
				Rows:     rows,
				Message:  fmt.Sprintf("%d indexes", len(docs)),
				JSONData: jsonData,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	return c
}

// indexRow renders an index spec document into a table row.
func indexRow(d bson.D) []any {
	keys, err := compactExtJSON(docLookup(d, "key"), false)
	if err != nil {
		keys = fmt.Sprint(docLookup(d, "key"))
	}
	ttl := ""
	if v := docLookup(d, "expireAfterSeconds"); v != nil {
		ttl = fmt.Sprint(v)
	}
	return []any{docLookup(d, "name"), keys, docLookup(d, "unique") == true, docLookup(d, "sparse") == true, ttl}
}

// parseIndexKeys parses the keys argument: a non-empty Extended JSON
// document such as {"email":1} or {"loc":"2dsphere"}.
func parseIndexKeys(s string) (bson.D, error) {
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(s), false, &d); err != nil {
		return nil, output.NewError(output.CodeConfigInvalid,
			"invalid keys JSON: "+err.Error(), `examples: {"email":1}, {"loc":"2dsphere"}`)
	}
	if len(d) == 0 {
		return nil, output.NewError(output.CodeConfigInvalid,
			"the keys document must not be empty", `examples: {"email":1}, {"loc":"2dsphere"}`)
	}
	return d, nil
}

func newIndexesCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create <collection> <keys>",
		Short: "Create an index from an Extended JSON keys document",
		Args:  cli.ExactArgs(2, "<collection> <keys>", "collection", "keys"),
		Example: `  muxcat mongodb indexes create users '{"email":1}' --unique
  muxcat mongodb indexes create sessions '{"createdAt":1}' --ttl 3600
  muxcat mongodb indexes create places '{"loc":"2dsphere"}' --name geo`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn); err != nil {
				return err
			}
			keys, err := parseIndexKeys(args[1])
			if err != nil {
				return err
			}
			ttl, _ := cmd.Flags().GetInt("ttl")
			if ttl < 0 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --ttl value: %d", ttl), "must be >= 0 (seconds)")
			}
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			client, err := connect(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = client.Disconnect(context.Background()) }()
			coll, err := collectionOf(client, cmd, conn, args[0])
			if err != nil {
				return err
			}

			opts := options.Index()
			if n := cli.FlagString(cmd, "name"); n != "" {
				opts.SetName(n)
			}
			if cli.FlagBool(cmd, "unique") {
				opts.SetUnique(true)
			}
			if cli.FlagBool(cmd, "sparse") {
				opts.SetSparse(true)
			}
			if cmd.Flags().Changed("ttl") {
				opts.SetExpireAfterSeconds(int32(ttl))
			}
			indexName, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys, Options: opts})
			if err != nil {
				return classifyErr(err, "create index failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"name": indexName},
				Message: fmt.Sprintf("created index %s on %s", indexName, args[0]),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().String("name", "", "index name (default: server-generated from the keys)")
	c.Flags().Bool("unique", false, "create a unique index")
	c.Flags().Bool("sparse", false, "create a sparse index")
	c.Flags().Int("ttl", 0, "expire documents after N seconds (TTL index)")
	return c
}

func newIndexesDropCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "drop <collection> <name>",
		Short: "Drop an index by name (requires confirmation)",
		Args:  cli.ExactArgs(2, "<collection> <name>", "collection", "name"),
		Example: `  muxcat mongodb indexes drop users email_1 --yes
  muxcat mongodb indexes drop sessions createdAt_1 --db shop --yes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn); err != nil {
				return err
			}
			dbName, err := resolveDB(cmd, conn)
			if err != nil {
				return err
			}
			if err := confirmDestructive(cmd,
				fmt.Sprintf("Drop index %s on %s.%s?", args[1], dbName, args[0])); err != nil {
				return err
			}
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			client, err := connect(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = client.Disconnect(context.Background()) }()
			coll := client.Database(dbName).Collection(args[0])

			if err := coll.Indexes().DropOne(ctx, args[1]); err != nil {
				var srvErr mongo.ServerError
				if errors.As(err, &srvErr) && srvErr.HasErrorCode(27) { // IndexNotFound
					return output.NewError(output.CodeQueryError,
						"index does not exist: "+args[1],
						"list indexes with muxcat mongodb indexes ls "+args[0])
				}
				return classifyErr(err, "drop index failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: fmt.Sprintf("dropped index %s on %s.%s", args[1], dbName, args[0]),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().Bool("yes", false, "skip the confirmation prompt")
	return c
}
