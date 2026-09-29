package mongodb

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// maxDocColumns caps the document columns in the table rendering; wider
// documents should be read via --json.
const maxDocColumns = 15

// renderDocs renders find/aggregate results: a table of _id plus the
// union of top-level fields for text modes, relaxed Extended JSON
// documents for --json.
func renderDocs(cmd *cobra.Command, connName string, docs []bson.D, truncated bool, start time.Time) error {
	columns, rows, extra := docsTable(docs)
	jsonDocs := make([]any, 0, len(docs))
	for _, d := range docs {
		v, err := relaxedJSON(d)
		if err != nil {
			return output.NewError(output.CodeGeneral, "failed to encode a result document: "+err.Error(), "")
		}
		jsonDocs = append(jsonDocs, v)
	}
	message := docsMessage(len(docs), truncated, extra)
	return cli.RenderResult(cmd, &output.Result{
		Columns:  columns,
		Rows:     rows,
		Message:  message,
		JSONData: jsonDocs,
	}, meta(connName, start, truncated))
}

// docsMessage builds the result message: "N documents", with truncation
// and column-cap notes.
func docsMessage(n int, truncated bool, extra int) string {
	msg := fmt.Sprintf("%d documents", n)
	if truncated {
		msg += " (truncated)"
	}
	if extra > 0 {
		msg += fmt.Sprintf(", +%d more fields, use --json", extra)
	}
	return msg
}

// docsTable flattens documents into table columns and rows: _id first
// when present in every document, then the union of the remaining
// top-level fields in first-appearance order (capped at maxDocColumns;
// extra reports how many fields were dropped). Scalar values render
// directly, composites as compact Extended JSON strings, missing fields
// as empty cells.
func docsTable(docs []bson.D) (columns []string, rows [][]any, extra int) {
	hasID := len(docs) > 0
	seen := map[string]bool{}
	fields := []string{}
	for _, d := range docs {
		docHasID := false
		for _, e := range d {
			if e.Key == "_id" {
				docHasID = true
				continue
			}
			if !seen[e.Key] {
				seen[e.Key] = true
				fields = append(fields, e.Key)
			}
		}
		if !docHasID {
			hasID = false
		}
	}
	extra = 0
	if len(fields) > maxDocColumns {
		extra = len(fields) - maxDocColumns
		fields = fields[:maxDocColumns]
	}
	columns = fields
	if hasID {
		columns = append([]string{"_id"}, fields...)
	}

	rows = make([][]any, 0, len(docs))
	for _, d := range docs {
		vals := map[string]any{}
		for _, e := range d {
			vals[e.Key] = e.Value
		}
		row := make([]any, 0, len(columns))
		if hasID {
			row = append(row, docCell(vals["_id"]))
		}
		for _, f := range fields {
			if v, ok := vals[f]; ok {
				row = append(row, docCell(v))
			} else {
				row = append(row, "")
			}
		}
		rows = append(rows, row)
	}
	return columns, rows, extra
}

// docCell renders a document value for a table cell: scalars via
// cellString, everything else as a compact Extended JSON string.
func docCell(v any) any {
	if s, ok := cellString(v); ok {
		return s
	}
	if s, err := compactExtJSON(v, false); err == nil {
		return s
	}
	return fmt.Sprint(v)
}

// cellString renders a directly-renderable scalar; the bool result
// reports whether the value is one.
func cellString(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "null", true
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case int32:
		return strconv.FormatInt(int64(t), 10), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), true
	case bson.DateTime:
		return t.Time().UTC().Format(time.RFC3339), true
	case bson.ObjectID:
		return t.Hex(), true
	default:
		return "", false
	}
}

// relaxedJSON round-trips a BSON value through relaxed Extended JSON so
// the envelope data is guaranteed to be valid encoding/json.
func relaxedJSON(v any) (any, error) {
	s, err := compactExtJSON(v, true)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// compactExtJSON encodes any BSON value (documents, arrays, scalars) as a
// compact relaxed Extended JSON string. MarshalExtJSON only accepts
// top-level documents, so non-documents go through a wrapper document.
func compactExtJSON(v any, escapeHTML bool) (string, error) {
	if _, ok := v.(bson.D); ok {
		raw, err := bson.MarshalExtJSON(v, false, escapeHTML)
		return string(raw), err
	}
	raw, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, escapeHTML)
	if err != nil {
		return "", err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	return string(m["v"]), nil
}

// docLookup returns the value of a top-level document key.
func docLookup(d bson.D, key string) any {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}
