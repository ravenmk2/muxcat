package mongodb

import (
	"context"
	"strings"
	"testing"

	"github.com/ravenmk2/muxcat/internal/output"
)

func TestParseDocsInput(t *testing.T) {
	docs, single, err := parseDocsInput(`{"a":1}`, false)
	if err != nil || !single || len(docs) != 1 {
		t.Fatalf("single doc: docs=%v single=%v err=%v", docs, single, err)
	}

	docs, single, err = parseDocsInput(`[{"a":1},{"_id":{"$oid":"0123456789abcdef01234567"}}]`, false)
	if err != nil || single || len(docs) != 2 {
		t.Fatalf("array: docs=%v single=%v err=%v", docs, single, err)
	}

	docs, single, err = parseDocsInput("{\"a\":1}\n\n{\"b\":2}\n", true)
	if err != nil || single || len(docs) != 2 {
		t.Fatalf("jsonl (blank lines skipped): docs=%v single=%v err=%v", docs, single, err)
	}

	_, _, err = parseDocsInput("{\"a\":1}\nnope\n", true)
	if err == nil || output.ToError(err).Code != output.CodeConfigInvalid ||
		!strings.Contains(err.Error(), "line 2") {
		t.Fatalf("jsonl bad line: err=%v", err)
	}

	if _, _, err := parseDocsInput("  \n ", true); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("jsonl empty: err=%v, want MISSING_ARGUMENT", err)
	}

	if _, _, err := parseDocsInput(`[1,2]`, false); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("array with non-doc elements: err=%v, want CONFIG_INVALID", err)
	}

	if _, _, err := parseDocsInput(`nope`, false); err == nil ||
		output.ToError(err).Code != output.CodeConfigInvalid {
		t.Fatalf("garbage: err=%v, want CONFIG_INVALID", err)
	}
}

func TestParseUpdateDoc(t *testing.T) {
	d, err := parseUpdateDoc(`{"$set":{"a":1}}`)
	if err != nil || len(d) != 1 {
		t.Fatalf("operator doc: d=%v err=%v", d, err)
	}
	// A document without $-keys passes through as replacement semantics.
	d, err = parseUpdateDoc(`{"a":1}`)
	if err != nil || len(d) != 1 {
		t.Fatalf("replacement doc: d=%v err=%v", d, err)
	}
	for _, bad := range []string{`{}`, `nope`, `[1]`} {
		if _, err := parseUpdateDoc(bad); err == nil ||
			output.ToError(err).Code != output.CodeConfigInvalid {
			t.Fatalf("update %q: err=%v, want CONFIG_INVALID", bad, err)
		}
	}
}

// TestWriteReadonlyInterception verifies all four write commands refuse a
// readonly connection before dialing: the connection points at a closed
// port with a short timeout, so any network activity would surface as
// CONNECT_FAILED instead.
func TestWriteReadonlyInterception(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--port", "1", "--timeout", "2s", "--readonly", "--set-default")
	for _, args := range [][]string{
		{"mongodb", "insert", "c", `{"a":1}`},
		{"mongodb", "update", "c", `{}`, `{"$set":{"a":1}}`},
		{"mongodb", "delete", "c", `{"a":1}`},
		{"mongodb", "drop", "c", "--yes"},
	} {
		_, err := runMuxcat(t, args...)
		if e := output.ToError(err); err == nil || e.Code != output.CodeReadonlyViolation {
			t.Fatalf("%v on readonly conn: err=%v, want READONLY_VIOLATION", args, err)
		}
	}

	// Read commands are unaffected: query on the same connection reaches
	// the dial stage and fails as CONNECT_FAILED.
	_, err := runMuxcat(t, "mongodb", "query", "c")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("query on readonly conn: err=%v, want CONNECT_FAILED", err)
	}
}

func TestUpdateArgValidation(t *testing.T) {
	setupEnv(t)
	addConn(t, "w", "--port", "1", "--timeout", "2s", "--set-default")
	_, err := runMuxcat(t, "mongodb", "update", "c", `{}`, `{}`)
	if e := output.ToError(err); err == nil || e.Code != output.CodeConfigInvalid {
		t.Fatalf("empty update doc: err=%v, want CONFIG_INVALID", err)
	}
	// A valid update reaches the dial stage (closed port).
	_, err = runMuxcat(t, "mongodb", "update", "c", `{}`, `{"$set":{"a":1}}`)
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("valid update: err=%v, want CONNECT_FAILED", err)
	}
}

func TestConfirmDestructive(t *testing.T) {
	setupEnv(t)
	// Non-TTY without --yes → MISSING_ARGUMENT.
	cmd := newDeleteCmd()
	cmd.SetContext(context.Background())
	if err := confirmDestructive(cmd, "Drop collection app.c?"); err == nil ||
		output.ToError(err).Code != output.CodeMissingArgument {
		t.Fatalf("non-TTY without --yes: err=%v", err)
	}
	// --yes skips the confirmation.
	cmd = newDeleteCmd()
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("yes", "true"); err != nil {
		t.Fatal(err)
	}
	if err := confirmDestructive(cmd, "Drop collection app.c?"); err != nil {
		t.Fatalf("--yes should skip confirmation: err=%v", err)
	}
}

// TestDeleteWipeConfirmation verifies only delete --many with an empty
// filter (collection wipe) and drop require confirmation; everything else
// proceeds to the dial stage (closed port → CONNECT_FAILED).
func TestDeleteWipeConfirmation(t *testing.T) {
	setupEnv(t)
	addConn(t, "w", "--port", "1", "--timeout", "2s", "--database", "app", "--set-default")

	// Empty filter + --many = wipe → confirmation required.
	_, err := runMuxcat(t, "mongodb", "delete", "c", "--many")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("wipe without --yes: err=%v, want MISSING_ARGUMENT", err)
	}

	// Non-empty filter + --many needs no confirmation.
	_, err = runMuxcat(t, "mongodb", "delete", "c", `{"a":1}`, "--many")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("filtered --many: err=%v, want CONNECT_FAILED", err)
	}

	// --yes skips the wipe confirmation.
	_, err = runMuxcat(t, "mongodb", "delete", "c", "--many", "--yes")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("wipe with --yes: err=%v, want CONNECT_FAILED", err)
	}

	// drop always confirms.
	_, err = runMuxcat(t, "mongodb", "drop", "c")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("drop without --yes: err=%v, want MISSING_ARGUMENT", err)
	}
	_, err = runMuxcat(t, "mongodb", "drop", "c", "--yes")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("drop with --yes: err=%v, want CONNECT_FAILED", err)
	}

	// Ordinary inserts need no confirmation.
	_, err = runMuxcat(t, "mongodb", "insert", "c", `{"a":1}`)
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("insert: err=%v, want CONNECT_FAILED", err)
	}
}
