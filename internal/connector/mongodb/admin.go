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
