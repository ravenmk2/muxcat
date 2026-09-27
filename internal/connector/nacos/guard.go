package nacos

import (
	"fmt"

	"github.com/ravenmk2/muxcat/internal/output"
)

// guardWrite rejects write operations (config publish/delete) on a
// readonly connection. It is an anti-footgun measure, not a security
// boundary; hard constraints need server-side auth/permissions.
func guardWrite(conn Connection, op string) error {
	if conn.Readonly {
		return output.NewError(output.CodeReadonlyViolation,
			fmt.Sprintf("%s is not allowed on a readonly connection", op),
			"use a writable connection (-c), or recreate the connection without --readonly")
	}
	return nil
}
