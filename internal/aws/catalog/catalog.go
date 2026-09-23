package catalog

import "context"

const AWSJSON10 = "awsJson1_0"

type Binding struct {
	Input       map[string]MemberBinding
	Output      map[string]MemberBinding
	Method      string
	URI         string
	Target      string
	ContentType string
	StatusCode  int
}

type MemberBinding struct{ Location, Name string }

type Operation struct {
	Bindings map[string]Binding
	NewInput func() any
	Invoke   func(context.Context, any) (any, error)
	Name     string
}

type Service struct {
	Operations   map[string]Operation
	Name         string
	SigningName  string
	TargetPrefix string
	Protocols    []string
}
