package openobserve

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// validMethod checks the HTTP method enum of the request command.
func validMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}

func newRequestCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "request <method> <path>",
		Short: "Raw request passthrough to the OpenObserve server",
		Long: `Raw request passthrough to the OpenObserve server (curl semantics).

The path is appended to the instance base URL verbatim and must start
with / (it carries the /api/{org}/... prefix itself), e.g.:
  muxcat o2 request GET /api/default/streams
  muxcat o2 request POST /api/default/app/_json --file logs.json

Any completed exchange is reported as {status, headers, body} whatever
the status code; only transport failures become errors. On readonly
connections only GET/HEAD are allowed.

Discover available endpoints from the server's OpenAPI spec first:
  muxcat o2 apidoc ls [--keyword k]   # list endpoints (method, path, summary)
  muxcat o2 apidoc show <path>        # show one endpoint's spec fragment`,
		Args: cli.ExactArgs(2, "<method> <path>", "method", "path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			method := strings.ToUpper(args[0])
			if !validMethod(method) {
				return output.NewError(output.CodeConfigInvalid,
					"invalid method: "+args[0], "valid values: GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS")
			}
			path := args[1]
			if !strings.HasPrefix(path, "/") {
				return output.NewError(output.CodeConfigInvalid,
					"path must start with /: "+path, "example: /api/default/streams")
			}

			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			// The passthrough can reach write/delete endpoints; readonly
			// connections are limited to safe methods.
			if conn.Readonly && method != "GET" && method != "HEAD" {
				return output.NewError(output.CodeReadonlyViolation,
					fmt.Sprintf("method %s is not allowed on a readonly connection", method),
					"use a writable connection (-c), or recreate the connection without --readonly")
			}

			body, err := resolveBody(cmd)
			if err != nil {
				return err
			}
			// Raw passthrough: a completed exchange is reported whatever the
			// status; only transport failures become errors.
			resp, err := cl.exchange(cmd.Context(), method, path, body)
			if err != nil {
				return err
			}

			data := map[string]any{
				"status":  resp.status,
				"headers": resp.headers,
				"body":    decodeResponseBody(resp.headers, resp.body),
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    textResponse(resp),
				JSONData: data,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("file", "", "read the request body from a file (- reads stdin)")
	return c
}

// resolveBody reads the request body from --file; "-" means stdin. No flag
// means no body.
func resolveBody(cmd *cobra.Command) ([]byte, error) {
	file := cli.FlagString(cmd, "file")
	switch {
	case file == "-":
		if cli.RuntimeFrom(cmd.Context()).TTY {
			return nil, output.NewError(output.CodeMissingArgument,
				"--file - reads the request body from stdin, but stdin is a terminal",
				"pipe the body in, or pass a file path")
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, output.NewError(output.CodeGeneral,
				"cannot read request body from stdin: "+err.Error(), "")
		}
		return raw, nil
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, output.NewError(output.CodeGeneral,
				"cannot read body file "+file+": "+err.Error(), "")
		}
		return raw, nil
	default:
		return nil, nil
	}
}

// decodeResponseBody parses the body as JSON when the content type says so,
// otherwise keeps it as a string.
func decodeResponseBody(headers map[string]string, raw []byte) any {
	ct := headers["Content-Type"]
	if strings.Contains(ct, "json") {
		var v any
		if err := json.Unmarshal(raw, &v); err == nil {
			return v
		}
	}
	return string(raw)
}

// textResponse renders the passthrough result for text modes: a status line
// followed by the body (pretty-printed when JSON).
func textResponse(resp *response) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "HTTP %d", resp.status)
	if len(resp.body) > 0 {
		sb.WriteString("\n")
		v := decodeResponseBody(resp.headers, resp.body)
		if s, ok := v.(string); ok {
			sb.WriteString(s)
		} else if b, err := json.MarshalIndent(v, "", "  "); err == nil {
			sb.Write(b)
		}
	}
	return sb.String()
}
