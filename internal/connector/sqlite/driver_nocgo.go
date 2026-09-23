//go:build !cgo

package sqlite

import _ "modernc.org/sqlite"

// driverName is the database/sql driver name under pure-Go builds.
const driverName = "sqlite"
