package openobserve

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
)

// FileName is the openobserve connector's config file name.
const FileName = "openobserve.json"

// Instance is an OpenObserve endpoint: the base URL only. Credentials and
// the organization live on connections, so one instance can serve multiple
// connections (different orgs or users).
type Instance struct {
	URL string `json:"url"`
}

// Connection is a session config pointing at an instance: organization,
// credentials, and usage policies. Password is stored as an enc:v1: blob
// and is never echoed back in output.
type Connection struct {
	Instance string `json:"instance"`
	Org      string `json:"org,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Readonly bool   `json:"readonly,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

// Config is the openobserve.json model.
type Config struct {
	Version           int                   `json:"version"`
	Instances         map[string]Instance   `json:"instances,omitempty"`
	Connections       map[string]Connection `json:"connections,omitempty"`
	DefaultConnection string                `json:"defaultConnection,omitempty"`
}

// loadConfig reads openobserve.json; a missing file yields an empty config.
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
			"specify one with -c/--conn, or create one with muxcat openobserve conn add <name> --url <url> --set-default")
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

// org returns the connection's organization, defaulting to "default".
func (conn Connection) org() string {
	if conn.Org == "" {
		return "default"
	}
	return conn.Org
}

// normalizeURL validates a base URL and strips trailing slashes. The URL
// must carry an http/https scheme and must not embed credentials: basic
// auth is built from the connection's username/password, and plaintext
// credentials never touch the config file.
func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", output.NewError(output.CodeConfigInvalid,
			"invalid url: "+raw, "example: http://127.0.0.1:5080 or https://o2.example.com")
	}
	if u.User != nil {
		return "", output.NewError(output.CodeConfigInvalid,
			"url must not contain credentials (user:password@host)",
			"pass credentials with --username/--password; they are stored encrypted")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
