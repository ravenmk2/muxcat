// Package schema embeds JSON Schemas (Draft 2020-12) via go:embed and
// provides config document validation.
package schema

import (
	"bytes"
	"embed"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravenmk2/muxcat/internal/output"
)

//go:embed *.schema.json
var FS embed.FS

// baseURI is the common prefix of the schema $ids.
const baseURI = "https://github.com/ravenmk2/muxcat/schema/"

// SchemaName returns the embedded schema name matching a config file, or
// "" when none matches. Convention: config.json → config.schema.json;
// <connector>.json → <connector>.schema.json.
func SchemaName(configFile string) string {
	name := strings.TrimSuffix(configFile, ".json") + ".schema.json"
	if _, err := FS.Open(name); err != nil {
		return ""
	}
	return name
}

// Validate validates a config document. configFile is the file name (e.g.
// config.json, sqlite.json) and selects the schema; files without a
// matching schema yield CONFIG_INVALID.
func Validate(configFile string, data []byte) error {
	name := SchemaName(configFile)
	if name == "" {
		return output.NewError(output.CodeConfigInvalid,
			"no schema matches "+configFile, "")
	}

	c := jsonschema.NewCompiler()
	entries, err := FS.ReadDir(".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		raw, err := FS.ReadFile(e.Name())
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return output.NewError(output.CodeConfigInvalid,
				fmt.Sprintf("embedded schema %s failed to parse: %v", e.Name(), err), "")
		}
		if err := c.AddResource(baseURI+e.Name(), doc); err != nil {
			return err
		}
	}
	sch, err := c.Compile(baseURI + name)
	if err != nil {
		return output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("schema %s failed to compile: %v", name, err), "")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return output.NewError(output.CodeConfigInvalid,
			configFile+" is not valid JSON: "+err.Error(), "")
	}
	if err := sch.Validate(inst); err != nil {
		return output.NewError(output.CodeConfigInvalid,
			configFile+" failed validation: "+err.Error(), "")
	}
	return nil
}
