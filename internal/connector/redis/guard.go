package redis

import (
	"fmt"
	"strings"

	"github.com/ravenmk2/muxcat/internal/output"
)

// readonlyCmds is the whitelist of commands permitted on readonly
// connections. exec passthrough and the structured commands share this
// table; CONFIG GET and MEMORY USAGE are split out by subcommand in
// isReadonly. EVAL is deliberately absent: it counts as a write.
var readonlyCmds = map[string]bool{
	"GET": true, "MGET": true, "GETRANGE": true, "STRLEN": true,
	"EXISTS": true, "TTL": true, "PTTL": true, "TYPE": true,
	"SCAN": true, "SSCAN": true, "HSCAN": true, "ZSCAN": true,
	"RANDOMKEY": true,
	// hash reads
	"HGET": true, "HGETALL": true, "HMGET": true, "HKEYS": true,
	"HVALS": true, "HLEN": true, "HEXISTS": true, "HSTRLEN": true,
	"HRANDFIELD": true,
	// list reads
	"LRANGE": true, "LLEN": true, "LINDEX": true, "LPOS": true,
	// set reads
	"SMEMBERS": true, "SCARD": true, "SISMEMBER": true, "SMISMEMBER": true,
	"SRANDMEMBER": true, "SINTER": true, "SINTERCARD": true,
	"SUNION": true, "SDIFF": true,
	// sorted set reads
	"ZRANGE": true, "ZREVRANGE": true, "ZRANGEBYSCORE": true,
	"ZREVRANGEBYSCORE": true, "ZRANGEBYLEX": true, "ZREVRANGEBYLEX": true,
	"ZCARD": true, "ZSCORE": true, "ZMSCORE": true, "ZRANK": true,
	"ZREVRANK": true, "ZCOUNT": true, "ZLEXCOUNT": true, "ZRANDMEMBER": true,
	// stream reads
	"XINFO": true, "XRANGE": true, "XREVRANGE": true, "XLEN": true,
	// server introspection
	"DBSIZE": true, "INFO": true, "PING": true, "ECHO": true, "OBJECT": true,
}

// dangerousCmds are rejected unless the connection sets allowDangerous.
// CONFIG is handled by subcommand in isDangerous (only CONFIG GET passes).
var dangerousCmds = map[string]bool{
	"FLUSHALL": true, "FLUSHDB": true, "SHUTDOWN": true, "DEBUG": true,
	"KEYS": true, "RESET": true, "FAILOVER": true, "REPLICAOF": true,
	"SLAVEOF": true, "SWAPDB": true, "SCRIPT": true,
}

// guardCommand applies the connector-side interception rules to a command
// name and its arguments. It is an anti-footgun measure, not a security
// boundary: Lua can bypass it; hard constraints need server-side ACLs.
func guardCommand(conn Connection, name string, args ...any) error {
	upper := strings.ToUpper(name)
	if conn.Readonly && !isReadonly(upper, args) {
		return output.NewError(output.CodeReadonlyViolation,
			fmt.Sprintf("command %s is not allowed on a readonly connection", upper),
			"use a writable connection (-c), or recreate the connection without --readonly")
	}
	if !conn.AllowDangerous && isDangerous(upper, args) {
		return output.NewError(output.CodeUnsupportedOperation,
			fmt.Sprintf("command %s is blocked by default (dangerous operation)", upper),
			"recreate the connection with --allow-dangerous to enable it")
	}
	return nil
}

// subcommand returns the uppercased first argument ("" when absent).
func subcommand(args []any) string {
	if len(args) == 0 {
		return ""
	}
	s, _ := args[0].(string)
	return strings.ToUpper(s)
}

func isReadonly(name string, args []any) bool {
	switch name {
	case "CONFIG":
		return subcommand(args) == "GET"
	case "MEMORY":
		return subcommand(args) == "USAGE"
	}
	return readonlyCmds[name]
}

func isDangerous(name string, args []any) bool {
	if name == "CONFIG" {
		return subcommand(args) != "GET"
	}
	return dangerousCmds[name]
}
