package mysql

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
)

// FileName is the mysql connector's config file name.
const FileName = "mysql.json"

// Instance is a MySQL endpoint: pure endpoint properties only.
type Instance struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	TLS  bool   `json:"tls,omitempty"`
}

// Connection is a session config pointing at an instance: credentials, the
// default database, and usage policies. Password is stored as an enc:v1:
// blob and is never echoed back in output.
type Connection struct {
	Instance string `json:"instance"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
	Readonly bool   `json:"readonly,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

// Config is the mysql.json model.
type Config struct {
	Version           int                   `json:"version"`
	Instances         map[string]Instance   `json:"instances,omitempty"`
	Connections       map[string]Connection `json:"connections,omitempty"`
	DefaultConnection string                `json:"defaultConnection,omitempty"`
}

// loadConfig reads mysql.json; a missing file yields an empty config.
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
			"specify one with -c/--conn, or create one with muxcat mysql conn add <name> --host <host> --set-default")
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

// addr renders the host:port of an instance.
func addr(inst Instance) string {
	return net.JoinHostPort(inst.Host, strconv.Itoa(inst.Port))
}
