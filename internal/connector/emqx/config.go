package emqx

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
)

// FileName is the emqx connector's config file name.
const FileName = "emqx.json"

// Instance is an EMQX endpoint: the base URL of the HTTP API (server
// root, typically the dashboard port 18083).
type Instance struct {
	URL string `json:"url"`
}

// Connection is a session config pointing at an instance: two credential
// pairs (dashboard username/password and/or API key/secret, at least one
// complete pair) plus usage policies. Password and APISecret are stored
// as enc:v1: blobs and are never echoed back in output; APIKey is treated
// like a username and stored unencrypted.
type Connection struct {
	Instance  string `json:"instance"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	APIKey    string `json:"apiKey,omitempty"`
	APISecret string `json:"apiSecret,omitempty"`
	Readonly  bool   `json:"readonly,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
}

// Config is the emqx.json model.
type Config struct {
	Version           int                   `json:"version"`
	Instances         map[string]Instance   `json:"instances,omitempty"`
	Connections       map[string]Connection `json:"connections,omitempty"`
	DefaultConnection string                `json:"defaultConnection,omitempty"`
}

// loadConfig reads emqx.json; a missing file yields an empty config.
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
			"specify one with -c/--conn, or create one with muxcat emqx conn add <name> --url <url> --username <user> --set-default")
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

// credPairsOf reports which credential pairs a connection carries:
// dashboard (username+password) and apikey (apiKey+apiSecret). Each pair
// must be complete and at least one pair is required; violations are
// CONFIG_INVALID.
func credPairsOf(conn Connection) (dashboard, apikey bool, err error) {
	dashUser, dashPass := conn.Username != "", conn.Password != ""
	key, secret := conn.APIKey != "", conn.APISecret != ""
	switch {
	case dashUser != dashPass:
		return false, false, output.NewError(output.CodeConfigInvalid,
			"username and password must be passed together", "the dashboard credential pair is incomplete")
	case key != secret:
		return false, false, output.NewError(output.CodeConfigInvalid,
			"apiKey and apiSecret must be passed together", "the API key credential pair is incomplete")
	case !dashUser && !key:
		return false, false, output.NewError(output.CodeConfigInvalid,
			"a connection needs at least one complete credential pair",
			"pass --username/--password (dashboard) and/or --api-key/--api-secret")
	}
	return dashUser, key, nil
}

// authLabel describes the connection's configured credential pairs, for
// the conn ls auth column.
func authLabel(conn Connection) string {
	dashboard, apikey, err := credPairsOf(conn)
	switch {
	case err != nil:
		return "invalid"
	case dashboard && apikey:
		return "dashboard+apikey"
	case apikey:
		return "apikey"
	case dashboard:
		return "dashboard"
	default:
		return "none"
	}
}

// normalizeURL validates a base URL and strips trailing slashes. The URL
// must carry an http/https scheme and must not embed credentials:
// plaintext credentials never touch the config file.
func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", output.NewError(output.CodeConfigInvalid,
			"invalid url: "+raw, "example: http://127.0.0.1:18083 or https://emqx.example.com")
	}
	if u.User != nil {
		return "", output.NewError(output.CodeConfigInvalid,
			"url must not contain credentials (user:password@host)",
			"pass credentials with --username/--password or --api-key/--api-secret; they are stored encrypted")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
