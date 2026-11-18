package apigateway

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/openapi"
)

func (s *Service) importResponses(
	ctx context.Context,
	in *api.PutIntegrationRequest,
	op openapi.Operation,
	ext openapi.Object,
) error {
	var methods map[string]struct {
		Headers map[string]openapi.Object `json:"headers"`
	}
	if err := openapi.Decode(op.Body["responses"], &methods); err != nil {
		return err
	}

	for _, code := range slices.Sorted(maps.Keys(methods)) {
		if code == openAPIDefault {
			continue
		}

		parameters := map[string]bool{}
		for k, header := range methods[code].Headers {
			parameters[methodHeader+k], _ = header[openAPIRequired].(bool)
		}

		_, err := s.PutMethodResponse(ctx, &api.PutMethodResponseRequest{
			RestApiId: in.RestApiId, ResourceId: in.ResourceId, HttpMethod: in.HttpMethod,
			StatusCode: new(code), ResponseParameters: parameters,
		})
		if err != nil {
			return err
		}
	}

	var responses map[string]openapi.Object
	if err := openapi.Decode(ext["responses"], &responses); err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, pattern := range slices.Sorted(maps.Keys(responses)) {
		response := responses[pattern]
		if err := openapi.Check(
			response,
			"statusCode",
			"responseParameters",
			"responseTemplates",
			"contentHandling",
		); err != nil {
			return err
		}

		ir := &api.PutIntegrationResponseRequest{
			RestApiId:  in.RestApiId,
			ResourceId: in.ResourceId,
			HttpMethod: in.HttpMethod,
		}
		if err := openapi.Decode(response, ir); err != nil {
			return err
		}

		code := openapi.Text(response["statusCode"])
		if !statusCode.MatchString(code) {
			return badRequest("Invalid statusCode in OpenAPI integration response %q", pattern)
		}

		ir.StatusCode = new(code)
		if seen[code] {
			return badRequest("Multiple integration responses for status %q are unsupported", code)
		}

		seen[code] = true
		if pattern != openAPIDefault {
			if pattern == "" {
				return badRequest("Use default for an integration response without a selection pattern")
			}

			ir.SelectionPattern = new(pattern)
		}

		if _, err := s.PutIntegrationResponse(ctx, ir); err != nil {
			return err
		}
	}

	return nil
}

func exportMethodResponses(methods map[string]api.MethodResponse, swagger bool) openapi.Object {
	responses := openapi.Object{}
	for code, method := range methods {
		n, _ := strconv.Atoi(code)
		resp := openapi.Object{openAPIDescription: http.StatusText(n)}
		headers := openapi.Object{}
		for param, required := range method.ResponseParameters {
			if name, ok := strings.CutPrefix(param, methodHeader); ok {
				if swagger {
					headers[name] = openapi.Object{openAPIType: openAPIString}
				} else {
					headers[name] = openapi.Object{
						openAPISchema:   openapi.Object{openAPIType: openAPIString},
						openAPIRequired: required,
					}
				}
			}
		}

		if len(headers) > 0 {
			resp["headers"] = headers
		}

		responses[code] = resp
	}

	if len(responses) == 0 {
		responses["200"] = openapi.Object{openAPIDescription: "Success"}
	}

	return responses
}

func exportIntegrationResponses(i *api.Integration) (openapi.Object, error) {
	responses := openapi.Object{}
	for _, r := range i.IntegrationResponses {
		key := common.Value(r.SelectionPattern)
		switch key {
		case "":
			key = openAPIDefault
		case openAPIDefault:
			key = "(?:default)"
		}

		if _, exists := responses[key]; exists {
			return nil, badRequest("Multiple integration responses with selection pattern %q cannot be exported", key)
		}

		ext := openapi.Object{}
		if err := openapi.Decode(r, &ext); err != nil {
			return nil, err
		}

		delete(ext, "selectionPattern")
		responses[key] = ext
	}

	return responses, nil
}
