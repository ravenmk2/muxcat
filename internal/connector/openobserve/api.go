package openobserve

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// apiMethods is the set of HTTP methods carried by an OpenAPI path item.
var apiMethods = []string{"get", "post", "put", "patch", "delete", "head", "options"}

func newApiCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "api",
		Short: "Explore the server's OpenAPI spec (/api-doc/openapi.json)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newApiLsCmd(), newApiShowCmd())
	return c
}

// fetchSpec downloads and parses the server's OpenAPI spec, returning the
// resolved connection name along with it. The endpoint lives at the server
// root (not under /api/{org}); servers without it get a clear error
// pointing at the official docs.
func fetchSpec(cmd *cobra.Command) (string, map[string]any, error) {
	name, _, cl, _, err := openForCmd(cmd)
	if err != nil {
		return "", nil, err
	}
	resp, err := cl.do(cmd.Context(), "GET", "/api-doc/openapi.json", nil)
	if err != nil {
		if resp != nil && resp.status == 404 {
			return "", nil, output.NewError(output.CodeQueryError,
				"this server does not expose an OpenAPI spec at /api-doc/openapi.json",
				"the spec endpoint exists on recent OpenObserve versions; explore endpoints in the official docs: https://openobserve.ai/docs/reference/api/")
		}
		return "", nil, err
	}
	var spec map[string]any
	if err := json.Unmarshal(resp.body, &spec); err != nil {
		return "", nil, output.NewError(output.CodeQueryError,
			"OpenAPI spec is not valid JSON: "+err.Error(), "")
	}
	return name, spec, nil
}

// specPaths returns the spec's paths object.
func specPaths(spec map[string]any) map[string]any {
	paths, _ := spec["paths"].(map[string]any)
	return paths
}

// apiEntry is one method+path row of the api ls table.
type apiEntry struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

// listEntries flattens the spec's paths into method+path+summary rows,
// optionally filtered by a case-insensitive keyword on path or summary.
func listEntries(spec map[string]any, keyword string) []apiEntry {
	kw := strings.ToLower(keyword)
	var entries []apiEntry
	for path, item := range specPaths(spec) {
		ops, _ := item.(map[string]any)
		for _, m := range apiMethods {
			op, ok := ops[m].(map[string]any)
			if !ok {
				continue
			}
			summary, _ := op["summary"].(string)
			if kw != "" && !strings.Contains(strings.ToLower(path), kw) &&
				!strings.Contains(strings.ToLower(summary), kw) {
				continue
			}
			entries = append(entries, apiEntry{Method: strings.ToUpper(m), Path: path, Summary: summary})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].Method < entries[j].Method
	})
	return entries
}

func newApiLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List endpoints of the server's OpenAPI spec",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, spec, err := fetchSpec(cmd)
			if err != nil {
				return err
			}
			entries := listEntries(spec, cli.FlagString(cmd, "keyword"))
			rows := make([][]any, 0, len(entries))
			for _, e := range entries {
				rows = append(rows, []any{e.Method, e.Path, e.Summary})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"method", "path", "summary"},
				Rows:     rows,
				JSONData: entriesOrEmpty(entries),
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("keyword", "", "filter endpoints by keyword (matches path or summary, case-insensitive)")
	return c
}

// entriesOrEmpty keeps JSON output as [] instead of null when empty.
func entriesOrEmpty(entries []apiEntry) []apiEntry {
	if entries == nil {
		return []apiEntry{}
	}
	return entries
}

func newApiShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <path>",
		Short: "Show the OpenAPI spec fragment of one endpoint",
		Args:  cli.ExactArgs(1, "<path>", "path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, spec, err := fetchSpec(cmd)
			if err != nil {
				return err
			}
			key, item, err := findPath(specPaths(spec), args[0])
			if err != nil {
				return err
			}
			fragment := map[string]any{key: item}
			pretty, err := json.MarshalIndent(fragment, "", "  ")
			if err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    string(pretty),
				JSONData: fragment,
			}, meta(name, start, false))
		},
	}
}

// findPath locates a path item in the spec: exact key first, then a
// segment-wise match where any {param} segment of the spec path matches a
// concrete segment (so /api/default/_search finds /api/{org_id}/_search).
func findPath(paths map[string]any, want string) (string, any, error) {
	if item, ok := paths[want]; ok {
		return want, item, nil
	}
	var matches []string
	for p := range paths {
		if pathSegmentsMatch(p, want) {
			matches = append(matches, p)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", nil, output.NewError(output.CodeQueryError,
			"endpoint not found in the OpenAPI spec: "+want,
			"list available endpoints with o2 api ls (optionally --keyword)")
	case 1:
		return matches[0], paths[matches[0]], nil
	default:
		return "", nil, output.NewError(output.CodeQueryError,
			fmt.Sprintf("endpoint %q is ambiguous, matches: %s", want, strings.Join(matches, ", ")),
			"give the exact spec path")
	}
}

// pathSegmentsMatch compares two paths segment by segment; a {param}
// segment in the spec path matches any concrete segment.
func pathSegmentsMatch(specPath, want string) bool {
	ss := strings.Split(strings.Trim(specPath, "/"), "/")
	ws := strings.Split(strings.Trim(want, "/"), "/")
	if len(ss) != len(ws) {
		return false
	}
	for i := range ss {
		if strings.HasPrefix(ss[i], "{") && strings.HasSuffix(ss[i], "}") {
			continue
		}
		if ss[i] != ws[i] {
			return false
		}
	}
	return true
}
