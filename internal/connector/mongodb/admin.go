package mongodb

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newDbsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dbs",
		Short: "List databases with their sizes",
		Args:  cobra.NoArgs,
		Example: `  muxcat mongodb dbs
  muxcat mongodb dbs --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
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

			var res struct {
				Databases bson.A `bson:"databases"`
			}
			if err := client.Database("admin").RunCommand(ctx,
				bson.D{{Key: "listDatabases", Value: 1}}).Decode(&res); err != nil {
				return classifyErr(err, "list databases failed")
			}
			rows := make([][]any, 0, len(res.Databases))
			for _, raw := range res.Databases {
				d, _ := raw.(bson.D)
				rows = append(rows, []any{docLookup(d, "name"), docLookup(d, "sizeOnDisk"), docLookup(d, "empty")})
			}
			jsonData, err := relaxedJSON(res.Databases)
			if err != nil {
				return output.NewError(output.CodeGeneral, "failed to encode the database list: "+err.Error(), "")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "sizeOnDisk", "empty"},
				Rows:     rows,
				Message:  fmt.Sprintf("%d databases", len(res.Databases)),
				JSONData: jsonData,
			}, meta(name, start, false))
		},
	}
}

func newCollectionsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "collections",
		Short: "List collections in the database",
		Args:  cobra.NoArgs,
		Example: `  muxcat mongodb collections
  muxcat mongodb collections --db shop --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			dbName, err := resolveDB(cmd, conn)
			if err != nil {
				return err
			}

			cur, err := client.Database(dbName).ListCollections(ctx, bson.D{})
			if err != nil {
				return classifyErr(err, "list collections failed")
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
				rows = append(rows, []any{docLookup(d, "name"), docLookup(d, "type")})
			}
			jsonData, err := relaxedJSON(docs)
			if err != nil {
				return output.NewError(output.CodeGeneral, "failed to encode the collection list: "+err.Error(), "")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "type"},
				Rows:     rows,
				Message:  fmt.Sprintf("%d collections", len(docs)),
				JSONData: jsonData,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	return c
}

func newStatsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "stats [collection]",
		Short: "Show database or collection statistics (dbStats / collStats)",
		Args:  cobra.MaximumNArgs(1),
		Example: `  muxcat mongodb stats
  muxcat mongodb stats users
  muxcat mongodb stats users --db shop --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			dbName, err := resolveDB(cmd, conn)
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

			command := bson.D{{Key: "dbStats", Value: 1}}
			if len(args) == 1 {
				command = bson.D{{Key: "collStats", Value: args[0]}}
			}
			var res bson.D
			if err := client.Database(dbName).RunCommand(ctx, command).Decode(&res); err != nil {
				return classifyErr(err, "stats failed")
			}
			jsonData, err := relaxedJSON(res)
			if err != nil {
				return output.NewError(output.CodeGeneral, "failed to encode the stats: "+err.Error(), "")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    statsValue(res),
				JSONData: jsonData,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	return c
}

// statsValue picks the summary fields of a dbStats/collStats response;
// fields absent from the response (version differences) are omitted.
func statsValue(doc bson.D) map[string]any {
	v := map[string]any{}
	for _, k := range []string{
		"db", "ns", "collections", "objects", "count", "size",
		"avgObjSize", "dataSize", "storageSize", "totalIndexSize",
		"indexes", "indexSize", "nindexes",
	} {
		if val := docLookup(doc, k); val != nil {
			v[k] = val
		}
	}
	return v
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show a serverStatus summary (--json for the full document)",
		Args:  cobra.NoArgs,
		Example: `  muxcat mongodb status
  muxcat mongodb status --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
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

			var res bson.D
			if err := client.Database("admin").RunCommand(ctx,
				bson.D{{Key: "serverStatus", Value: 1}}).Decode(&res); err != nil {
				return classifyErr(err, "server status failed")
			}
			jsonData, err := relaxedJSON(res)
			if err != nil {
				return output.NewError(output.CodeGeneral, "failed to encode the server status: "+err.Error(), "")
			}
			value := statusValue(res)
			return cli.RenderResult(cmd, &output.Result{
				Value:    value,
				JSONData: jsonData,
			}, meta(name, start, false))
		},
	}
}

// statusValue picks the summary fields of a serverStatus response as flat
// dotted keys; missing sections (e.g. no wiredTiger on in-memory engines)
// are skipped. serverStatus mem figures are already in MB.
func statusValue(doc bson.D) map[string]any {
	v := map[string]any{}
	for _, k := range []string{"host", "version", "process", "pid", "uptime"} {
		if val := docLookup(doc, k); val != nil {
			v[k] = val
		}
	}
	if sub := subDoc(doc, "connections"); sub != nil {
		for _, k := range []string{"current", "available", "active"} {
			if val := docLookup(sub, k); val != nil {
				v["connections."+k] = val
			}
		}
	}
	if sub := subDoc(doc, "mem"); sub != nil {
		for _, k := range []string{"resident", "virtual"} {
			if val := docLookup(sub, k); val != nil {
				v["mem."+k] = val
			}
		}
	}
	if sub := subDoc(doc, "opcounters"); sub != nil {
		for _, k := range []string{"insert", "query", "update", "delete", "getmore", "command"} {
			if val := docLookup(sub, k); val != nil {
				v["opcounters."+k] = val
			}
		}
	}
	if wt := subDoc(doc, "wiredTiger"); wt != nil {
		if cache := subDoc(wt, "cache"); cache != nil {
			// 8.0 renamed the field to "bytes currently in the cache".
			cur := docLookup(cache, "bytes currently in the cache")
			if cur == nil {
				cur = docLookup(cache, "bytes currently in cache")
			}
			if cur != nil {
				v["wiredTiger.cache.currentBytes"] = cur
			}
			if val := docLookup(cache, "maximum bytes configured"); val != nil {
				v["wiredTiger.cache.maxBytes"] = val
			}
		}
	}
	return v
}

// subDoc returns a nested document, or nil when absent or not a document.
func subDoc(d bson.D, key string) bson.D {
	if sub, ok := docLookup(d, key).(bson.D); ok {
		return sub
	}
	return nil
}
