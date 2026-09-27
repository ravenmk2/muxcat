package jenkins

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// defaultLogTail is the number of log lines kept when --full is not passed
// and the global --limit flag was not set explicitly.
const defaultLogTail = 200

func newBuildCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "build",
		Short: "Inspect and control Jenkins builds",
		Long: `Inspect the builds of a job: list build history, show one
build's details (parameters, causes, artifacts, changelog), read its
console log (tail by default, --follow streams a running build), or
abort a running build with stop. The build is addressed by number or
by alias: last, lastSuccessful, lastFailed, lastCompleted.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newBuildLsCmd(), newBuildShowCmd(), newBuildLogCmd(), newBuildStopCmd())
	return c
}

func newBuildLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls <job>",
		Short: "List a job's build history (GET /job/.../api/json?tree=builds[...])",
		Args:  cli.ExactArgs(1, "<job>", "job"),
		Example: `  muxcat jenkins build ls my-job
  muxcat jenkins build ls my-job --limit 20
  muxcat jenkins build ls team/backend/deploy --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET",
				jobPath(args[0])+"/api/json?tree=builds[number,result,timestamp,duration,url,building]", nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			var rows [][]any
			if m, ok := raw.(map[string]any); ok {
				if builds, ok := m["builds"].([]any); ok {
					for _, item := range builds {
						b, ok := item.(map[string]any)
						if !ok {
							continue
						}
						result := str(b["result"])
						if building, _ := b["building"].(bool); building {
							result = "BUILDING"
						}
						rows = append(rows, []any{
							b["number"], result, formatTS(b["timestamp"]),
							formatDuration(b["duration"]), str(b["url"]),
						})
					}
				}
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"number", "result", "timestamp", "duration", "url"},
				Rows:     rows,
				JSONData: raw,
			}, meta(name, start, truncated))
		},
	}
}

func newBuildShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <job> <n|alias>",
		Short: "Show one build's details (GET /job/.../{n}/api/json)",
		Args:  cli.ExactArgs(2, "<job> <n|alias>", "job", "n|alias"),
		Example: `  muxcat jenkins build show my-job 42
  muxcat jenkins build show my-job last
  muxcat jenkins build show team/backend/deploy lastFailed --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			ref, err := buildRef(args[1])
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET",
				jobPath(args[0])+"/"+ref+"/api/json?tree="+buildShowTree, nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			m, _ := raw.(map[string]any)
			result := str(m["result"])
			if building, _ := m["building"].(bool); building {
				result = "BUILDING"
			}
			params, causes := buildActions(m["actions"])
			value := map[string]any{
				"job":          args[0],
				"number":       m["number"],
				"display_name": str(m["displayName"]),
				"description":  str(m["description"]),
				"result":       result,
				"timestamp":    formatTS(m["timestamp"]),
				"duration":     formatDuration(m["duration"]),
				"url":          str(m["url"]),
				"parameters":   params,
				"causes":       causes,
				"artifacts":    artifacts(m["artifacts"]),
				"changes":      changeSet(m["changeSet"]),
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    textValue(value),
				JSONData: value,
				Syntax:   "yaml",
			}, meta(name, start, false))
		},
	}
}

// buildShowTree fetches the build detail; parameters carry their _class so
// password-typed values can be masked.
const buildShowTree = "number,displayName,description,result,building,timestamp,duration,url," +
	"actions[parameters[name,value,_class],causes[shortDescription]]," +
	"artifacts[fileName,relativePath,size],changeSet[items[msg,author[fullName]]]"

// sensitiveParamRe matches build parameter names that typically carry
// credentials; their values are masked as *** in output.
var sensitiveParamRe = regexp.MustCompile(`(?i)password|secret|token|key`)

// buildActions extracts build parameters and causes from the actions list.
// Parameter values that look like credentials (by name or by parameter
// class) are masked as ***; an empty value stays empty so it remains
// possible to tell whether it was set.
func buildActions(v any) ([]map[string]any, []string) {
	actions, _ := v.([]any)
	var params []map[string]any
	var causes []string
	for _, item := range actions {
		a, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if ps, ok := a["parameters"].([]any); ok {
			for _, p := range ps {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				pname := str(pm["name"])
				val := valueString(pm["value"])
				if val != "" && (sensitiveParamRe.MatchString(pname) ||
					strings.Contains(strings.ToLower(str(pm["_class"])), "password")) {
					val = "***"
				}
				params = append(params, map[string]any{"name": pname, "value": val})
			}
		}
		if cs, ok := a["causes"].([]any); ok {
			for _, c := range cs {
				if cm, ok := c.(map[string]any); ok {
					causes = append(causes, str(cm["shortDescription"]))
				}
			}
		}
	}
	return params, causes
}

// artifacts flattens the artifact list for display.
func artifacts(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, map[string]any{
				"file": str(m["fileName"]),
				"path": str(m["relativePath"]),
				"size": m["size"],
			})
		}
	}
	return out
}

// changeSet summarizes the changelog as "author: message" entries.
func changeSet(v any) []string {
	cs, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	items, _ := cs["items"].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		author := ""
		if a, ok := m["author"].(map[string]any); ok {
			author = str(a["fullName"])
		}
		msg := str(m["msg"])
		if author != "" {
			out = append(out, author+": "+msg)
		} else {
			out = append(out, msg)
		}
	}
	return out
}

func newBuildLogCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "log <job> <n|alias>",
		Short: "Read a build's console log (tail by default; --full for all; --follow streams)",
		Long: `Read a build's console log via the progressiveText endpoint.
