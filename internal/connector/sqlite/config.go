package sqlite

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
)

// FileName is the sqlite connector's config file name.
const FileName = "sqlite.json"

// Instance is a sqlite endpoint: a database file path.
type Instance struct {
	Path string `json:"path"`
}

// Connection is a session config pointing at an instance. sqlite has no
// password field.
type Connection struct {
	Instance string `json:"instance"`
	Readonly bool   `json:"readonly,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

// Config is the sqlite.json model.
type Config struct {
	Version           int                   `json:"version"`
	Instances         map[string]Instance   `json:"instances,omitempty"`
	Connections       map[string]Connection `json:"connections,omitempty"`
	DefaultConnection string                `json:"defaultConnection,omitempty"`
}

// loadConfig reads sqlite.json; a missing file yields an empty config.
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
			"specify one with -c/--conn, or create one with muxcat sqlite conn add <name> --path <file> --set-default")
	}
	conn, ok := cfg.Connections[name]
	if !ok {
		return "", Connection{}, output.NewError(output.CodeConnNotFound,
			fmt.Sprintf("connection not found: %s", name),
			"list connections with muxcat sqlite conn ls, or create one with muxcat sqlite conn add")
	}
	return name, conn, nil
}

// instancePath resolves the file path of the instance a connection points
// to (with ~ expansion).
func (c *Config) instancePath(conn Connection) (string, error) {
	inst, ok := c.Instances[conn.Instance]
	if !ok {
		return "", output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("instance %q referenced by the connection does not exist", conn.Instance), "")
	}
	return ExpandHome(inst.Path), nil
}

// ExpandHome expands a leading ~ to the user's home directory.
func ExpandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// dsn builds a DSN from the _pragma= common subset supported by both
// drivers; readonly connections get mode=ro.
func dsn(path string, readonly bool) string {
	if path == ":memory:" {
		return ":memory:"
	}
	d := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if readonly {
		d += "&mode=ro"
	}
	return d
}
