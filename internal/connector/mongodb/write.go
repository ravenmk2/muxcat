package mongodb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// requireWritable refuses write commands on a readonly connection before
// any dialing.
func requireWritable(conn Connection) error {
	if conn.Readonly {
		return output.NewError(output.CodeReadonlyViolation,
			"write commands are not allowed on a readonly connection",
			"use a writable connection (-c)")
	}
	return nil
}

// confirmDestructive guards irreversible operations: --yes skips the
// confirmation, non-TTY without --yes fails, TTY asks via huh.
func confirmDestructive(cmd *cobra.Command, prompt string) error {
	if cli.FlagBool(cmd, "yes") {
		return nil
	}
	if !cli.RuntimeFrom(cmd.Context()).Interactive {
		return output.NewError(output.CodeMissingArgument,
			strings.TrimSuffix(prompt, "?")+" requires confirmation",
			"pass --yes in non-interactive environments")
	}
	confirm := false
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(prompt).Value(&confirm),
	))
	if err := form.Run(); err != nil {
		return err
	}
	if !confirm {
		return output.NewError(output.CodeGeneral, "cancelled", "")
	}
	return nil
}

// parseDocsInput parses insert input: a single document, an Extended JSON
// array of documents, or (jsonl) line-delimited documents. single reports
// the single-document shape (InsertOne); array and JSONL input go to
// InsertMany.
func parseDocsInput(s string, jsonl bool) (docs []any, single bool, err error) {
	if jsonl {
		for i, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var d bson.D
			if err := bson.UnmarshalExtJSON([]byte(line), false, &d); err != nil {
				return nil, false, output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid JSON on line %d: %s", i+1, err), "")
			}
			docs = append(docs, d)
		}
		if len(docs) == 0 {
			return nil, false, output.NewError(output.CodeMissingArgument,
				"no documents in the input",
				"pipe line-delimited JSON documents, one per line")
		}
		return docs, false, nil
	}
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(s), false, &d); err == nil {
		return []any{d}, true, nil
	}
	var a bson.A
	if err := bson.UnmarshalExtJSON([]byte(s), false, &a); err != nil {
		return nil, false, output.NewError(output.CodeConfigInvalid,
			"invalid document JSON: "+err.Error(),
			`accepted shapes: a single document {"a":1}, an array [{...},{...}], or line-delimited JSON with --jsonl`)
	}
	for _, el := range a {
		doc, ok := el.(bson.D)
		if !ok {
			return nil, false, output.NewError(output.CodeConfigInvalid,
				"invalid document JSON: every array element must be a document", "")
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return nil, false, output.NewError(output.CodeMissingArgument,
			"no documents in the input", "")
	}
	return docs, false, nil
}

// parseUpdateDoc parses the update argument: an Extended JSON document,
// either with update operators ($set, ...) or a replacement document; it
// must not be empty.
func parseUpdateDoc(s string) (bson.D, error) {
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(s), false, &d); err != nil {
		return nil, output.NewError(output.CodeConfigInvalid,
			"invalid update JSON: "+err.Error(),
			`examples: {"$set":{"status":"done"}} or a replacement document`)
	}
	if len(d) == 0 {
		return nil, output.NewError(output.CodeConfigInvalid,
			"the update document must not be empty",
			`examples: {"$set":{"status":"done"}} or a replacement document`)
	}
	return d, nil
}

func newInsertCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "insert <collection> [doc]",
		Short: "Insert documents (single doc, Extended JSON array, or --jsonl lines)",
		Long: `Insert documents into a collection (created implicitly). The input
is a single Extended JSON document (InsertOne), an Extended JSON array
of documents (InsertMany), or — with --jsonl — line-delimited documents
(mongoimport convention), given as the positional argument, via
--file <path>, --file - (stdin), or a piped stdin.`,
		Args: cobra.RangeArgs(1, 2),
		Example: `  muxcat mongodb insert users '{"name":"a","age":30}'
  muxcat mongodb insert users '[{"name":"a"},{"name":"b"}]'
  muxcat mongodb insert users --file docs.jsonl --jsonl
  echo '{"name":"a"}' | muxcat mongodb insert users --db shop`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn); err != nil {
				return err
			}
			docText, err := resolveJSONInput(cmd, args[1:], "document(s)")
			if err != nil {
				return err
			}
			docs, single, err := parseDocsInput(docText, cli.FlagBool(cmd, "jsonl"))
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

			var ids []any
			if single {
				res, err := coll.InsertOne(ctx, docs[0])
				if err != nil {
					return classifyErr(err, "insert failed")
				}
				ids = []any{res.InsertedID}
			} else {
				res, err := coll.InsertMany(ctx, docs)
				if err != nil {
					return classifyErr(err, "insert failed")
				}
				ids = res.InsertedIDs
			}
			textIDs := make([]any, len(ids))
			jsonIDs := make([]any, len(ids))
			for i, id := range ids {
				if oid, ok := id.(bson.ObjectID); ok {
					textIDs[i] = oid.Hex()
				} else {
					textIDs[i] = fmt.Sprint(id)
				}
				jsonIDs[i], err = relaxedJSON(id)
				if err != nil {
					return output.NewError(output.CodeGeneral, "failed to encode an inserted id: "+err.Error(), "")
				}
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    map[string]any{"insertedCount": len(ids), "insertedIds": textIDs},
				JSONData: map[string]any{"insertedCount": len(ids), "insertedIds": jsonIDs},
				Message:  fmt.Sprintf("inserted %d document(s) into %s", len(ids), args[0]),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().String("file", "", "read the documents from a file (- reads from stdin)")
	c.Flags().Bool("jsonl", false, "read line-delimited JSON documents (one per line)")
	return c
}

func newUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update <collection> <filter> <update>",
		Short: "Update documents (single by default, --many for all matches)",
		Long: `Update documents in a collection. filter is an Extended JSON query
document; update is an Extended JSON document with update operators
(e.g. {"$set":...}) or a replacement document. By default only the
first matching document is updated (UpdateOne); --many updates all
matches, --upsert inserts when nothing matches.`,
		Args: cli.ExactArgs(3, "<collection> <filter> <update>", "collection", "filter", "update"),
		Example: `  muxcat mongodb update users '{"name":"a"}' '{"$set":{"active":true}}'
  muxcat mongodb update users '{"status":"pending"}' '{"$set":{"status":"done"}}' --many
  muxcat mongodb update settings '{"_id":"theme"}' '{"$set":{"value":"dark"}}' --upsert --db shop`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn); err != nil {
				return err
			}
			filter, err := parseFilter(args[1])
			if err != nil {
				return err
			}
			update, err := parseUpdateDoc(args[2])
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

			many := cli.FlagBool(cmd, "many")
			upsert := cli.FlagBool(cmd, "upsert")
			var res *mongo.UpdateResult
			if many {
				res, err = coll.UpdateMany(ctx, filter, update, options.UpdateMany().SetUpsert(upsert))
			} else {
				res, err = coll.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(upsert))
			}
			if err != nil {
				return classifyErr(err, "update failed")
			}
			value := map[string]any{
				"matchedCount":  res.MatchedCount,
				"modifiedCount": res.ModifiedCount,
				"upsertedCount": res.UpsertedCount,
			}
			jsonValue := map[string]any{
				"matchedCount":  res.MatchedCount,
				"modifiedCount": res.ModifiedCount,
				"upsertedCount": res.UpsertedCount,
			}
			message := fmt.Sprintf("matched %d, modified %d", res.MatchedCount, res.ModifiedCount)
			if res.UpsertedID != nil {
				if oid, ok := res.UpsertedID.(bson.ObjectID); ok {
					value["upsertedId"] = oid.Hex()
				} else {
					value["upsertedId"] = fmt.Sprint(res.UpsertedID)
				}
				v, err := relaxedJSON(res.UpsertedID)
				if err != nil {
					return output.NewError(output.CodeGeneral, "failed to encode the upserted id: "+err.Error(), "")
				}
				jsonValue["upsertedId"] = v
				message += fmt.Sprintf(", upserted %d", res.UpsertedCount)
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    value,
				JSONData: jsonValue,
				Message:  message,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().Bool("many", false, "update all matching documents (default: first match only)")
	c.Flags().Bool("upsert", false, "insert the update document when nothing matches")
	return c
}

func newDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete <collection> [filter]",
		Short: "Delete documents (single by default, --many for all matches)",
		Long: `Delete documents from a collection. filter is an Extended JSON
query document given as the positional argument, via --file, or a piped
stdin; omitted (or empty) matches all documents. By default only the
first matching document is deleted (DeleteOne); --many deletes all
matches. Deleting all documents of a collection (--many with an empty
filter) requires confirmation (--yes on non-TTY).`,
		Args: cobra.RangeArgs(1, 2),
		Example: `  muxcat mongodb delete users '{"name":"a"}'
  muxcat mongodb delete users '{"status":"expired"}' --many
  muxcat mongodb delete sessions --many --yes   # wipe the collection`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn); err != nil {
				return err
			}
			var filterText string
			if len(args) == 2 || cli.FlagString(cmd, "file") != "" || !stdinIsTTY() {
				filterText, err = resolveJSONInput(cmd, args[1:], "filter")
				if err != nil {
					return err
				}
			}
			filter, err := parseFilter(filterText)
			if err != nil {
				return err
			}
			many := cli.FlagBool(cmd, "many")
			dbName, err := resolveDB(cmd, conn)
			if err != nil {
				return err
			}
			if many && len(filter) == 0 {
				if err := confirmDestructive(cmd,
					fmt.Sprintf("Delete ALL documents in %s.%s?", dbName, args[0])); err != nil {
					return err
				}
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

			var res *mongo.DeleteResult
			if many {
				res, err = coll.DeleteMany(ctx, filter)
			} else {
				res, err = coll.DeleteOne(ctx, filter)
			}
			if err != nil {
				return classifyErr(err, "delete failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"deletedCount": res.DeletedCount},
				Message: fmt.Sprintf("deleted %d document(s) from %s", res.DeletedCount, args[0]),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().Bool("many", false, "delete all matching documents (default: first match only)")
	c.Flags().String("file", "", "read the filter from a file (- reads from stdin)")
	c.Flags().Bool("yes", false, "skip the confirmation prompt")
	return c
}

func newDropCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "drop <collection>",
		Short: "Drop a collection (irreversible, requires confirmation)",
		Args:  cli.ExactArgs(1, "<collection>", "collection"),
		Example: `  muxcat mongodb drop sessions --yes
  muxcat mongodb drop staging --db shop --yes`,
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
				fmt.Sprintf("Drop collection %s.%s?", dbName, args[0])); err != nil {
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

			// RunCommand (not Collection.Drop, which swallows
			// NamespaceNotFound) so a missing collection is an error.
			// Modern servers make drop idempotent: the response carries
			// "ns" only when a collection was actually dropped.
			var res bson.M
			err = client.Database(dbName).RunCommand(ctx,
				bson.D{{Key: "drop", Value: args[0]}}).Decode(&res)
			if err != nil {
				var srvErr mongo.ServerError
				if errors.As(err, &srvErr) && srvErr.HasErrorCode(26) { // NamespaceNotFound
					return output.NewError(output.CodeQueryError,
						"collection does not exist: "+dbName+"."+args[0],
						"list collections with muxcat mongodb collections")
				}
				return classifyErr(err, "drop failed")
			}
			if _, ok := res["ns"]; !ok {
				return output.NewError(output.CodeQueryError,
					"collection does not exist: "+dbName+"."+args[0],
					"list collections with muxcat mongodb collections")
			}
			return cli.RenderResult(cmd, &output.Result{
				Message: "dropped collection " + dbName + "." + args[0],
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().Bool("yes", false, "skip the confirmation prompt")
	return c
}
