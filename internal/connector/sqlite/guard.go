package sqlite

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/ravenmk2/muxcat/internal/output"
)

// readonlyVerbs is the whitelist of leading keywords permitted on readonly
// connections, matching query.go's queryVerbs (VALUES and TABLE are
// genuinely read-only statements). Writable PRAGMA / WITH variants pass
// through: mode=ro on the SQLite side remains the hard constraint.
var readonlyVerbs = map[string]bool{
	"SELECT": true, "PRAGMA": true, "EXPLAIN": true,
	"WITH": true, "VALUES": true, "TABLE": true,
}

// guardQuery applies the connector-side readonly interception before
// opening the database file. It is an anti-footgun measure, not a security
// boundary: mode=ro in the DSN is the hard constraint.
func guardQuery(conn Connection, sqlText string) error {
	if !conn.Readonly {
		return nil
	}
	if kw := firstKeyword(sqlText); !readonlyVerbs[kw] {
		return output.NewError(output.CodeReadonlyViolation,
			fmt.Sprintf("statement %q is not allowed on a readonly connection", kw),
			"use a writable connection (-c), or recreate the connection without --readonly")
	}
	return nil
}

// firstKeyword returns the uppercased first SQL keyword after skipping
// leading whitespace, parentheses, and -- / /* */ comments.
func firstKeyword(sqlText string) string {
	s := sqlText
	for {
		s = strings.TrimLeft(s, " \t\r\n(")
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return ""
			}
			s = s[i+2:]
		default:
			end := strings.IndexFunc(s, func(r rune) bool {
				return !unicode.IsLetter(r) && r != '_'
			})
			if end < 0 {
				return strings.ToUpper(s)
			}
			return strings.ToUpper(s[:end])
		}
	}
}
