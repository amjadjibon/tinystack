package catalog

import (
	"context"
	"fmt"
	"slices"
)

const (
	AWSJSON10 = "awsJson1_0"
	AWSJSON11 = "awsJson1_1"
	AWSQuery  = "awsQuery"
	EC2Query  = "ec2Query"
	RESTXML   = "restXml"
	RESTJSON  = "restJson1"
	// RPCV2CBOR is Smithy's RPC v2 CBOR protocol.
	RPCV2CBOR = "rpcv2Cbor"
)

type Binding struct {
	Input       map[string]MemberBinding
	Output      map[string]MemberBinding
	Method      string
	URI         string
	Target      string
	Version     string
	ContentType string
	StatusCode  int
}

// MemberBinding places a member in a REST message: "label", "header", "query",
// "prefix" (httpPrefixHeaders), "payload", "status" (the response status code,
// httpResponseCode), or "document" for body members.
// "unsupported:header", "unsupported:query", and "unsupported:prefix" mark
// modeled inputs the profile does not implement; requests that set them must
// fail instead of being ignored.
type MemberBinding struct{ Location, Name string }

// ErrorBinding carries modeled wire facts for one error code. MessageMember is
// the modeled message member name JSON error bodies use ("message" or
// "Message").
type ErrorBinding struct {
	QueryCode     string
	MessageMember string
	StatusCode    int
}

type Operation struct {
	Bindings map[string]Binding
	NewInput func() any
	Invoke   func(context.Context, any) (any, error)
	Name     string
	// IAMAction is generated from model IAM traits and cited API overrides.
	IAMAction string
	// UnitOutput marks operations whose modeled output is smithy.api#Unit.
	// AWS Query omits their Result element but writes one, even empty, for
	// every structure output.
	UnitOutput bool
	// Sensitive marks operations whose input or output carries a value the
	// model marks smithy.api#sensitive, such as a secret; the request
	// inspector does not keep their bodies.
	Sensitive bool
	// Anonymous marks operations the model lets clients call unsigned
	// (smithy.api#optionalAuth or an empty smithy.api#auth), such as Cognito's
	// sign-in operations, which identify their caller by a client ID or token.
	Anonymous bool
}

// Resource is one resource a service holds, as the resource browser shows it.
type Resource struct {
	Details any               `json:"details,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
	Type    string            `json:"type"`
	Account string            `json:"account"`
	Region  string            `json:"region"`
	Name    string            `json:"name"`
	ARN     string            `json:"arn"`
}

// Inventory is implemented by services whose resources the resource browser
// lists, across every account and region.
type Inventory interface {
	Resources(context.Context) ([]Resource, error)
}

// Owned is a resource labeled with the name of the service that holds it.
type Owned struct {
	Service string `json:"service"`
	Resource
}

// Resources lists the resources of every service that has an Inventory, in
// every account and region.
func Resources(ctx context.Context, services map[string]*Service) ([]Owned, error) {
	var out []Owned
	for name, service := range services {
		if service.Inventory == nil {
			continue
		}

		resources, err := service.Inventory.Resources(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		for i := range resources {
			out = append(out, Owned{Service: name, Resource: resources[i]})
		}
	}

	return out, nil
}

// InventoryOf returns api's Inventory, or nil if it has none.
func InventoryOf(api any) Inventory {
	inventory, _ := api.(Inventory)
	return inventory
}

type Service struct {
	// Inventory lists the service's resources; it may be nil.
	Authorizer      Authorizer
	IAMPrefix       string
	Inventory       Inventory
	Operations      map[string]Operation
	Errors          map[string]ErrorBinding
	Name            string
	SigningName     string
	TargetPrefix    string
	XMLNamespace    string
	Protocols       []string
	QueryCompatible bool
	// WrappedErrors marks REST XML services whose error bodies are wrapped in
	// an ErrorResponse element; S3's are not.
	WrappedErrors bool
}

func (s *Service) Supports(protocol string) bool {
	return slices.Contains(s.Protocols, protocol)
}
