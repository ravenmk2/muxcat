package etcd

import (
	"fmt"

	"github.com/ravenmk2/muxcat/internal/output"
)

// guardWrite rejects write operations (put/del and any future write) on a
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

// guardDangerous rejects dangerous operations unless the connection sets
// allowDangerous.
func guardDangerous(conn Connection, op string) error {
	if !conn.AllowDangerous {
		return output.NewError(output.CodeUnsupportedOperation,
			fmt.Sprintf("%s is blocked by default (dangerous operation)", op),
			"recreate the connection with --allow-dangerous to enable it")
	}
	return nil
}
