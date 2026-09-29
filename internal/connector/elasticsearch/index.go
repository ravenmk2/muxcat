package elasticsearch

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// catJSON fetches a _cat JSON endpoint and decodes the array of row
// objects. _cat numeric fields arrive as strings; callers convert with
// catInt/catNum.
func (c *client) catJSON(ctx context.Context, path string) ([]map[string]any, error) {
	resp, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(resp.body, &rows); err != nil {
		return nil, output.NewError(output.CodeQueryError,
			"response is not valid JSON: "+err.Error(), "")
	}
	return rows, nil
}

// catStr reads a _cat string field.
func catStr(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}

// catInt converts a _cat string field to int64; unparseable or missing
// values yield 0.
func catInt(row map[string]any, key string) int64 {
	n, _ := strconv.ParseInt(catStr(row, key), 10, 64)
	return n
}

// catNum converts a _cat string field to a number (int64 when integral,
// float64 otherwise); unparseable or missing values yield the empty string.
func catNum(row map[string]any, key string) any {
	s := catStr(row, key)
	if s == "" {
		return ""
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func newIndexCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "index",
		Short: "Inspect Elasticsearch indices",
		Long: `Inspect indices: list them with health/doc-count/size from the
_cat API, or show one index's settings, mappings and aliases.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newIndexLsCmd(), newIndexShowCmd())
	return c
}

func newIndexLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all indices",
		Args:  cobra.NoArgs,
		Example: `  muxcat es index ls
  muxcat es index ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			rows, err := cl.catJSON(cmd.Context(),
				"/_cat/indices?format=json&h=health,status,index,docs.count,store.size,pri,rep&s=index")
			if err != nil {
				return err
			}
			out := make([][]any, 0, len(rows))
			for _, r := range rows {
				out = append(out, []any{
					catStr(r, "index"), catStr(r, "health"), catStr(r, "status"),
					catInt(r, "docs.count"), catStr(r, "store.size"),
					catInt(r, "pri"), catInt(r, "rep"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "health", "status", "docs", "size", "pri", "rep"},
				Rows:    out,
			}, meta(name, start, false))
		},
	}
}

func newIndexShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show an index's settings, mappings and aliases",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat es index show app-logs
  muxcat es index show app-logs --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			index := args[0]
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/"+url.PathEscape(index), nil)
			if err != nil {
				return err
			}
			var body map[string]any
			if err := json.Unmarshal(resp.body, &body); err != nil {
				return output.NewError(output.CodeQueryError,
					"response is not valid JSON: "+err.Error(), "")
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    indexSummary(index, body),
				JSONData: body,
			}, meta(name, start, false))
		},
	}
}

// indexSummary extracts the stable parts of GET /<index>: shard/replica
// settings, top-level mapping fields and alias names.
func indexSummary(index string, body map[string]any) map[string]any {
	value := map[string]any{"index": index}
	doc, _ := body[index].(map[string]any)
	if len(body) == 1 {
		for _, v := range body {
			doc, _ = v.(map[string]any)
		}
	}
	if settings, _ := doc["settings"].(map[string]any); settings != nil {
		if idx, _ := settings["index"].(map[string]any); idx != nil {
			value["shards"] = catIntStr(idx["number_of_shards"])
			value["replicas"] = catIntStr(idx["number_of_replicas"])
		}
	}
	if mappings, _ := doc["mappings"].(map[string]any); mappings != nil {
		if props, _ := mappings["properties"].(map[string]any); props != nil {
			fields := make([]string, 0, len(props))
			for k := range props {
				fields = append(fields, k)
			}
			sort.Strings(fields)
			value["field_count"] = len(fields)
			value["fields"] = fields
		}
	}
	if aliases, _ := doc["aliases"].(map[string]any); aliases != nil {
		names := make([]string, 0, len(aliases))
		for k := range aliases {
			names = append(names, k)
		}
		sort.Strings(names)
		value["aliases"] = names
	}
	return value
}

// catIntStr converts a settings value (string or number) to int64.
func catIntStr(v any) any {
	switch t := v.(type) {
	case string:
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return n
		}
		return t
	case float64:
		return int64(t)
	default:
		return v
	}
}
