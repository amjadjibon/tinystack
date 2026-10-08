package pipes

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// placeholder is a <...> reference in an InputTemplate.
var placeholder = regexp.MustCompile(`<([^<>]*)>`)

// jsonPath is the JSONPath subset templates use: object members and array
// indexes below the root.
var jsonPath = regexp.MustCompile(`^\$((\.[\w/-]+)|(\[\d+\]))*$`)

// variables are the <aws.pipes.*> references a template can make besides the
// event.
var variables = map[string]bool{
	"aws.pipes.event":                true,
	"aws.pipes.event.json":           true,
	"aws.pipes.event.ingestion-time": true,
	"aws.pipes.pipe-name":            true,
	"aws.pipes.pipe-arn":             true,
	"aws.pipes.source-arn":           true,
	"aws.pipes.enrichment-arn":       true,
	"aws.pipes.target-arn":           true,
}

// checkTemplate fails on a template that refers to something TinyStack does
// not evaluate.
func checkTemplate(field, template string) error {
	if len(template) > maxTemplate {
		return awserror.Validation("%s must be at most %d characters long", field, maxTemplate)
	}

	for _, match := range placeholder.FindAllStringSubmatch(template, -1) {
		ref := match[1]
		if !variables[ref] && !jsonPath.MatchString(ref) {
			return awserror.Validation("%s refers to <%s>, which TinyStack does not support", field, ref)
		}
	}

	return nil
}

// context holds the values a template's aws.pipes variables stand for.
type templateContext struct {
	pipeName, pipeARN, source, enrichment, target string
	ingestion                                     string
}

// render fills a template for one event: <$.path> takes the value at the
// path in the event and the aws.pipes variables their own. A string is
// inserted as its text and anything else as JSON, so "<$.id>" is a string
// and <$.count> a number. A missing value is null. The result is the JSON
// the text makes, or else a JSON string.
func render(template string, event json.RawMessage, c *templateContext) json.RawMessage {
	var document any
	_ = json.Unmarshal(event, &document)
	text := placeholder.ReplaceAllStringFunc(template, func(match string) string {
		ref := match[1 : len(match)-1]
		var value any
		switch ref {
		case "aws.pipes.event", "aws.pipes.event.json":
			return string(event)
		case "aws.pipes.event.ingestion-time":
			value = c.ingestion
		case "aws.pipes.pipe-name":
			value = c.pipeName
		case "aws.pipes.pipe-arn":
			value = c.pipeARN
		case "aws.pipes.source-arn":
			value = c.source
		case "aws.pipes.enrichment-arn":
			value = c.enrichment
		case "aws.pipes.target-arn":
			value = c.target
		default:
			value = lookup(document, ref)
		}

		if s, ok := value.(string); ok {
			return s
		}

		out, _ := json.Marshal(value)
		return string(out)
	})

	return asJSON(text)
}

// asJSON returns text as JSON when it is, or as a JSON string.
func asJSON(text string) json.RawMessage {
	if trimmed := strings.TrimSpace(text); json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}

	out, _ := json.Marshal(text)
	return out
}

// lookup follows a checked JSONPath in a decoded document; a path that leads
// nowhere is nil.
func lookup(document any, path string) any {
	rest := strings.TrimPrefix(path, "$")
	for rest != "" {
		// A string holding JSON, such as an SQS body, is followed into.
		if encoded, ok := document.(string); ok {
			var decoded any
			if json.Unmarshal([]byte(encoded), &decoded) != nil {
				return nil
			}

			document = decoded
		}

		switch rest[0] {
		case '.':
			name := rest[1:]
			if end := strings.IndexAny(name, ".["); end >= 0 {
				name, rest = name[:end], rest[1+end:]
			} else {
				rest = ""
			}

			object, ok := document.(map[string]any)
			if !ok {
				return nil
			}

			document = object[name]
		case '[':
			end := strings.IndexByte(rest, ']')
			index, err := strconv.Atoi(rest[1:end])
			list, ok := document.([]any)
			if err != nil || !ok || index >= len(list) {
				return nil
			}

			document, rest = list[index], rest[end+1:]
		default:
			return nil
		}
	}

	return document
}

// text is how an individual target takes an item: a JSON string as its text
// and anything else as JSON.
func text(item json.RawMessage) string {
	var s string
	if json.Unmarshal(item, &s) == nil {
		return s
	}

	return string(item)
}
