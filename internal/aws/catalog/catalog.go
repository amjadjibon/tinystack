package catalog

import (
	"context"
	"slices"
)

const (
	AWSJSON10 = "awsJson1_0"
	AWSJSON11 = "awsJson1_1"
	AWSQuery  = "awsQuery"
	RESTXML   = "restXml"
	RESTJSON  = "restJson1"
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
// the modeled message member name REST JSON error bodies use ("message" or
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
	// UnitOutput marks operations whose modeled output is smithy.api#Unit.
	// AWS Query omits their Result element but writes one, even empty, for
	// every structure output.
	UnitOutput bool
}

type Service struct {
	Operations      map[string]Operation
	Errors          map[string]ErrorBinding
	Name            string
	SigningName     string
	TargetPrefix    string
	XMLNamespace    string
	Protocols       []string
	QueryCompatible bool
}

func (s *Service) Supports(protocol string) bool {
	return slices.Contains(s.Protocols, protocol)
}
