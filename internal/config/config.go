// Package config resolves muxcat's config directory and reads/writes JSON
// config documents.
//
// Config directory: MUXCAT_HOME environment variable >
// os.UserHomeDir()/.config/muxcat. Each document is a JSON file:
// config.json is {version, props}; connector documents are
// {version, instances/connections/...}.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ravenmk2/muxcat/internal/output"
)

// MainFile is the main config file name.
const MainFile = "config.json"

// DirEnv is the environment variable that overrides the config directory.
const DirEnv = "MUXCAT_HOME"

// Doc is a config document. JSON numbers decode to float64.
type Doc map[string]any

// Version returns the document version, defaulting to 1 when absent.
func (d Doc) Version() int {
	if v, ok := d["version"].(float64); ok {
		return int(v)
	}
	return 1
}

// Dir resolves the config directory:
// MUXCAT_HOME > os.UserHomeDir()/.config/muxcat.
func Dir() (string, error) {
	if v := os.Getenv(DirEnv); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", output.NewError(output.CodeConfigInvalid,
			"cannot resolve user home directory: "+err.Error(), "set the "+DirEnv+" environment variable to choose a config directory")
	}
	return filepath.Join(home, ".config", "muxcat"), nil
}

// Path returns the full path of a document inside the config directory.
func Path(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Load reads a config document. A missing file yields the zero-value
// document (version=1).
func Load(name string) (Doc, error) {
	path, err := Path(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Doc{"version": float64(1)}, nil
	}
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("failed to read config %s: %v", path, err), "")
	}
	var doc Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("config file %s is not valid JSON: %v", path, err), "fix the file or delete it and retry")
	}
	if _, ok := doc["version"]; !ok {
		doc["version"] = float64(1)
	}
	return doc, nil
}

// Save writes a config document atomically: 2-space-indented JSON, temp
// file + rename, permission 0600.
func Save(name string, doc Doc) error {
	path, err := Path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return output.NewError(output.CodeConfigInvalid,
			"failed to create config directory: "+err.Error(), "")
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".muxcat-*.tmp")
	if err != nil {
		return output.NewError(output.CodeConfigInvalid, "failed to create temp file: "+err.Error(), "")
	}
	tmpName := tmp.Name()
	// Permission bits are best-effort on Windows; failure does not abort.
	_ = os.Chmod(tmpName, 0o600)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return output.NewError(output.CodeConfigInvalid, "failed to write config: "+err.Error(), "")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return output.NewError(output.CodeConfigInvalid, "failed to write config: "+err.Error(), "")
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("failed to save config %s: %v", path, err), "")
	}
	return nil
}

// NormalizePath implicitly prefixes dot paths of config.json with "props."
// (except "version", "props" itself, and paths already carrying the prefix).
func NormalizePath(name, path string) string {
	if name != MainFile {
		return path
	}
	if path == "version" || path == "props" || strings.HasPrefix(path, "props.") {
		return path
	}
	return "props." + path
}

// Get reads a document field by dot path, descending through maps segment
// by segment.
func Get(doc Doc, path string) (any, bool) {
	cur := any(map[string]any(doc))
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Set writes a document field by dot path, creating intermediate maps as
// needed.
func Set(doc Doc, path string, value any) {
	segs := strings.Split(path, ".")
	cur := map[string]any(doc)
	for i, seg := range segs {
		if i == len(segs)-1 {
			cur[seg] = value
			return
		}
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
}

// ParseValue parses a command-line string as a JSON value
// (number/bool/null/quoted string), falling back to the raw string.
func ParseValue(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}
