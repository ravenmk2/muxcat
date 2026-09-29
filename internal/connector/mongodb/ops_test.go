package mongodb

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ravenmk2/muxcat/internal/output"
)

func TestParseIndexKeys(t *testing.T) {
	d, err := parseIndexKeys(`{"email":1}`)
	if err != nil || len(d) != 1 {
		t.Fatalf("valid keys: d=%v err=%v", d, err)
	}
	d, err = parseIndexKeys(`{"loc":"2dsphere"}`)
	if err != nil || len(d) != 1 {
		t.Fatalf("2dsphere keys: d=%v err=%v", d, err)
	}
	for _, bad := range []string{`{}`, `nope`, `[1]`} {
		if _, err := parseIndexKeys(bad); err == nil ||
			output.ToError(err).Code != output.CodeConfigInvalid {
			t.Fatalf("keys %q: err=%v, want CONFIG_INVALID", bad, err)
		}
	}
}

func TestIndexesCreateValidation(t *testing.T) {
	setupEnv(t)
	addConn(t, "w", "--port", "1", "--timeout", "2s", "--set-default")
	for _, extra := range [][]string{
		{"mongodb", "indexes", "create", "c", `{}`},
		{"mongodb", "indexes", "create", "c", `nope`},
		{"mongodb", "indexes", "create", "c", `{"a":1}`, "--ttl", "-5"},
	} {
		_, err := runMuxcat(t, extra...)
		if e := output.ToError(err); err == nil || e.Code != output.CodeConfigInvalid {
			t.Fatalf("%v: err=%v, want CONFIG_INVALID", extra, err)
		}
	}
	// Valid input reaches the dial stage (closed port).
	_, err := runMuxcat(t, "mongodb", "indexes", "create", "c", `{"a":1}`)
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("valid create: err=%v, want CONNECT_FAILED", err)
	}
}

// TestIndexesReadonlyInterception verifies indexes create/drop refuse a
// readonly connection before dialing, while the read-only ops commands
// reach the dial stage (closed port → CONNECT_FAILED).
func TestIndexesReadonlyInterception(t *testing.T) {
	setupEnv(t)
	addConn(t, "ro", "--port", "1", "--timeout", "2s", "--readonly", "--database", "app", "--set-default")
	for _, args := range [][]string{
		{"mongodb", "indexes", "create", "c", `{"a":1}`},
		{"mongodb", "indexes", "drop", "c", "a_1", "--yes"},
	} {
		_, err := runMuxcat(t, args...)
		if e := output.ToError(err); err == nil || e.Code != output.CodeReadonlyViolation {
			t.Fatalf("%v on readonly conn: err=%v, want READONLY_VIOLATION", args, err)
		}
	}
	for _, args := range [][]string{
		{"mongodb", "indexes", "ls", "c"},
		{"mongodb", "stats"},
		{"mongodb", "stats", "c"},
		{"mongodb", "status"},
		{"mongodb", "users"},
		{"mongodb", "roles"},
	} {
		_, err := runMuxcat(t, args...)
		if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
			t.Fatalf("%v on readonly conn: err=%v, want CONNECT_FAILED", args, err)
		}
	}
}

func TestIndexesDropRequiresConfirmation(t *testing.T) {
	setupEnv(t)
	addConn(t, "w", "--port", "1", "--timeout", "2s", "--database", "app", "--set-default")
	_, err := runMuxcat(t, "mongodb", "indexes", "drop", "c", "a_1")
	if e := output.ToError(err); err == nil || e.Code != output.CodeMissingArgument {
		t.Fatalf("drop without --yes: err=%v, want MISSING_ARGUMENT", err)
	}
	_, err = runMuxcat(t, "mongodb", "indexes", "drop", "c", "a_1", "--yes")
	if e := output.ToError(err); err == nil || e.Code != output.CodeConnectFailed {
		t.Fatalf("drop with --yes: err=%v, want CONNECT_FAILED", err)
	}
}

func TestIndexRow(t *testing.T) {
	row := indexRow(bson.D{
		{Key: "name", Value: "email_1"},
		{Key: "key", Value: bson.D{{Key: "email", Value: int32(1)}}},
		{Key: "unique", Value: true},
		{Key: "expireAfterSeconds", Value: int32(3600)},
	})
	if row[0] != "email_1" || row[1] != `{"email":1}` || row[2] != true || row[3] != false || row[4] != "3600" {
		t.Fatalf("row = %v", row)
	}
	// Absent flags default to false and absent ttl renders empty.
	row = indexRow(bson.D{
		{Key: "name", Value: "_id_"},
		{Key: "key", Value: bson.D{{Key: "_id", Value: int32(1)}}},
	})
	if row[2] != false || row[3] != false || row[4] != "" {
		t.Fatalf("defaults row = %v", row)
	}
}

