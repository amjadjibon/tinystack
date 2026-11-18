package apigateway

import (
	"encoding/json"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/common"
)

// validateRequest applies the method's request validator, if it has one, after authorization and before the
// integration, as AWS does: required parameters must be present and not blank, and a body whose content type has a
// model (or a $default model) must match its schema.
func validateRequest(c *call) *execute.Response {
	v := c.snapshot.Validators[common.Value(c.method.RequestValidatorId)]
	if v == nil {
		return nil
	}

	if common.Value(v.ValidateRequestParameters) {
		if missing := missingParameters(c); len(missing) > 0 {
			c.validationError = "Missing required request parameters: [" + strings.Join(missing, ", ") + "]"
			return badRequestResponse(c.validationError)
		}
	}

	if !common.Value(v.ValidateRequestBody) {
		return nil
	}

	// A request without a content type is treated as JSON.
	contentType := mediaJSON
	if header := c.req.Header.Get("Content-Type"); header != "" {
		if parsed, _, err := mime.ParseMediaType(header); err == nil {
			contentType = parsed
		}
	}

	name, ok := c.method.RequestModels[contentType]
	if !ok {
		if name, ok = c.method.RequestModels["$default"]; !ok {
			return nil
		}
	}

	schema, err := compileModel(*c.api.API.Id, c.snapshot.Models, name)
	if err != nil {
		c.validationError = "Invalid model " + name + ": " + err.Error()
		return execute.Message(http.StatusInternalServerError, "Internal server error")
	}

	var body any
	if err = json.Unmarshal(c.req.Body, &body); err == nil {
		err = schema.Validate(body)
	}

	if err != nil {
		c.validationError = err.Error()
		return badRequestResponse("Invalid request body")
	}

	return nil
}

// missingParameters lists the method's required request parameters that the request lacks or leaves blank.
func missingParameters(c *call) []string {
	var missing []string
	for _, key := range slices.Sorted(maps.Keys(c.method.RequestParameters)) {
		if !c.method.RequestParameters[key] {
			continue
		}

		var value string
		switch location, name, _ := strings.Cut(strings.TrimPrefix(key, "method.request."), "."); location {
		case locationQuery:
			value = c.req.Query.Get(name)
		case "header":
			value = c.req.Header.Get(name)
		case locationPath:
			value = c.params[name]
		default:
			continue
		}

		if strings.TrimSpace(value) == "" {
			missing = append(missing, key[strings.LastIndex(key, ".")+1:])
		}
	}

	return missing
}

func badRequestResponse(message string) *execute.Response {
	resp := execute.Message(http.StatusBadRequest, message)
	resp.Header.Set("X-Amzn-Errortype", "BadRequestException")
	return resp
}
