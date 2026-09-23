//go:build cgo

package sqlite

import _ "github.com/mattn/go-sqlite3"

// driverName is the database/sql driver name under CGO builds.
const driverName = "sqlite3"
