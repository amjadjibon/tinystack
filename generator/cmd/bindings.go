package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// compileBinding retains wire metadata independently of runtime codec support.
// fields selects the generated members of a structure.
func compileBinding(
	m model,
	id, protocol, targetPrefix, version string,
	fields func(string) []string,
) (catalog.Binding, error) {
	op := m.Shapes[id]
	contentType := "application/json"
	switch protocol {
	case catalog.AWSQuery:
		return catalog.Binding{
			Method:      "POST",
			URI:         "/",
			Target:      short(id),
			Version:     version,
			ContentType: "application/x-www-form-urlencoded",
			StatusCode:  200,
		}, nil
	case catalog.AWSJSON10:
		return catalog.Binding{
			Method:      "POST",
			URI:         "/",
			Target:      targetPrefix + "." + short(id),
			ContentType: "application/x-amz-json-1.0",
			StatusCode:  200,
		}, nil
	case catalog.RESTJSON:
	case catalog.RESTXML:
		contentType = "application/xml"
	default:
		return catalog.Binding{}, fmt.Errorf("unsupported protocol %s", protocol)
	}

	var httpTrait struct {
		Method, URI string
		Code        int
	}

	raw, ok := op.Traits["smithy.api#http"]
	if !ok {
		return catalog.Binding{}, fmt.Errorf("REST operation missing http trait")
	}

	if guardErr := json.Unmarshal(raw, &httpTrait); guardErr != nil {
		return catalog.Binding{}, guardErr
	}
	// Smithy defaults the success code to 200.
	if httpTrait.Code == 0 {
		httpTrait.Code = 200
	}

	if httpTrait.Method == "" || httpTrait.URI == "" || httpTrait.Code < 100 {
		return catalog.Binding{}, fmt.Errorf("invalid HTTP trait")
	}

	in, err := memberBindings(m.Shapes[op.Input.Target].Members, fields(op.Input.Target), true)
	if err != nil {
		return catalog.Binding{}, err
	}

	out, err := memberBindings(m.Shapes[op.Output.Target].Members, fields(op.Output.Target), false)
	if err != nil {
		return catalog.Binding{}, err
	}

	return catalog.Binding{
		Method:      httpTrait.Method,
		URI:         httpTrait.URI,
		StatusCode:  httpTrait.Code,
		ContentType: contentType,
		Input:       in,
		Output:      out,
	}, nil
}

const (
	locHeader = "header"
	locQuery  = "query"
	locPrefix = "prefix"
)

// memberBindings locates the selected members. For inputs, modeled header and
// query members the profile leaves out are recorded as unsupported, so a
// request that sets them fails instead of being silently ignored.
func memberBindings(
	members map[string]reference,
	selected []string,
	input bool,
) (map[string]catalog.MemberBinding, error) {
	bindings := map[string]catalog.MemberBinding{}
	for name, member := range members {
		binding := catalog.MemberBinding{Location: "document", Name: name}
		bound := false
		for trait, raw := range member.Traits {
			location := ""
			switch trait {
			case "smithy.api#httpLabel":
				location = "label"
			case "smithy.api#httpHeader":
				location = locHeader
			case "smithy.api#httpQuery":
				location = locQuery
			case "smithy.api#httpPayload":
				location = "payload"
			case "smithy.api#httpPrefixHeaders":
				location = locPrefix
			case "smithy.api#httpResponseCode":
				if input {
					return nil, fmt.Errorf("httpResponseCode on input member %s", name)
				}

				location = "status"
			default:
				if strings.HasPrefix(trait, "smithy.api#http") && slices.Contains(selected, name) {
					return nil, fmt.Errorf("unsupported HTTP binding %s", trait)
				}
			}

			if location == "" {
				continue
			}

			if bound {
				return nil, fmt.Errorf("conflicting bindings on %s", name)
			}

			bound = true
			binding.Location = location
			if location == locHeader || location == locQuery || location == locPrefix {
				if guardErr := json.Unmarshal(raw, &binding.Name); guardErr != nil {
					return nil, guardErr
				}
			}
		}

		switch {
		case slices.Contains(selected, name):
			bindings[name] = binding
		case input && (binding.Location == locHeader || binding.Location == locQuery || binding.Location == locPrefix):
			binding.Location = "unsupported:" + binding.Location
			bindings[name] = binding
		}
	}

	return bindings, nil
}
