package jenkins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// jobLsTree limits the job listing response to the fields the table needs;
// Jenkins answers api/json with every detail otherwise.
const jobLsTree = "jobs[name,fullName,url,color,buildable,lastBuild[number,result,timestamp]]"

// maxFolderDepth bounds folder recursion in job ls so pathological nesting
// (or a cyclic folder structure) cannot spin forever.
const maxFolderDepth = 10

func newJobCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "job",
		Short: "Inspect Jenkins jobs",
		Long: `Inspect Jenkins jobs: list jobs of the root or a folder
(folders are recursed into), or show one job's details and build
parameter definitions. Jobs inside folders are addressed by full
name (folder/sub/job).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newJobLsCmd(), newJobShowCmd())
	return c
}

func newJobLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List jobs, recursing into folders (GET /api/json)",
		Args:  cobra.NoArgs,
		Example: `  muxcat jenkins job ls
  muxcat jenkins job ls --folder team/backend
  muxcat jenkins job ls --class workflow-job
  muxcat jenkins job ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			entry := ""
			if folder := cli.FlagString(cmd, "folder"); folder != "" {
				entry = jobPath(folder)
			}
			raw, rows, err := listJobs(cmd.Context(), cl, entry, 0)
			if err != nil {
				return err
			}
			if class := cli.FlagString(cmd, "class"); class != "" {
				rows = filterByClass(rows, class)
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"full_name", "class", "status", "last_build"},
				Rows:     rows,
				JSONData: raw,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("folder", "", "start the listing from this folder (full name, e.g. team/backend)")
	c.Flags().String("class", "", "show only jobs of this class (as shown in the class column, e.g. workflow-job, freestyle, folder)")
	return c
}

// filterByClass keeps only rows whose class cell matches, case-insensitively.
// It filters display rows only: folder recursion in listJobs is unaffected,
// so jobs inside filtered-out folders still show up.
func filterByClass(rows [][]any, class string) [][]any {
	out := make([][]any, 0, len(rows))
	for _, r := range rows {
		if len(r) > 1 {
			if s, ok := r[1].(string); ok && strings.EqualFold(s, class) {
				out = append(out, r)
			}
		}
	}
	return out
}

// listJobs fetches the jobs at prefix ("" for the root, otherwise a
// jobPath form) and recurses into folders. It returns the raw root
// response for --json passthrough plus the flattened table rows.
func listJobs(ctx context.Context, cl *client, prefix string, depth int) (any, [][]any, error) {
	resp, err := cl.do(ctx, "GET", prefix+"/api/json?tree="+jobLsTree, nil)
	if err != nil {
		return nil, nil, err
	}
	raw, err := decodeBody(resp.body)
	if err != nil {
		return nil, nil, err
	}
	var rows [][]any
	m, _ := raw.(map[string]any)
	jobs, _ := m["jobs"].([]any)
	for _, item := range jobs {
		j, ok := item.(map[string]any)
		if !ok {
			continue
		}
		class, _ := j["_class"].(string)
		fullName, _ := j["fullName"].(string)
		rows = append(rows, []any{fullName, simpleClass(class), colorStatus(str(j["color"])), lastBuild(j["lastBuild"])})
		// Folders (and multibranch projects, which nest branch jobs) are
		// recursed into by full name.
		if depth < maxFolderDepth && isFolder(class) {
			_, sub, err := listJobs(ctx, cl, jobPath(fullName), depth+1)
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, sub...)
		}
	}
	return raw, rows, nil
}

// isFolder reports whether a job _class nests other jobs.
func isFolder(class string) bool {
	low := strings.ToLower(class)
	return strings.Contains(low, "folder") || strings.Contains(low, "multibranch")
}

// simpleClass shortens Jenkins' Java class names to a readable form.
func simpleClass(class string) string {
	low := strings.ToLower(class)
	switch {
	case strings.Contains(low, "multibranch"):
		return "multibranch"
	case strings.Contains(low, "folder"):
		return "folder"
	case strings.Contains(low, "workflowjob"):
		return "workflow-job"
	case strings.Contains(low, "freestyleproject"):
		return "freestyle"
	case strings.Contains(low, "matrixproject"):
		return "matrix"
	}
	if i := strings.LastIndex(class, "."); i >= 0 {
		return class[i+1:]
	}
	return class
}

// colorStatus maps a job color ball to a readable status. The _anime
// suffix marks a running build.
func colorStatus(color string) string {
	if strings.HasSuffix(color, "_anime") {
		return "building"
	}
	switch color {
	case "blue":
		return "success"
	case "red":
		return "failed"
	case "yellow":
		return "unstable"
	case "aborted", "disabled":
		return color
	case "notbuilt":
		return "not_built"
	}
	return color
}

