package glue

import (
	"errors"
	"fmt"
	"slices"
)

type jsonObjectSchema struct {
	properties map[string]string
	required   []string
	open       bool
}

// parseJSONCompatibilitySchema supports flat object schemas without value constraints.
// For other valid draft-07 schemas callers can choose NONE or DISABLED.
func parseJSONCompatibilitySchema(value any) (*jsonObjectSchema, error) {
	doc, ok := value.(map[string]any)
	if !ok || doc[schemaTypeKey] != "object" {
		return nil, invalid("TinyStack JSON compatibility checks require a flat object schema.")
	}

	for key := range doc {
		switch key {
		case schemaTypeKey, "properties", "required", "additionalProperties", "$schema", "$id", "$comment",
			"title", "description", "default", "examples":
		default:
			return nil, invalid("TinyStack JSON compatibility checks do not support root keyword %q.", key)
		}
	}

	out := &jsonObjectSchema{open: true, properties: map[string]string{}}
	if raw, exists := doc["additionalProperties"]; exists {
		var ok bool
		out.open, ok = raw.(bool)
		if !ok {
			return nil, invalid("TinyStack JSON compatibility checks require boolean additionalProperties.")
		}
	}

	if raw, exists := doc["properties"]; exists {
		properties, ok := raw.(map[string]any)
		if !ok {
			return nil, invalid("JSON schema properties must be an object.")
		}

		for name, property := range properties {
			kind, err := jsonPropertyType(property)
			if err != nil {
				return nil, invalid("JSON property %q: %s", name, err)
			}

			out.properties[name] = kind
		}
	}

	if raw, exists := doc["required"]; exists {
		required, ok := raw.([]any)
		if !ok {
			return nil, invalid("JSON schema required must be an array.")
		}

		for _, item := range required {
			name, ok := item.(string)
			if _, found := out.properties[name]; !ok || !found {
				return nil, invalid(
					"TinyStack JSON compatibility checks require each required property to be declared.",
				)
			}

			out.required = append(out.required, name)
		}
	}

	return out, nil
}

func jsonPropertyType(property any) (string, error) {
	if property == true {
		return "", nil
	}

	doc, ok := property.(map[string]any)
	if !ok {
		return "", errors.New("TinyStack JSON compatibility checks require an unconstrained or single-type property")
	}

	for key := range doc {
		switch key {
		case schemaTypeKey, "$comment", "title", "description", "default", "examples":
		default:
			return "", fmt.Errorf("TinyStack JSON compatibility checks do not support keyword %q", key)
		}
	}

	if _, exists := doc[schemaTypeKey]; !exists {
		return "", nil
	}

	kind, _ := doc[schemaTypeKey].(string)
	switch kind {
	case "array", "object", schemaBoolean, "integer", schemaNull, "number", schemaString:
		return kind, nil
	default:
		return "", errors.New("TinyStack JSON compatibility checks require one JSON type per property")
	}
}

func (reader *jsonObjectSchema) reads(writer *jsonObjectSchema) bool {
	if writer.open && !reader.open {
		return false
	}

	for _, name := range reader.required {
		if !slices.Contains(writer.required, name) {
			return false
		}
	}

	for name, from := range writer.properties {
		if to, declared := reader.properties[name]; declared {
			if to != "" && to != from && (from != "integer" || to != "number") {
				return false
			}
		} else if !reader.open {
			return false
		}
	}

	if writer.open {
		for name, to := range reader.properties {
			if _, declared := writer.properties[name]; !declared && to != "" {
				return false
			}
		}
	}

	return true
}