By default only the last 200 lines are shown (the global --limit flag
overrides the count; --limit 0 or --full prints everything). --follow
keeps polling the start-offset form of the endpoint and streams new
chunks as the build produces them, until the build's log is complete
(--wait-timeout bounds the follow). Output model: text modes stream
chunks to stdout as they arrive; --json cannot stream, so it collects
everything and emits a single envelope with the complete log at the
end.`,
		Args: cli.ExactArgs(2, "<job> <n|alias>", "job", "n|alias"),
		Example: `  muxcat jenkins build log my-job last
  muxcat jenkins build log my-job 42 --limit 500
  muxcat jenkins build log my-job last --full
  muxcat jenkins build log my-job last --follow
  muxcat jenkins build log my-job last --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			ref, err := buildRef(args[1])
			if err != nil {
				return err
			}
			path := jobPath(args[0])
			text, size, more, err := fetchLog(cmd.Context(), cl, path, ref, 0)
			if err != nil {
				return err
			}

			tail := defaultLogTail
			if cmd.Flags().Changed("limit") {
				tail = cli.FlagLimit(cmd)
			}
			full := cli.FlagBool(cmd, "full") || tail <= 0
			out, shown, truncated := text, 0, false
			if !full {
				out, shown = tailLines(text, tail)
				truncated = shown < lineCount(text)
			}
			if truncated {
				out = fmt.Sprintf("... (showing last %d lines, --full for all)\n%s", shown, out)
			}
			if !cli.FlagBool(cmd, "follow") {
				return cli.RenderResult(cmd, &output.Result{
					Value: out,
					JSONData: map[string]any{
						"job":       args[0],
						"build":     args[1],
						"size":      size,
						"truncated": truncated,
						"log":       out,
					},
				}, meta(name, start, truncated))
			}

			// --follow: text modes stream increments to stdout as they
			// arrive; --json cannot stream, so the increments are
			// collected and a single envelope with the complete log is
			// emitted at the end.
			wt, err := waitTimeout(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), wt)
			defer cancel()
			jsonMode := cli.FlagBool(cmd, "json") || strings.EqualFold(cli.FlagString(cmd, "output"), "json")
			w := cmd.OutOrStdout()
			if !jsonMode {
				_, _ = fmt.Fprintln(w, out)
			}
			var whole strings.Builder
			whole.WriteString(text)
			offset := size
			for more {
				if err := sleepCtx(ctx, logPollInterval); err != nil {
					return waitCtxErr(ctx)
				}
				chunk, newSize, m, err := fetchLog(ctx, cl, path, ref, offset)
				if werr := waitCtxErr(ctx); werr != nil {
					return werr
				}
				if err != nil {
					return err
				}
				if newSize > offset {
					offset = newSize
				}
				whole.WriteString(chunk)
				if !jsonMode && chunk != "" {
					_, _ = fmt.Fprint(w, chunk)
				}
				more = m
			}
			if !jsonMode {
				return nil
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: whole.String(),
				JSONData: map[string]any{
					"job":       args[0],
					"build":     args[1],
					"size":      offset,
					"truncated": false,
					"log":       whole.String(),
				},
			}, meta(name, start, false))
		},
	}
	c.Flags().Bool("full", false, "print the whole log instead of the tail")
	c.Flags().Bool("follow", false, "stream new log chunks until the build's log is complete")
	addWaitTimeoutFlag(c)
	return c
}

// fetchLog reads the console log through logText/progressiveText starting
// at byte offset start. size is the total log size so far (X-Text-Size)
// and more reports whether the build is still producing output
// (X-More-Data) — the pair a --follow mode would poll with start=size.
func fetchLog(ctx context.Context, cl *client, path, ref string, startOffset int64) (text string, size int64, more bool, err error) {
	resp, err := cl.do(ctx, "GET",
		path+"/"+ref+"/logText/progressiveText?start="+strconv.FormatInt(startOffset, 10), nil)
	if err != nil {
		return "", 0, false, err
	}
	if s := resp.headers["X-Text-Size"]; s != "" {
		size, _ = strconv.ParseInt(s, 10, 64)
	}
	more = resp.headers["X-More-Data"] == "true"
	return string(resp.body), size, more, nil
}

// tailLines keeps the last n lines of s.
func tailLines(s string, n int) (string, int) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s, len(lines)
	}
	return strings.Join(lines[len(lines)-n:], "\n"), n
}

// lineCount counts the lines of s.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

func newBuildStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <job> <n|alias>",
		Short: "Abort a running build (POST /job/.../{n}/stop)",
		Args:  cli.ExactArgs(2, "<job> <n|alias>", "job", "n|alias"),
		Example: `  muxcat jenkins build stop my-job 42
  muxcat jenkins build stop my-job last`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if conn.Readonly {
				return readonlyErr("build stop")
			}
			ref, err := buildRef(args[1])
			if err != nil {
				return err
			}
			// Jenkins answers with a 302 to the build page; the http client
			// follows it, so any non-2xx reaching here is a real failure.
			if _, err := cl.do(cmd.Context(), "POST", jobPath(args[0])+"/"+ref+"/stop", nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"job": args[0], "number": args[1], "stopped": true},
				Message: fmt.Sprintf("stop requested for %s #%s", args[0], args[1]),
			}, meta(name, start, false))
		},
	}
}
