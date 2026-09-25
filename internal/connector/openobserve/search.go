package openobserve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// lastDurations is the --last enum: common relative time windows, aligned
// with the OpenObserve UI's relative time options.
var lastDurations = map[string]time.Duration{
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1h":  time.Hour,
	"3h":  3 * time.Hour,
	"6h":  6 * time.Hour,
	"12h": 12 * time.Hour,
	"24h": 24 * time.Hour,
	"2d":  48 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

const lastValues = "5m|15m|30m|1h|3h|6h|12h|24h|2d|7d"

func newSearchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search [<sql>]",
		Short: "Search logs with SQL (POST /api/{org}/_search)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			sql, err := resolveSQL(cmd, args)
			if err != nil {
				return err
			}
			from, _ := cmd.Flags().GetInt("from")
			size, _ := cmd.Flags().GetInt("size")
			if from < 0 || size < 0 {
				return output.NewError(output.CodeConfigInvalid,
					"--from and --size must be >= 0", "")
			}
			now := time.Now()
			startT, endT, err := resolveTimeWindow(cmd, now)
			if err != nil {
				return err
			}

			name, conn, cl, timeout, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			reqBody, err := json.Marshal(map[string]any{
				"query": map[string]any{
					"sql":        sql,
					"start_time": startT.UnixMicro(),
					"end_time":   endT.UnixMicro(),
					"from":       from,
					"size":       size,
				},
				// The effective command timeout doubles as the API's own
				// timeout field (seconds).
				"timeout": int(timeout.Seconds()),
			})
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "POST", "/api/"+url.PathEscape(conn.org())+"/_search", reqBody)
			if err != nil {
				return err
			}

			var parsed struct {
				Hits []json.RawMessage `json:"hits"`
			}
			if err := json.Unmarshal(resp.body, &parsed); err != nil {
				return output.NewError(output.CodeQueryError,
					"response is not valid JSON: "+err.Error(), "")
			}
			cols, rows := hitsToTable(parsed.Hits)
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  cols,
				Rows:     rows,
				JSONData: json.RawMessage(resp.body),
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("sql-file", "", "read the SQL from a file (- reads stdin); mutually exclusive with the sql argument")
	c.Flags().String("start-time", "", "query start time: unix microseconds, RFC3339, or a negative duration like -1h")
	c.Flags().String("end-time", "", "query end time: unix microseconds, RFC3339, a negative duration, or now")
	c.Flags().String("last", "", "relative time window shortcut: "+lastValues+" (mutually exclusive with --start-time/--end-time)")
	c.Flags().Int("from", 0, "offset of the first hit (maps to the API's from field)")
	c.Flags().Int("size", 100, "maximum number of hits (maps to the API's size field)")
	return c
}

// resolveSQL reads the SQL from the positional argument or --sql-file;
// the two are mutually exclusive and one of them is required.
func resolveSQL(cmd *cobra.Command, args []string) (string, error) {
	file := cli.FlagString(cmd, "sql-file")
	switch {
	case len(args) == 1 && file != "":
		return "", output.NewError(output.CodeMissingArgument,
			"sql argument and --sql-file are mutually exclusive", "")
	case file == "-":
		if cli.RuntimeFrom(cmd.Context()).TTY {
			return "", output.NewError(output.CodeMissingArgument,
				"--sql-file - reads the SQL from stdin, but stdin is a terminal",
				"pipe the SQL in, or pass a file path")
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", output.NewError(output.CodeGeneral,
				"cannot read SQL from stdin: "+err.Error(), "")
		}
		return string(raw), nil
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", output.NewError(output.CodeGeneral,
				"cannot read SQL file "+file+": "+err.Error(), "")
		}
		return string(raw), nil
	case len(args) == 1:
		return args[0], nil
	default:
		return "", output.NewError(output.CodeMissingArgument,
			"missing sql (usage: "+cmd.CommandPath()+" <sql> or --sql-file <path>)", "")
	}
}

