package mongodb

import (
	"net"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"

	"github.com/ravenmk2/muxcat/internal/output"
)

// uriParts holds the pieces parsed from a mongodb:// URI; they serve as
// defaults for conn add flags (explicit flags win).
type uriParts struct {
	host        string
	port        int
	portSet     bool
	username    string
	password    string
	passwordSet bool
	authSource  string
	database    string
	tls         bool
	tlsSet      bool
	replicaSet  string
}

// parseURI parses a mongodb:// URI. mongodb+srv:// and multi-host URIs are
// rejected (phase 1 supports a single host only). Error messages never
// echo the URI's userinfo, which may carry a password.
func parseURI(s string) (uriParts, error) {
	var parts uriParts
	cs, err := connstring.ParseAndValidate(s)
	if err != nil {
		return parts, output.NewError(output.CodeConfigInvalid,
			"invalid --uri: "+sanitizeURIErr(err.Error(), s), "")
	}
	if cs.Scheme == connstring.SchemeMongoDBSRV {
		return parts, output.NewError(output.CodeConfigInvalid,
			"the URI uses the mongodb+srv scheme",
			"mongodb+srv:// is not supported yet; use --host/--port")
	}
	if len(cs.Hosts) > 1 {
		return parts, output.NewError(output.CodeConfigInvalid,
			"the URI lists multiple hosts",
			"multiple hosts (replica set seeds) are not supported yet")
	}
	if len(cs.Hosts) == 1 {
		host, portStr, err := net.SplitHostPort(cs.Hosts[0])
		if err != nil {
			parts.host = cs.Hosts[0]
		} else {
			parts.host = host
			if p, err := strconv.Atoi(portStr); err == nil {
				parts.port = p
				parts.portSet = true
			}
		}
	}
	parts.username = cs.Username
	parts.password = cs.Password
	parts.passwordSet = cs.PasswordSet
	parts.authSource = cs.AuthSource
	parts.database = cs.Database
	parts.tls = cs.SSL
	parts.tlsSet = cs.SSLSet
	parts.replicaSet = cs.ReplicaSet
	return parts, nil
}

// sanitizeURIErr removes the URI's userinfo (between "://" and "@") from
// a parser error message so a password never leaks into the output.
func sanitizeURIErr(msg, uri string) string {
	i := strings.Index(uri, "://")
	if i < 0 {
		return msg
	}
	rest := uri[i+3:]
	if j := strings.IndexAny(rest, "/?"); j >= 0 {
		rest = rest[:j]
	}
	at := strings.Index(rest, "@")
	if at <= 0 {
		return msg
	}
	return strings.ReplaceAll(msg, rest[:at], "***")
}
