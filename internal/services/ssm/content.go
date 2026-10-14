package ssm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
)

const (
	formatJSON = "JSON"
	formatYAML = "YAML"
	formatText = "TEXT"

	maxContent = 64 * 1024

	platformLinux   = "Linux"
	platformMacOS   = "MacOS"
	platformWindows = "Windows"
)

// schemaVersions are the document schema versions TinyStack accepts.
var schemaVersions = []string{"0.3", "1.0", "1.2", "2.0", "2.2"}

// content is what the console and clients read out of a document's content.
type content struct {
	Schema      string
	Description string
	Parameters  []api.DocumentParameter
	// Plugins are the plugins a command document runs: the actions of its
	// main steps, or the runtimeConfig keys of schema 1.2.
	Plugins []string
}

// decode parses document content of format into a generic map.
func decode(text, format string) (map[string]any, error) {
	var out map[string]any
	var err error
	switch format {
	case formatJSON:
		err = json.Unmarshal([]byte(text), &out)
	case formatYAML:
		err = yaml.Unmarshal([]byte(text), &out)
	}

	if err != nil || out == nil {
		return nil, awserror.New("InvalidDocumentContent", "The content for the document is not valid.")
	}

	return out, nil
}

// parseContent validates document content of docType and returns what it
// describes. TEXT content is not parsed.
func parseContent(text, format, docType string) (*content, error) {
	if len(text) > maxContent {
		return nil, awserror.New("MaxDocumentSizeExceeded", "The document size is larger than the 64 KB limit.")
	}

	if format == formatText {
		return &content{}, nil
	}

	doc, err := decode(text, format)
	if err != nil {
		return nil, err
	}

	out := &content{Schema: scalar(doc["schemaVersion"]), Description: scalar(doc["description"])}
	if !slices.Contains(schemaVersions, out.Schema) {
		return nil, awserror.New("InvalidDocumentSchemaVersion", "The document schema version is not supported: "+
			out.Schema)
	}

	steps, _ := doc["mainSteps"].([]any)
	for _, step := range steps {
		if m, ok := step.(map[string]any); ok {
			out.Plugins = append(out.Plugins, scalar(m["action"]))
		}
	}

	if runtime, ok := doc["runtimeConfig"].(map[string]any); ok {
		for plugin := range runtime {
			out.Plugins = append(out.Plugins, plugin)
		}

		slices.Sort(out.Plugins)
	}

	if docType == "Command" && len(out.Plugins) == 0 {
		return nil, awserror.New("InvalidDocumentContent", "A command document needs mainSteps, or runtimeConfig "+
			"in schema 1.2.")
	}

	if docType == "Automation" && len(steps) == 0 {
		return nil, awserror.New("InvalidDocumentContent", "An automation document needs mainSteps.")
	}

	out.Parameters = documentParameters(doc["parameters"])

	return out, nil
}

// scalar renders a JSON or YAML scalar as AWS shows it.
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}

	return fmt.Sprint(v)
}

// documentParameters lists the parameters a document declares, by name.
func documentParameters(raw any) []api.DocumentParameter {
	declared, _ := raw.(map[string]any)
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}

	slices.Sort(names)

	var out []api.DocumentParameter
	for _, name := range names {
		spec, _ := declared[name].(map[string]any)
		p := api.DocumentParameter{Name: new(name), Type: optional(scalar(spec["type"]))}
		p.Description = optional(scalar(spec["description"]))
		if def, ok := spec["default"]; ok {
			p.DefaultValue = new(defaultText(def))
		}

		out = append(out, p)
	}

	return out
}

// defaultText renders a parameter's default value: text as it is and
// anything else as JSON.
func defaultText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}

	return string(b)
}

// required lists the parameters of a document that have no default.
func (c *content) required() []string {
	var out []string
	for _, p := range c.Parameters {
		if p.DefaultValue == nil {
			out = append(out, *p.Name)
		}
	}

	return out
}

// platforms returns the platform types the plugins of a command document run on.
func (c *content) platforms() []string {
	seen := map[string]bool{}
	for _, plugin := range c.Plugins {
		switch plugin {
		case "aws:runShellScript":
			seen[platformLinux], seen[platformMacOS] = true, true
		case "aws:runPowerShellScript":
			seen[platformWindows] = true
		default:
			seen[platformLinux], seen[platformMacOS], seen[platformWindows] = true, true, true
		}
	}

	var out []string
	for _, p := range []string{platformLinux, platformMacOS, platformWindows} {
		if seen[p] {
			out = append(out, p)
		}
	}

	if len(out) == 0 {
		return []string{platformLinux, platformMacOS, platformWindows}
	}

	return out
}

// convert re-encodes JSON or YAML content as format.
func convert(text, from, to string) (string, error) {
	if from == to {
		return text, nil
	}

	if from == formatText || to == formatText {
		return "", awserror.New("InvalidDocumentContent", "The document content can't be converted to "+to+".")
	}

	doc, err := decode(text, from)
	if err != nil {
		return "", err
	}

	if to == formatYAML {
		b, marshalErr := yaml.Marshal(doc)
		return string(b), marshalErr
	}

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if encodeErr := encoder.Encode(doc); encodeErr != nil {
		return "", encodeErr
	}

	return strings.TrimSpace(out.String()), nil
}