// resolveTimeWindow computes the query's [start, end) window from --last or
// --start-time/--end-time, defaulting to the last hour.
func resolveTimeWindow(cmd *cobra.Command, now time.Time) (time.Time, time.Time, error) {
	end := now
	startT := now.Add(-time.Hour)
	if last := cli.FlagString(cmd, "last"); last != "" {
		if cmd.Flags().Changed("start-time") || cmd.Flags().Changed("end-time") {
			return time.Time{}, time.Time{}, output.NewError(output.CodeMissingArgument,
				"--last and --start-time/--end-time are mutually exclusive", "")
		}
		d, ok := lastDurations[last]
		if !ok {
			return time.Time{}, time.Time{}, output.NewError(output.CodeConfigInvalid,
				"invalid --last value: "+last, "valid values: "+lastValues)
		}
		return now.Add(-d), end, nil
	}
	if s := cli.FlagString(cmd, "start-time"); s != "" {
		t, err := parseTime(s, now)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		startT = t
	}
	if s := cli.FlagString(cmd, "end-time"); s != "" {
		t, err := parseTime(s, now)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		end = t
	}
	if !startT.Before(end) {
		return time.Time{}, time.Time{}, output.NewError(output.CodeConfigInvalid,
			"start-time must be before end-time", "")
	}
	return startT, end, nil
}

// parseTime parses a --start-time/--end-time value: unix microseconds (all
// digits), RFC3339, a negative Go duration relative to now (e.g. -1h), or
// the literal "now".
func parseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "now") {
		return now, nil
	}
	if isAllDigits(s) {
		us, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return time.UnixMicro(us), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d < 0 {
		return now.Add(d), nil
	}
	return time.Time{}, output.NewError(output.CodeConfigInvalid,
		"invalid time value: "+s,
		"examples: 1719900000000000 (µs), 2026-07-03T12:00:00Z (RFC3339), -1h (relative), now")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hitsToTable builds the dynamic table for text modes: _timestamp pinned as
// the first column, the remaining columns in order of first appearance
// (document order within each hit). _timestamp cells are rendered as local
// human-readable time; nested objects/arrays become compact JSON strings.
// The JSON envelope bypasses this table and carries the raw response.
func hitsToTable(hits []json.RawMessage) ([]string, [][]any) {
	var order []string
	seen := map[string]bool{}
	hasTS := false
	type parsedHit struct {
		keys   []string
		values map[string]any
	}
	parsed := make([]parsedHit, 0, len(hits))
	for _, raw := range hits {
		keys, err := orderedKeys(raw)
		if err != nil {
			continue
		}
		var values map[string]any
		if err := json.Unmarshal(raw, &values); err != nil {
			continue
		}
		for _, k := range keys {
			if k == "_timestamp" {
				hasTS = true
				continue
			}
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
		parsed = append(parsed, parsedHit{keys, values})
	}
	cols := order
	if hasTS {
		cols = append([]string{"_timestamp"}, order...)
	}
	rows := make([][]any, 0, len(parsed))
	for _, h := range parsed {
		row := make([]any, len(cols))
		for j, col := range cols {
			row[j] = cellValue(col, h.values[col])
		}
		rows = append(rows, row)
	}
	return cols, rows
}

// orderedKeys returns the top-level keys of a JSON object in document order
// (encoding/json maps lose it).
func orderedKeys(raw []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected token %v", t)
		}
		keys = append(keys, key)
		var skip any
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// cellValue converts a hit field into a table cell: scalars pass through,
// nested values become compact JSON strings, and numeric _timestamp values
// become local human-readable time with millisecond precision.
func cellValue(col string, v any) any {
	switch val := v.(type) {
	case map[string]any, []any:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	case float64:
		if col == "_timestamp" {
			return time.UnixMicro(int64(val)).Local().Format("2006-01-02 15:04:05.000")
		}
		return val
	default:
		return v
	}
}
