package glue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	formatAvro     = "AVRO"
	formatJSON     = "JSON"
	compatNone     = "NONE"
	compatDisabled = "DISABLED"
	compatBackward = "BACKWARD"
	compatForward  = "FORWARD"
	compatFull     = "FULL"
	schemaBoolean  = "boolean"
	schemaTypeKey  = "type"
	schemaNull     = "null"
	avroBytes      = "bytes"
	schemaString   = "string"
	avroInt        = "int"
	avroLong       = "long"
	avroFloat      = "float"
	avroDouble     = "double"
)

type avroSchema struct {
	name   string
	types  []string
	fields []avroField
}

type avroField struct {
	name       string
	types      []string
	hasDefault bool
}

var avroName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type schemaShape struct {
	avro *avroSchema
	json *jsonObjectSchema
}

func checkSchemaMode(format, mode string) error {
	if format != formatAvro && format != formatJSON {
		return invalid("TinyStack does not support schema data format %q.", format)
	}

	switch mode {
	case compatNone, compatDisabled, compatBackward, compatForward, compatFull,
		"BACKWARD_ALL", "FORWARD_ALL", "FULL_ALL":
		return nil
	default:
		return invalid("TinyStack does not support %s compatibility mode %q.", format, mode)
	}
}

func validatedSchema(format, mode, definition string) (hash string, shape schemaShape, err error) {
	if len(definition) > 170_000 || !json.Valid([]byte(definition)) {
		return "", shape, invalid("SchemaDefinition must be valid JSON of at most 170000 bytes.")
	}

	decoder := json.NewDecoder(strings.NewReader(definition))
	decoder.UseNumber()
	var value any
	if decodeErr := decoder.Decode(&value); decodeErr != nil {
		return "", shape, invalid("Invalid schema definition: %s", decodeErr)
	}

	switch format {
	case formatAvro:
		shape.avro, err = parseAvroSchema(value)
		if err != nil {
			return "", shape, err
		}
	case formatJSON:
		if validationErr := validateJSONSchema(definition, value); validationErr != nil {
			return "", shape, validationErr
		}

		if mode != compatNone && mode != compatDisabled {
			shape.json, err = parseJSONCompatibilitySchema(value)
			if err != nil {
				return "", shape, err
			}
		}
	default:
		return "", shape, invalid("TinyStack does not support schema data format %q.", format)
	}

	canonical, err := json.Marshal(value)
	if err != nil {
		return "", shape, err
	}

	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), shape, nil
}

func validateJSONSchema(definition string, value any) error {
	doc, ok := value.(map[string]any)
	if !ok {
		return invalid("TinyStack JSON schemas must be draft-07 objects.")
	}

	if draft, supplied := doc["$schema"]; supplied &&
		draft != "http://json-schema.org/draft-07/schema#" && draft != "https://json-schema.org/draft-07/schema#" {
		return invalid("TinyStack supports only draft-07 JSON schemas.")
	}

	var schema jsonschema.Schema
	if err := json.Unmarshal([]byte(definition), &schema); err != nil {
		return invalid("Invalid JSON schema: %s", err)
	}

	if schema.Schema == "" {
		schema.Schema = "http://json-schema.org/draft-07/schema#"
	}

	if _, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true}); err != nil {
		return invalid("Invalid or unsupported JSON schema: %s", err)
	}

	return nil
}

func parseAvroSchema(value any) (*avroSchema, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		types, err := avroTypes(value)
		return &avroSchema{types: types}, err
	}

	if obj[schemaTypeKey] != "record" {
		return nil, invalid("TinyStack AVRO supports primitive types, primitive unions and flat records.")
	}

	if _, supplied := obj["aliases"]; supplied {
		return nil, invalid("TinyStack AVRO does not support record aliases.")
	}

	n, ok := obj["name"].(string)
	if !ok || !validAvroDottedName(n) {
		return nil, invalid("Invalid AVRO record name.")
	}

	if namespace, supplied := obj["namespace"]; supplied {
		ns, isString := namespace.(string)
		if !isString || ns != "" && !validAvroDottedName(ns) {
			return nil, invalid("Invalid AVRO namespace.")
		}
	}

	// Avro resolves record names by their unqualified name.
	if dot := strings.LastIndexByte(n, '.'); dot >= 0 {
		n = n[dot+1:]
	}

	items, ok := obj["fields"].([]any)
	if !ok {
		return nil, invalid("AVRO record fields must be an array.")
	}

	out := &avroSchema{name: n}
	names := make(map[string]bool, len(items))
	for _, item := range items {
		field, ok := item.(map[string]any)
		if !ok {
			return nil, invalid("Invalid AVRO field.")
		}

		name, ok := field["name"].(string)
		if !ok || !avroName.MatchString(name) || names[name] {
			return nil, invalid("Invalid or duplicate AVRO field name %q.", name)
		}

		if _, ok := field["aliases"]; ok {
			return nil, invalid("TinyStack AVRO does not support field aliases.")
		}

		types, err := avroTypes(field[schemaTypeKey])
		if err != nil {
			return nil, err
		}

		def, hasDefault := field["default"]
		if hasDefault && !validAvroDefault(types[0], def) {
			return nil, invalid("AVRO default for field %q must match type %s.", name, types[0])
		}

		names[name] = true
		out.fields = append(out.fields, avroField{name: name, types: types, hasDefault: hasDefault})
	}

	return out, nil
}

