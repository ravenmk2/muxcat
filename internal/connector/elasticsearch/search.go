package elasticsearch

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// maxSourceColumns caps the extra _source columns in the search table;
// wider documents should be read via --json.
const maxSourceColumns = 15

func newSearchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search <index>",
		Short: "Search an index (Lucene query_string or a full DSL body)",
		Long: `Search an index with a Lucene query_string (-q) or a full
request body (--file). The two are mutually exclusive; --file owns the
whole DSL body, so --size/--from/--sort cannot be combined with it.
The result table holds _id, _score and the union of scalar _source keys
(capped at 15 columns; use --json for wider documents).`,
		Args: cli.ExactArgs(1, "<index>", "index"),
		Example: `  muxcat es search app-logs --query "level:error" --size 20
  muxcat es search app-logs -q "status:[500 TO 599]" --sort "@timestamp:desc"
  muxcat es search app-logs --file query.json
  cat dsl.json | muxcat es search app-logs --file -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			index := args[0]
			query := cli.FlagString(cmd, "query")
			file := cli.FlagString(cmd, "file")
			if query != "" && file != "" {
				return output.NewError(output.CodeConfigInvalid,
					"--query and --file are mutually exclusive",
					"use -q for a Lucene query_string, or --file for a full DSL body")
			}
			size, _ := cmd.Flags().GetInt("size")
			from, _ := cmd.Flags().GetInt("from")
			if size < 0 || from < 0 {
				return output.NewError(output.CodeConfigInvalid,
					"--size and --from must be >= 0", "")
			}
			sorts, _ := cmd.Flags().GetStringArray("sort")
			if file != "" && (cmd.Flags().Changed("size") || cmd.Flags().Changed("from") || len(sorts) > 0) {
				return output.NewError(output.CodeConfigInvalid,
					"--size/--from/--sort cannot be combined with --file",
					"with --file the DSL body owns its own pagination and sorting")
			}

			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}

			var body []byte
			if file != "" {
				if body, err = resolveBody(cmd); err != nil {
					return err
				}
			} else if body, err = searchBody(query, size, from, sorts); err != nil {
				return err
			}

			resp, err := cl.do(cmd.Context(), "POST", "/"+url.PathEscape(index)+"/_search", body)
			if err != nil {
				return err
			}

			sr, err := parseSearchResponse(resp.body)
			if err != nil {
				return err
			}
			columns, rows := searchTable(sr.hits)
			message := fmt.Sprintf("%d hits (total %d, %d ms)", len(sr.hits), sr.total, sr.took)
			if sr.relation == "gte" {
				message = fmt.Sprintf("%d hits (total >= %d, %d ms)", len(sr.hits), sr.total, sr.took)
			}
			if sr.timedOut {
				message += ", timed out"
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns:  columns,
				Rows:     rows,
				Message:  message,
				JSONData: sr.raw,
			}, meta(name, start, int64(len(sr.hits)) < sr.total))
		},
	}
	c.Flags().StringP("query", "q", "", "Lucene query_string (e.g. level:error)")
	c.Flags().String("file", "", "read a full DSL request body from a file (- reads stdin)")
	c.Flags().Int("size", 10, "number of hits to return")
	c.Flags().Int("from", 0, "offset of the first hit")
	c.Flags().StringArray("sort", nil, "sort clause field:asc|desc (repeatable)")
	return c
}

// searchBody builds the request body for --query / default mode. All user
// input travels through encoding/json; nothing is string-concatenated.
func searchBody(query string, size, from int, sorts []string) ([]byte, error) {
	var q any
	if query == "" {
		q = map[string]any{"match_all": map[string]any{}}
	} else {
		q = map[string]any{"query_string": map[string]any{"query": query}}
	}
	body := map[string]any{"query": q, "size": size, "from": from}
	if len(sorts) > 0 {
		clauses := make([]map[string]any, 0, len(sorts))
		for _, s := range sorts {
			field, order, _ := strings.Cut(s, ":")
			if field == "" {
				return nil, output.NewError(output.CodeConfigInvalid,
					"invalid --sort value: "+s, "expected field:asc|desc")
			}
			if order == "" {
				order = "asc"
			}
			if order != "asc" && order != "desc" {
				return nil, output.NewError(output.CodeConfigInvalid,
					"invalid --sort order in: "+s, "valid orders: asc|desc")
			}
			clauses = append(clauses, map[string]any{field: map[string]any{"order": order}})
		}
		body["sort"] = clauses
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, output.NewError(output.CodeGeneral, "failed to build the search body: "+err.Error(), "")
	}
	return raw, nil
}

// searchHit is one entry of hits.hits.
type searchHit struct {
	id     string
	score  any
	source map[string]any
}

// searchResult is the parsed _search response.
type searchResult struct {
	raw      any
	took     int64
	timedOut bool
	total    int64
	relation string
	hits     []searchHit
}

// parseSearchResponse decodes a _search body, tolerating both hits.total
// shapes: a bare number (7.x with rest_total_hits_as_int) and the object
// {"value":N,"relation":"eq|gte"} (8/9 default).
func parseSearchResponse(raw []byte) (*searchResult, error) {
	var decoded struct {
		Took     int64 `json:"took"`
		TimedOut bool  `json:"timed_out"`
		Hits     struct {
			Total json.RawMessage `json:"total"`
			Hits  []struct {
				ID     string         `json:"_id"`
				Score  any            `json:"_score"`
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, output.NewError(output.CodeQueryError,
			"response is not valid JSON: "+err.Error(), "")
	}
	total, relation := parseHitsTotal(decoded.Hits.Total)
	var generic any
	_ = json.Unmarshal(raw, &generic)
	sr := &searchResult{
		raw:      generic,
		took:     decoded.Took,
		timedOut: decoded.TimedOut,
		total:    total,
		relation: relation,
	}
	for _, h := range decoded.Hits.Hits {
		sr.hits = append(sr.hits, searchHit{id: h.ID, score: h.Score, source: h.Source})
	}
	return sr, nil
}

// parseHitsTotal accepts both hits.total shapes; relation defaults to "eq".
func parseHitsTotal(raw json.RawMessage) (int64, string) {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, "eq"
	}
	var obj struct {
		Value    int64  `json:"value"`
		Relation string `json:"relation"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Relation == "" {
			obj.Relation = "eq"
		}
		return obj.Value, obj.Relation
	}
	return 0, "eq"
}

// searchTable flattens hits into table columns and rows: _id, _score, then
// the sorted union of top-level _source keys (capped at maxSourceColumns).
// Non-scalar values render as compact JSON; missing keys yield empty cells.
func searchTable(hits []searchHit) ([]string, [][]any) {
	keySet := map[string]bool{}
	for _, h := range hits {
		for k := range h.source {
			keySet[k] = true
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxSourceColumns {
		keys = keys[:maxSourceColumns]
	}

	columns := append([]string{"_id", "_score"}, keys...)
	rows := make([][]any, 0, len(hits))
	for _, h := range hits {
		row := make([]any, 0, len(columns))
		row = append(row, h.id, cellValue(h.score))
		for _, k := range keys {
			row = append(row, cellValue(h.source[k]))
		}
		rows = append(rows, row)
	}
	return columns, rows
}

// cellValue renders a _source value for a table cell: scalars as-is,
// composites as compact JSON, missing/nil as "".
func cellValue(v any) any {
	switch t := v.(type) {
	case nil:
		return ""
	case string, bool, float64, json.Number:
		return t
	default:
		if raw, err := json.Marshal(t); err == nil {
			return string(raw)
		}
		return fmt.Sprint(t)
	}
}
