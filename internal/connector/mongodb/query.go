package mongodb

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// stdinIsTTY reports whether stdin is a terminal; a variable so tests can
// stub it.
var stdinIsTTY = func() bool {
	return isatty.IsTerminal(os.Stdin.Fd())
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

// resolveDB resolves the target database: --db flag > the connection's
// default database.
func resolveDB(cmd *cobra.Command, conn Connection) (string, error) {
	db := cli.FlagString(cmd, "db")
	if db == "" {
		db = conn.Database
	}
	if db == "" {
		return "", output.NewError(output.CodeMissingArgument,
			"no database specified",
			"pass --db, or set a default database on the connection (muxcat mongodb conn add ... --database)")
	}
	return db, nil
}

// collectionOf resolves the target collection on the resolved database.
func collectionOf(client *mongo.Client, cmd *cobra.Command, conn Connection, name string) (*mongo.Collection, error) {
	db, err := resolveDB(cmd, conn)
	if err != nil {
		return nil, err
	}
	return client.Database(db).Collection(name), nil
}

// resolveJSONInput resolves a JSON document input (filter, pipeline): the
// positional argument wins, then --file <path>, --file - (stdin), or a
// piped stdin.
func resolveJSONInput(cmd *cobra.Command, args []string, what string) (string, error) {
	file := cli.FlagString(cmd, "file")
	if len(args) == 1 && file != "" {
		return "", output.NewError(output.CodeConfigInvalid,
			what+" given both as an argument and via --file",
			"choose one: positional argument or --file <path>")
	}
	if len(args) == 1 {
		return args[0], nil
	}
	if file != "" {
		if file == "-" {
			return readStdin(cmd, what)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", output.NewError(output.CodeMissingArgument,
				"cannot read --file "+file+": "+err.Error(), "")
		}
		return string(data), nil
	}
	if stdinIsTTY() {
		return "", output.NewError(output.CodeMissingArgument,
			"no "+what+" provided",
			"pass the "+what+" as an argument, via --file <path>, or through stdin")
	}
	return readStdin(cmd, what)
}

func readStdin(cmd *cobra.Command, what string) (string, error) {
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", output.NewError(output.CodeMissingArgument,
			"cannot read "+what+" from stdin: "+err.Error(), "")
	}
	return string(data), nil
}

// parseFilter parses an Extended JSON document; empty input matches all
// documents.
func parseFilter(s string) (bson.D, error) {
	if strings.TrimSpace(s) == "" {
		return bson.D{}, nil
	}
	var f bson.D
	if err := bson.UnmarshalExtJSON([]byte(s), false, &f); err != nil {
		return nil, output.NewError(output.CodeConfigInvalid,
			"invalid filter JSON: "+err.Error(),
			`examples: {"status":"ok"}, {"_id":{"$oid":"..."}} (Extended JSON)`)
	}
	return f, nil
}

// parsePipeline parses an Extended JSON pipeline; every stage must be a
// document, and write stages ($out/$merge) are rejected — aggregate is
// read-only.
func parsePipeline(s string) (bson.A, error) {
	var p bson.A
	if err := bson.UnmarshalExtJSON([]byte(s), false, &p); err != nil {
		return nil, output.NewError(output.CodeConfigInvalid,
			"invalid pipeline JSON: "+err.Error(), "")
	}
	for _, stage := range p {
		doc, ok := stage.(bson.D)
		if !ok {
			return nil, output.NewError(output.CodeConfigInvalid,
				"invalid pipeline JSON: every stage must be a document", "")
		}
		if len(doc) > 0 && (doc[0].Key == "$out" || doc[0].Key == "$merge") {
			return nil, output.NewError(output.CodeReadonlyViolation,
				"aggregate is read-only: the "+doc[0].Key+" stage writes to a collection",
				"write stages ($out/$merge) are rejected; remove "+doc[0].Key+" from the pipeline")
		}
	}
	return p, nil
}

// parseSorts parses repeatable --sort values (field:asc|desc) into an
// order-preserving sort document.
func parseSorts(sorts []string) (bson.D, error) {
	d := bson.D{}
	for _, s := range sorts {
		field, order, _ := strings.Cut(s, ":")
		if field == "" {
			return nil, output.NewError(output.CodeConfigInvalid,
				"invalid --sort value: "+s, "expected field:asc|desc")
		}
		dir := int32(1)
		switch order {
		case "", "asc":
		case "desc":
			dir = -1
		default:
			return nil, output.NewError(output.CodeConfigInvalid,
				"invalid --sort order in: "+s, "valid orders: asc|desc")
		}
		d = append(d, bson.E{Key: field, Value: dir})
	}
	return d, nil
}

func newQueryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "query <collection> [filter]",
		Short: "Find documents (Extended JSON filter, projection and sort)",
		Long: `Find documents in a collection. The filter is an Extended JSON
document given as the positional argument, via --file <path>, --file -
(stdin), or a piped stdin; omitted (or empty) matches all documents.
--projection is an Extended JSON document, --sort is a repeatable
field:asc|desc clause, --skip offsets the result. Documents are capped
by --limit (meta.truncated reports an early stop).`,
		Args: cobra.RangeArgs(1, 2),
		Example: `  muxcat mongodb query users
  muxcat mongodb query users '{"age":{"$gte":18}}' --sort age:desc --projection '{"name":1,"age":1}'
  muxcat mongodb query users --file filter.json --db shop
  echo '{"status":"ok"}' | muxcat mongodb query users`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			// The filter may be omitted entirely (find all); stdin is only
			// read when it is not a TTY.
			var filterText string
			if len(args) == 2 || cli.FlagString(cmd, "file") != "" || !stdinIsTTY() {
				filterText, err = resolveJSONInput(cmd, args[1:], "filter")
				if err != nil {
					return err
				}
			}
			skip, _ := cmd.Flags().GetInt("skip")
			if skip < 0 {
				return output.NewError(output.CodeConfigInvalid, "--skip must be >= 0", "")
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

			filter, err := parseFilter(filterText)
			if err != nil {
				return err
			}
			opts := options.Find()
			if s := cli.FlagString(cmd, "projection"); s != "" {
				var projection bson.D
				if err := bson.UnmarshalExtJSON([]byte(s), false, &projection); err != nil {
					return output.NewError(output.CodeConfigInvalid,
						"invalid projection JSON: "+err.Error(),
						`example: {"name":1,"age":1}`)
				}
				opts.SetProjection(projection)
			}
			sorts, _ := cmd.Flags().GetStringArray("sort")
			if len(sorts) > 0 {
				sortDoc, err := parseSorts(sorts)
				if err != nil {
					return err
				}
				opts.SetSort(sortDoc)
			}
			if skip > 0 {
				opts.SetSkip(int64(skip))
			}
			limit := cli.FlagLimit(cmd)
			if limit > 0 {
				// Read one extra document to detect truncation.
				opts.SetLimit(int64(limit) + 1)
			}
			cur, err := coll.Find(ctx, filter, opts)
			if err != nil {
				return classifyErr(err, "query failed")
			}
			defer func() { _ = cur.Close(ctx) }()
			docs, truncated, err := collectDocs(ctx, cur, limit)
			if err != nil {
				return err
			}
			return renderDocs(cmd, name, docs, truncated, start)
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().String("projection", "", "projection document (Extended JSON, e.g. {\"name\":1})")
	c.Flags().StringArray("sort", nil, "sort clause field:asc|desc (repeatable)")
	c.Flags().Int("skip", 0, "number of documents to skip")
	c.Flags().String("file", "", "read the filter from a file (- reads from stdin)")
	return c
}

func newAggregateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "aggregate <collection> [pipeline]",
		Short: "Run a read-only aggregation pipeline (Extended JSON array)",
		Long: `Run an aggregation pipeline against a collection. The pipeline is
an Extended JSON array of stage documents given as the positional
argument, via --file <path>, --file - (stdin), or a piped stdin; it is
required. aggregate is read-only: $out/$merge write stages are rejected
(READONLY_VIOLATION). Documents are capped by --limit (meta.truncated
reports an early stop).`,
		Args: cobra.RangeArgs(1, 2),
		Example: `  muxcat mongodb aggregate orders '[{"$group":{"_id":"$status","n":{"$sum":1}}}]'
  muxcat mongodb aggregate orders --file pipeline.json --db shop
  cat pipeline.json | muxcat mongodb aggregate orders`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			pipeText, err := resolveJSONInput(cmd, args[1:], "pipeline")
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
			pipeline, err := parsePipeline(pipeText)
			if err != nil {
				return err
			}
			cur, err := coll.Aggregate(ctx, pipeline)
			if err != nil {
				return classifyErr(err, "aggregate failed")
			}
			defer func() { _ = cur.Close(ctx) }()
			limit := cli.FlagLimit(cmd)
			docs, truncated, err := collectDocs(ctx, cur, limit)
			if err != nil {
				return err
			}
			return renderDocs(cmd, name, docs, truncated, start)
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	c.Flags().String("file", "", "read the pipeline from a file (- reads from stdin)")
	return c
}

// collectDocs reads up to limit+1 documents; the extra document marks the
// result truncated and is dropped. limit 0 collects everything.
func collectDocs(ctx context.Context, cur *mongo.Cursor, limit int) ([]bson.D, bool, error) {
	docs := make([]bson.D, 0)
	truncated := false
	for cur.Next(ctx) {
		if limit > 0 && len(docs) >= limit {
			truncated = true
			break
		}
		var d bson.D
		if err := cur.Decode(&d); err != nil {
			return nil, false, classifyErr(err, "failed to read results")
		}
		docs = append(docs, d)
	}
	if err := cur.Err(); err != nil {
		return nil, false, classifyErr(err, "failed to read results")
	}
	return docs, truncated, nil
}
