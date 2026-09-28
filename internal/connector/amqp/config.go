package amqp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
)

// FileName is the amqp connector's config file name.
const FileName = "amqp.json"

// Protocol identifiers; the protocol is an instance attribute (endpoint
// dialect) and selects the client implementation.
const (
	Protocol091 = "0.9.1"
	Protocol10  = "1.0"
)

// Instance is an AMQP endpoint: the broker URL and the wire protocol to
// speak. Credentials and vhost live on connections, so one instance can
// serve multiple connections (different users or vhosts).
type Instance struct {
	URL      string `json:"url"`
	Protocol string `json:"protocol,omitempty"`
}

// protocol returns the effective protocol, defaulting to 1.0.
func (i Instance) protocol() string {
	if i.Protocol == "" {
		return Protocol10
	}
	return i.Protocol
}

// Connection is a session config pointing at an instance: credentials,
// vhost, and usage policies. Password is stored as an enc:v1: blob and
// never echoed back in output. Vhost defaults to "/".
type Connection struct {
	Instance      string `json:"instance"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Vhost         string `json:"vhost,omitempty"`
	Readonly      bool   `json:"readonly,omitempty"`
	Timeout       string `json:"timeout,omitempty"`
	TLSSkipVerify bool   `json:"tlsSkipVerify,omitempty"`
}

// Config is the amqp.json model.
type Config struct {
	Version           int                   `json:"version"`
	Instances         map[string]Instance   `json:"instances,omitempty"`
	Connections       map[string]Connection `json:"connections,omitempty"`
	DefaultConnection string                `json:"defaultConnection,omitempty"`
}

// loadConfig reads amqp.json; a missing file yields an empty config.
func loadConfig() (*Config, error) {
	doc, err := config.Load(FileName)
	if err != nil {
		return nil, err
	}
	return docToConfig(doc)
}

func docToConfig(doc config.Doc) (*Config, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, output.NewError(output.CodeConfigInvalid, FileName+" has invalid structure: "+err.Error(), "")
	}
	for name, inst := range c.Instances {
		if err := validateProtocol(inst.Protocol); err != nil {
			return nil, output.NewError(output.CodeConfigInvalid,
				fmt.Sprintf("instance %q: %s", name, err.Message), err.Hint)
		}
	}
	if c.Instances == nil {
		c.Instances = map[string]Instance{}
	}
	if c.Connections == nil {
		c.Connections = map[string]Connection{}
	}
	return &c, nil
}

func configToDoc(c *Config) (config.Doc, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var doc config.Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func saveConfig(c *Config) error {
	doc, err := configToDoc(c)
	if err != nil {
		return err
	}
	return config.Save(FileName, doc)
}

// validateProtocol enforces the strict protocol enum ("" defaults to 1.0).
func validateProtocol(p string) *output.Error {
	switch p {
	case "", Protocol091, Protocol10:
		return nil
	default:
		return output.NewError(output.CodeConfigInvalid,
			"invalid protocol: "+p, `valid values: "0.9.1" | "1.0"`)
	}
}

// resolve resolves a connection from the -c/--conn flag, falling back to
// defaultConnection.
func resolve(cfg *Config, flagConn string) (string, Connection, error) {
	name := flagConn
	if name == "" {
		name = cfg.DefaultConnection
	}
	if name == "" {
		return "", Connection{}, output.NewError(output.CodeConnNotFound,
			"no connection specified and no default connection set",
			"specify one with -c/--conn, or create one with muxcat amqp conn add <name> --url <url> --set-default")
	}
	conn, ok := cfg.Connections[name]
	if !ok {
		return "", Connection{}, connNotFound(name)
	}
	return name, conn, nil
}

// instanceOf returns the instance a connection points to.
func (c *Config) instanceOf(conn Connection) (Instance, error) {
	inst, ok := c.Instances[conn.Instance]
	if !ok {
		return Instance{}, output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("instance %q referenced by the connection does not exist", conn.Instance), "")
	}
	return inst, nil
}

// normalizeURL validates a broker URL. The scheme must be amqp/amqps and
// the URL must not embed credentials: username/password live on the
// connection, and plaintext credentials never touch the config file.
func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// The raw input may embed user:pass@ credentials; a parse failure
		// must not echo it back.
		return "", output.NewError(output.CodeConfigInvalid,
			"invalid url: cannot parse it", "example: amqp://127.0.0.1:5672 or amqps://broker.example.com:5671")
	}
	if u.Host == "" || (u.Scheme != "amqp" && u.Scheme != "amqps") {
		return "", output.NewError(output.CodeConfigInvalid,
			"invalid url: "+raw, "example: amqp://127.0.0.1:5672 or amqps://broker.example.com:5671")
	}
	if u.User != nil {
		return "", output.NewError(output.CodeConfigInvalid,
			"url must not contain credentials (user:password@host)",
			"pass credentials with --username/--password; they are stored encrypted")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

// schemeOf returns the URL scheme of an instance ("amqp" or "amqps").
func schemeOf(inst Instance) string {
	if u, err := url.Parse(inst.URL); err == nil {
		return u.Scheme
	}
	return ""
}

// dialURI builds the dial URI for both protocols: credentials ride in the
// userinfo, the vhost in the path ("/" stays "/", others are
// percent-encoded). The URI contains the plaintext password; it is used
// for dialing only and must never be written to any output or error.
func dialURI(inst Instance, conn Connection, vhost, password string) string {
	u, err := url.Parse(inst.URL)
	if err != nil {
		return inst.URL
	}
	switch {
	case conn.Username != "" || password != "":
		u.User = url.UserPassword(conn.Username, password)
	}
	if vhost == "" || vhost == "/" {
		u.Path = "/"
	} else {
		// Path holds the decoded form, RawPath its encoding; String()
		// prefers RawPath, so the vhost travels percent-encoded exactly
		// once ("/prod" -> "/%2Fprod").
		u.Path = "/" + vhost
		u.RawPath = "/" + url.PathEscape(vhost)
	}
	return u.String()
}
