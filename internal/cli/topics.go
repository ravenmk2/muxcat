package cli

import "github.com/spf13/cobra"

// Help topics document the global contracts every command honors. They are
// cobra additional help topics: not runnable, listed under "Additional help
// topics:" in root help, and read via `muxcat help <topic>`.
func newHelpTopics() []*cobra.Command {
	return []*cobra.Command{
		{
			Use:   "output",
			Short: "Output contract: envelope, output modes, meta fields",
			Long: `Every muxcat command prints its result in one of four output modes,
selected with --output (auto|table|plain|tsv|json) or --json (shortcut
for --output json). auto renders table on a TTY and degrades to plain
off-TTY. Colors are TTY-only; piped output is always clean.

With --json every command emits the same envelope on stdout:

  {
    "ok": true,
    "data": { ... },
    "meta": { "connector": "...", "connection": "...", "elapsed_ms": 0,
              "truncated": false },
    "error": { "code": "...", "message": "...", "hint": "..." }
  }

ok is true on success (error omitted) and false on failure (data
omitted). data shapes: a single value {"value": ...}, a table
{"columns": [...], "rows": [[...]]}, or a connector-specific object.
meta.truncated is true when output was cut by --limit (row count) or a
command's --max-bytes (value size). Connectors may add meta fields
(e.g. redis reports db, and cursor in scan's single-round mode).

In plain/tsv modes single values print bare (no labels), tables print
as columns; JSON envelope shape is unaffected.

Degraded results: a command may produce partial data and still fail
(e.g. etcd endpoint status when some endpoints are down). Text modes
print the partial result on stdout first, then the Error/Hint lines go
to stderr with a non-zero exit code; with --json the envelope carries
"ok": false and the partial result in data alongside the error.`,
		},
		{
			Use:   "errors",
			Short: "Error contract: error shape, error codes, exit codes",
			Long: `On failure, non-JSON output prints to stderr:

  Error: <message>
  Hint: <how to fix>        (when available)

With --json the failure arrives as the envelope with "ok": false and
error {code, message, hint} (see: muxcat help output).

Exit codes:

  0  success
  1  general error (KEY_UNAVAILABLE, unclassified failures)
  2  usage: MISSING_ARGUMENT, CONN_NOT_FOUND, CONFIG_INVALID,
     UNSUPPORTED_OPERATION, CONNECTOR_UNKNOWN
  3  connection: CONNECT_FAILED, TIMEOUT
  4  authentication: AUTH_FAILED
  5  execution: QUERY_ERROR, READONLY_VIOLATION, UPGRADE_FAILED

Scripts should branch on the exit code class and read error.code for
the specific cause; messages are for humans, hints suggest the fix.`,
		},
	}
}