func validAvroDottedName(n string) bool {
	for part := range strings.SplitSeq(n, ".") {
		if !avroName.MatchString(part) {
			return false
		}
	}

	return true
}

func avroTypes(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		items = []any{value}
	}

	types := make([]string, 0, len(items))
	for _, item := range items {
		n, _ := item.(string)
		switch n {
		case schemaNull, schemaBoolean, avroInt, avroLong, avroFloat, avroDouble, avroBytes, schemaString:
		default:
			return nil, invalid("TinyStack AVRO fields and unions support only primitive types.")
		}

		if slices.Contains(types, n) {
			return nil, invalid("Duplicate AVRO union type %q.", n)
		}

		types = append(types, n)
	}

	if len(types) == 0 {
		return nil, invalid("AVRO unions cannot be empty.")
	}

	return types, nil
}

func validAvroDefault(kind string, value any) bool {
	switch kind {
	case schemaNull:
		return value == nil
	case schemaBoolean:
		_, ok := value.(bool)
		return ok
	case schemaString:
		_, ok := value.(string)
		return ok
	case avroBytes:
		str, ok := value.(string)
		return ok && strings.IndexFunc(str, func(r rune) bool { return r > 255 }) < 0
	}

	number, ok := value.(json.Number)
	if !ok {
		return false
	}

	switch kind {
	case avroInt:
		_, err := strconv.ParseInt(string(number), 10, 32)
		return err == nil
	case avroLong:
		_, err := number.Int64()
		return err == nil
	case avroFloat:
		_, err := strconv.ParseFloat(string(number), 32)
		return err == nil
	default:
		_, err := number.Float64()
		return err == nil
	}
}

func avroReadsTypes(reader, writer []string) bool {
	for _, written := range writer {
		if !slices.ContainsFunc(reader, func(read string) bool {
			return read == written ||
				written == avroInt && (read == avroLong || read == avroFloat || read == avroDouble) ||
				written == avroLong && (read == avroFloat || read == avroDouble) ||
				written == avroFloat && read == avroDouble ||
				written == schemaString && read == avroBytes || written == avroBytes && read == schemaString
		}) {
			return false
		}
	}

	return true
}

func avroReads(reader, writer *avroSchema) bool {
	if reader.name != writer.name {
		return false
	}

	if reader.name == "" {
		return avroReadsTypes(reader.types, writer.types)
	}

	written := make(map[string][]string, len(writer.fields))
	for _, field := range writer.fields {
		written[field.name] = field.types
	}

	for _, field := range reader.fields {
		types, found := written[field.name]
		if (!found && !field.hasDefault) || (found && !avroReadsTypes(field.types, types)) {
			return false
		}
	}

	return true
}

func (reader schemaShape) reads(writer schemaShape) bool {
	if reader.avro != nil {
		return avroReads(reader.avro, writer.avro)
	}

	return reader.json.reads(writer.json)
}

func checkSchemaEvolution(
	tx storage.ReadTx, scope request.Scope, n string, schema *schemaRecord, candidate schemaShape,
) error {
	mode := *schema.Compatibility
	if mode == compatNone || schema.LatestSchemaVersion == nil {
		return nil
	}

	if mode == compatDisabled {
		return invalid("This schema uses DISABLED compatibility and cannot add versions.")
	}

	first, last := *schema.LatestSchemaVersion, *schema.LatestSchemaVersion
	if strings.HasSuffix(mode, "_ALL") {
		first = *schema.SchemaCheckpoint
		mode = strings.TrimSuffix(mode, "_ALL")
	}

	for i := first; i <= last; i++ {
		version, _, err := storage.Read[schemaVersionRecord](tx, schemaVersionKey(scope, n, i))
		if err != nil {
			return err
		}

		_, previous, err := validatedSchema(*schema.DataFormat, mode, *version.SchemaDefinition)
		if err != nil {
			return err
		}

		if (mode == compatBackward || mode == compatFull) && !candidate.reads(previous) ||
			(mode == compatForward || mode == compatFull) && !previous.reads(candidate) {
			return invalid("Schema definition is incompatible with version %d under %s.", i, *schema.Compatibility)
		}
	}

	return nil
}