func TestStatsValue(t *testing.T) {
	dbStats := bson.D{
		{Key: "db", Value: "app"},
		{Key: "collections", Value: int64(3)},
		{Key: "objects", Value: int64(42)},
		{Key: "dataSize", Value: int64(1024)},
		{Key: "indexes", Value: int32(4)},
	}
	v := statsValue(dbStats)
	if v["db"] != "app" || v["collections"] != int64(3) || v["objects"] != int64(42) {
		t.Fatalf("dbStats value = %v", v)
	}
	// Absent fields are omitted (version differences).
	if _, ok := v["nindexes"]; ok {
		t.Fatalf("absent fields must be omitted: %v", v)
	}

	collStats := bson.D{
		{Key: "ns", Value: "app.users"},
		{Key: "count", Value: int64(7)},
		{Key: "nindexes", Value: int32(2)},
	}
	v = statsValue(collStats)
	if v["ns"] != "app.users" || v["count"] != int64(7) || v["nindexes"] != int32(2) {
		t.Fatalf("collStats value = %v", v)
	}
	if _, ok := v["db"]; ok {
		t.Fatalf("collStats should not carry dbStats fields: %v", v)
	}
}

func TestStatusValue(t *testing.T) {
	doc := bson.D{
		{Key: "host", Value: "db.example.com:27017"},
		{Key: "version", Value: "8.0.4"},
		{Key: "process", Value: "mongod"},
		{Key: "pid", Value: int64(1234)},
		{Key: "uptime", Value: int64(3600)},
		{Key: "connections", Value: bson.D{
			{Key: "current", Value: int32(5)},
			{Key: "available", Value: int32(800)},
			{Key: "active", Value: int32(2)},
		}},
		{Key: "mem", Value: bson.D{
			{Key: "resident", Value: int32(128)},
			{Key: "virtual", Value: int32(2048)},
		}},
		{Key: "opcounters", Value: bson.D{
			{Key: "insert", Value: int64(10)},
			{Key: "query", Value: int64(20)},
			{Key: "update", Value: int64(3)},
			{Key: "delete", Value: int64(1)},
			{Key: "getmore", Value: int64(4)},
			{Key: "command", Value: int64(50)},
		}},
		{Key: "wiredTiger", Value: bson.D{
			{Key: "cache", Value: bson.D{
				{Key: "bytes currently in cache", Value: int64(4096)},
				{Key: "maximum bytes configured", Value: int64(8192)},
			}},
		}},
	}
	v := statusValue(doc)
	if v["host"] != "db.example.com:27017" || v["version"] != "8.0.4" || v["uptime"] != int64(3600) {
		t.Fatalf("top-level: %v", v)
	}
	if v["connections.current"] != int32(5) || v["connections.available"] != int32(800) ||
		v["connections.active"] != int32(2) {
		t.Fatalf("connections: %v", v)
	}
	if v["mem.resident"] != int32(128) || v["mem.virtual"] != int32(2048) {
		t.Fatalf("mem: %v", v)
	}
	if v["opcounters.command"] != int64(50) || v["opcounters.insert"] != int64(10) {
		t.Fatalf("opcounters: %v", v)
	}
	if v["wiredTiger.cache.currentBytes"] != int64(4096) || v["wiredTiger.cache.maxBytes"] != int64(8192) {
		t.Fatalf("wiredTiger cache: %v", v)
	}

	// Without wiredTiger (e.g. in-memory engine) no cache section appears.
	v = statusValue(bson.D{{Key: "host", Value: "h"}, {Key: "version", Value: "7.0.0"}})
	if _, ok := v["wiredTiger.cache.currentBytes"]; ok {
		t.Fatalf("cache section should be skipped: %v", v)
	}
	if _, ok := v["connections.current"]; ok {
		t.Fatalf("missing sections should be skipped: %v", v)
	}
}

// TestUsersRolesNoCredentials verifies users/roles table rows carry only
// identity fields — usersInfo/rolesInfo responses never contain
// credential material, and the rows must not either.
func TestUsersRolesNoCredentials(t *testing.T) {
	userDoc := bson.D{
		{Key: "user", Value: "app"},
		{Key: "db", Value: "shop"},
		{Key: "roles", Value: bson.A{
			bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "shop"}},
			bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "logs"}},
		}},
	}
	row := userRow(userDoc)
	if row[0] != "app" || row[1] != "shop" || row[2] != "readWrite@shop,read@logs" {
		t.Fatalf("userRow = %v", row)
	}

	roleDoc := bson.D{
		{Key: "role", Value: "auditor"},
		{Key: "db", Value: "admin"},
		{Key: "inheritedRoles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "admin"}}}},
		{Key: "privileges", Value: bson.A{bson.D{}, bson.D{}}},
	}
	rrow := roleRow(roleDoc)
	if rrow[0] != "auditor" || rrow[1] != "admin" || rrow[2] != 1 || rrow[3] != 2 {
		t.Fatalf("roleRow = %v", rrow)
	}

	for _, row := range [][]any{row, rrow} {
		for _, cell := range row {
			s, _ := cell.(string)
			low := strings.ToLower(s)
			if strings.Contains(low, "password") || strings.Contains(low, "credential") ||
				strings.Contains(low, "scram") {
				t.Fatalf("row leaks credential-looking content: %v", row)
			}
		}
	}
}
