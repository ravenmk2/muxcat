package mqtt

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
)

// FileName is the mqtt connector's config file name.
const FileName = "mqtt.json"

// Protocol versions; the version is an instance attribute (endpoint
// dialect) and selects the client implementation.
const (
	ProtocolV3 = 3 // MQTT 3.1.1
	ProtocolV5 = 5 // MQTT 5.0
)

// Instance is an MQTT endpoint: the broker URL and the protocol version to
// speak. Credentials live on connections, so one instance can serve
// multiple connections (different users or client IDs).
type Instance struct {
	URL             string `json:"url"`
	ProtocolVersion int    `json:"protocolVersion,omitempty"`
}

// protocolVersion returns the effective protocol version, defaulting to 3.
func (i Instance) protocolVersion() int {
	if i.ProtocolVersion == 0 {
		return ProtocolV3
	}
	return i.ProtocolVersion
}

// Connection is a session config pointing at an instance: credentials,
// client ID, and usage policies. Password is stored as an enc:v1: blob
// and never echoed back in output. An empty ClientID yields a random
// muxcat-<pid>-<rand> one per session (batch mode is clean-start only).
type Connection struct {
	Instance      string `json:"instance"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	ClientID      string `json:"clientId,omitempty"`
	Readonly      bool   `json:"readonly,omitempty"`
	Timeout       string `json:"timeout,omitempty"`
	TLSSkipVerify bool   `json:"tlsSkipVerify,omitempty"`
}

// Config is the mqtt.json model.
type Config struct {
	Version           int                   `json:"version"`
	Instances         map[string]Instance   `json:"instances,omitempty"`
	Connections       map[string]Connection `json:"connections,omitempty"`
	DefaultConnection string                `json:"defaultConnection,omitempty"`
}

// loadConfig reads mqtt.json; a missing file yields an empty config.
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
		if err := validateProtocolVersion(inst.ProtocolVersion); err != nil {
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

// validateProtocolVersion enforces the strict version enum (0 defaults
// to 3).
func validateProtocolVersion(v int) *output.Error {
	switch v {
	case 0, ProtocolV3, ProtocolV5:
		return nil
	default:
		return output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("invalid protocolVersion: %d", v), "valid values: 3 | 5")
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
			"specify one with -c/--conn, or create one with muxcat mqtt conn add <name> --url <url> --set-default")
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

// schemeAliases maps paho-style scheme aliases to the canonical schemes;
// config files only ever store the canonical ones.
var schemeAliases = map[string]string{
	"tcp": "mqtt",
	"ssl": "mqtts",
	"tls": "mqtts",
}

// defaultPorts fills in the well-known port when the URL omits one.
var defaultPorts = map[string]string{
	"mqtt": "1883", "mqtts": "8883", "ws": "80", "wss": "443",
}

// normalizeURL validates and canonicalizes a broker URL. The scheme must
// be mqtt/mqtts/ws/wss (aliases tcp/ssl/tls are normalized), the host must
// be non-empty, and the URL must not embed credentials: username/password
// live on the connection, and plaintext credentials never touch the
// config file. A missing port is filled with the scheme's default.
func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// The raw input may embed user:pass@ credentials; a parse failure
		// must not echo it back.
		return "", output.NewError(output.CodeConfigInvalid,
			"invalid url: cannot parse it", "example: mqtt://127.0.0.1:1883, mqtts://broker.example.com:8883 or ws://host:8083/mqtt")
	}
	scheme := u.Scheme
	if alias, ok := schemeAliases[scheme]; ok {
		scheme = alias
	}
	// Reject embedded credentials before any branch that echoes the raw
	// input: user:pass@ must never appear in an error message.
	if u.User != nil {
		return "", output.NewError(output.CodeConfigInvalid,
			"url must not contain credentials (user:password@host)",
			"pass credentials with --username/--password; they are stored encrypted")
	}
	if _, ok := defaultPorts[scheme]; !ok || u.Host == "" {
		return "", output.NewError(output.CodeConfigInvalid,
			"invalid url: "+raw, "example: mqtt://127.0.0.1:1883, mqtts://broker.example.com:8883 or ws://host:8083/mqtt")
	}
	u.Scheme = scheme
	if u.Port() == "" {
		u.Host = u.Host + ":" + defaultPorts[scheme]
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

// schemeOf returns the URL scheme of an instance ("mqtt", "mqtts", "ws"
// or "wss").
func schemeOf(inst Instance) string {
	if u, err := url.Parse(inst.URL); err == nil {
		return u.Scheme
	}
	return ""
}