// lastBuild renders the lastBuild object as "#N RESULT".
func lastBuild(v any) string {
	b, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	num := numStr(b["number"])
	if num == "" {
		return ""
	}
	res := str(b["result"])
	if res == "" {
		res = "BUILDING"
	}
	return "#" + num + " " + res
}

func newJobShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <job>",
		Short: "Show a job's details and parameter definitions (GET /job/.../api/json)",
		Args:  cli.ExactArgs(1, "<job>", "job"),
		Example: `  muxcat jenkins job show my-job
  muxcat jenkins job show team/backend/deploy --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			job := args[0]
			resp, err := cl.do(cmd.Context(), "GET", jobPath(job)+"/api/json?tree="+jobShowTree, nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			m, _ := raw.(map[string]any)
			value := map[string]any{
				"name":                  str(m["displayName"]),
				"full_name":             str(m["fullName"]),
				"description":           str(m["description"]),
				"url":                   str(m["url"]),
				"buildable":             m["buildable"],
				"status":                colorStatus(str(m["color"])),
				"concurrent_build":      m["concurrentBuild"],
				"health":                healthReport(m["healthReport"]),
				"last_build":            lastBuild(m["lastBuild"]),
				"last_successful_build": buildNumber(m["lastSuccessfulBuild"]),
				"last_failed_build":     buildNumber(m["lastFailedBuild"]),
				"parameters":            paramDefs(m["property"]),
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    textValue(value),
				JSONData: value,
				Syntax:   "yaml",
			}, meta(name, start, false))
		},
	}
}

// jobShowTree fetches the job detail plus its parameter definitions.
const jobShowTree = "displayName,fullName,description,url,buildable,color,concurrentBuild," +
	"healthReport[description,score],lastBuild[number,result,timestamp]," +
	"lastSuccessfulBuild[number],lastFailedBuild[number]," +
	"property[parameterDefinitions[name,type,defaultParameterValue[value],description]]"

// healthReport renders the health report as "description (score%)" entries.
func healthReport(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s (%v%%)", str(m["description"]), m["score"]))
	}
	return out
}

// buildNumber extracts the number of a build reference object.
func buildNumber(v any) any {
	if b, ok := v.(map[string]any); ok {
		if n, ok := b["number"]; ok {
			return n
		}
	}
	return nil
}

// paramDefs flattens the job property list into parameter definitions.
// Password-type default values are credentials and masked as *** (an
// empty value stays empty so it remains possible to tell whether a
// default is set).
func paramDefs(v any) []map[string]any {
	props, _ := v.([]any)
	var out []map[string]any
	for _, item := range props {
		p, ok := item.(map[string]any)
		if !ok {
			continue
		}
		defs, _ := p["parameterDefinitions"].([]any)
		for _, d := range defs {
			def, ok := d.(map[string]any)
			if !ok {
				continue
			}
			ptype := str(def["type"])
			dv := ""
			if dpv, ok := def["defaultParameterValue"].(map[string]any); ok {
				dv = valueString(dpv["value"])
			}
			if dv != "" && strings.Contains(strings.ToLower(ptype), "password") {
				dv = "***"
			}
			out = append(out, map[string]any{
				"name":        str(def["name"]),
				"type":        ptype,
				"default":     dv,
				"description": str(def["description"]),
			})
		}
	}
	return out
}

// str reads a decoded JSON string field.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// numStr reads a decoded JSON number field (float64) as an integer string.
func numStr(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%d", int64(f))
	}
	return ""
}

// valueString renders a parameter value (string, bool, number) as text.
func valueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// textValue flattens nested structures of a Value map into compact JSON
// strings so text modes stay readable (fmt's %v on maps is not).
func textValue(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch v.(type) {
		case map[string]any, []any, []map[string]any, []string:
			if b, err := json.Marshal(v); err == nil {
				out[k] = string(b)
			} else {
				out[k] = fmt.Sprint(v)
			}
		default:
			out[k] = v
		}
	}
	return out
}

// formatTS renders a Jenkins millisecond epoch as local readable time.
func formatTS(v any) string {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return ""
	}
	return time.UnixMilli(int64(f)).Local().Format("2006-01-02 15:04:05")
}

// formatDuration humanizes a Jenkins millisecond duration.
func formatDuration(v any) string {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return ""
	}
	return (time.Duration(int64(f)) * time.Millisecond).String()
}
