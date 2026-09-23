package catalog

import (
	"context"
	"slices"
)

const (
	AWSJSON10 = "awsJson1_0"
	AWSQuery  = "awsQuery"
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

type MemberBinding struct{ Location, Name string }

// ErrorBinding carries modeled wire facts for one error code.
type ErrorBinding struct {
	QueryCode  string
	StatusCode int
}

type Operation struct {
	Bindings map[string]Binding
	NewInput func() any
	Invoke   func(context.Context, any) (any, error)
	Name     string
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
