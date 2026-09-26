package etcd

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
)

// FileName is the etcd connector's config file name.
const FileName = "etcd.json"

// Instance is an etcd cluster endpoint set plus TLS material: pure
// endpoint properties only; credentials and policies live on connections.
type Instance struct {
	Endpoints []string `json:"endpoints"`
	TLS       bool     `json:"tls,omitempty"`
	CACert    string   `json:"cacert,omitempty"`
	Cert      string   `json:"cert,omitempty"`
	Key       string   `json:"key,omitempty"`
}

// Connection is a session config pointing at an instance: credentials and
// usage policies. Password is stored as an enc:v1: blob and is never
// echoed back in output.
type Connection struct {
	Instance       string `json:"instance"`
	Username       string `json:"username,omitempty"`
	Password       string `json:"password,omitempty"`
	Readonly       bool   `json:"readonly,omitempty"`
	AllowDangerous bool   `json:"allowDangerous,omitempty"`
	Timeout        string `json:"timeout,omitempty"`
}

// Config is the etcd.json model.
type Config struct {
	Version           int                   `json:"version"`
	Instances         map[string]Instance   `json:"instances,omitempty"`
	Connections       map[string]Connection `json:"connections,omitempty"`
	DefaultConnection string                `json:"defaultConnection,omitempty"`
}

// loadConfig reads etcd.json; a missing file yields an empty config.
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
			"specify one with -c/--conn, or create one with muxcat etcd conn add <name> --endpoints <host:port> --set-default")
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

// endpointsText renders the endpoint list of an instance.
func endpointsText(inst Instance) string {
	return strings.Join(inst.Endpoints, ",")
}

// parseEndpoints validates a comma-separated host:port list.
func parseEndpoints(s string) ([]string, error) {
	var endpoints []string
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(e)
		if err != nil || strings.TrimSpace(host) == "" {
			return nil, output.NewError(output.CodeConfigInvalid,
				fmt.Sprintf("invalid endpoint %q: expected host:port", e), "example: 127.0.0.1:2379")
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return nil, output.NewError(output.CodeConfigInvalid,
				fmt.Sprintf("invalid port in endpoint %q", e), "valid range: 1-65535")
		}
		endpoints = append(endpoints, e)
	}
	if len(endpoints) == 0 {
		return nil, output.NewError(output.CodeConfigInvalid,
			"no endpoints given", "pass at least one host:port, e.g. 127.0.0.1:2379")
	}
	return endpoints, nil
}
