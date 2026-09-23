package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

const (
	shapeUnion = "union"
	shapeList  = "list"
	shapeMap   = "map"
	unitShape  = "smithy.api#Unit"
)

type reference struct {
	Traits map[string]json.RawMessage
	Target string
}

type shape struct {
	Member  reference
	Key     reference
	Value   reference
	Input   reference
	Output  reference
	Members map[string]reference
	Traits  map[string]json.RawMessage
	Type    string
	Version string
	Errors  []reference
}

// serviceProfile selects what is generated for one pinned service model.
type serviceProfile struct {
	Shapes     map[string][]string `json:"shapes"`
	Query      *queryOverlay       `json:"query"`
	Operations []string            `json:"operations"`
}

// queryOverlay supplies legacy awsQuery facts that awsQueryCompatible models no
// longer carry. Pure awsQuery models declare their namespace themselves.
type queryOverlay struct {
	XMLNamespace string `json:"xmlNamespace"`
}

type (
	model     struct{ Shapes map[string]shape }
	generator struct {
		model   model
		profile serviceProfile
		seen    map[string]bool
		out     bytes.Buffer
		query   bool
	}
)

func short(s string) string { _, b, _ := strings.Cut(s, "#"); return b }
func (g *generator) fields(id string) []string {
	if p, ok := g.profile.Shapes[short(id)]; ok {
		return p
	}

	keys := make([]string, 0, len(g.model.Shapes[id].Members))
	for k := range g.model.Shapes[id].Members {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	return keys
}

func (g *generator) typeName(id string) (string, error) {
	s, ok := g.model.Shapes[id]
	if !ok {
		return "", fmt.Errorf("missing shape %s", id)
	}

	switch s.Type {
	case "string", "enum":
		return "string", nil
	case "boolean":
		return "bool", nil
	case "integer", "long":
		return "int64", nil
	case "double", "float", "timestamp":
		return "float64", nil
	case "blob":
		return "[]byte", nil
	case "structure", shapeUnion:
		return short(id), nil
	case shapeList:
		t, e := g.typeName(s.Member.Target)
		return "[]" + t, e
	case shapeMap:
		t, e := g.typeName(s.Value.Target)
		return "map[string]" + t, e
	default:
		return "", fmt.Errorf("unsupported shape %s: %s", id, s.Type)
	}
}

func checkTraits(traits map[string]json.RawMessage) error {
	for t := range traits {
		if strings.HasPrefix(t, "smithy.api#http") || t == "smithy.api#streaming" ||
			t == "smithy.api#timestampFormat" ||
			t == "smithy.api#jsonName" ||
			t == "smithy.api#sparse" ||
			t == "smithy.api#mediaType" ||
			t == "smithy.api#xmlAttribute" ||
			t == "smithy.api#xmlNamespace" {
			return fmt.Errorf("unsupported wire trait %s", t)
		}
	}

	return nil
}

func xmlName(traits map[string]json.RawMessage, fallback string) (string, error) {
	raw, ok := traits["smithy.api#xmlName"]
	if !ok {
		return fallback, nil
	}

	var name string
	if err := json.Unmarshal(raw, &name); err != nil || name == "" {
		return "", fmt.Errorf("invalid xmlName %s", raw)
	}

	return name, nil
}

// queryTag records the awsQuery key and XML element names used by the query codec.
func (g *generator) queryTag(member reference, name string) (string, error) {
	wire, err := xmlName(member.Traits, name)
	if err != nil {
		return "", err
	}

	opts := []string{wire}
	_, flat := member.Traits["smithy.api#xmlFlattened"]
	if flat {
		opts = append(opts, "flat")
	}

	target := g.model.Shapes[member.Target]
	nested := func(id string) bool {
		kind := g.model.Shapes[id].Type
		return kind == shapeList || kind == shapeMap
	}

	switch target.Type {
	case "timestamp":
		return "", fmt.Errorf("timestamps are not supported by the query codec")
	case shapeList:
		if nested(target.Member.Target) {
			return "", fmt.Errorf("nested collections are not supported by the query codec")
		}

		memberName, nameErr := xmlName(target.Member.Traits, "member")
		if nameErr != nil {
			return "", nameErr
		}

		if !flat && memberName != "member" {
			opts = append(opts, "member="+memberName)
		}
	case shapeMap:
		if nested(target.Value.Target) {
			return "", fmt.Errorf("nested collections are not supported by the query codec")
		}

		for _, entry := range []struct {
			ref          reference
			option, name string
		}{{target.Key, "key", "key"}, {target.Value, "value", "value"}} {
			entryName, nameErr := xmlName(entry.ref.Traits, entry.name)
			if nameErr != nil {
				return "", nameErr
			}

			if entryName != entry.name {
				opts = append(opts, entry.option+"="+entryName)
			}
		}
	}

	return ` query:"` + strings.Join(opts, ",") + `"`, nil
}

func (g *generator) emit(id string) error {
	if g.seen[id] {
		return nil
	}

	g.seen[id] = true
	s := g.model.Shapes[id]
	if guardErr := checkTraits(s.Traits); guardErr != nil {
		return fmt.Errorf("%s: %w", id, guardErr)
	}

	switch s.Type {
	case shapeList:
		if guardErr := checkTraits(s.Member.Traits); guardErr != nil {
			return fmt.Errorf("%s: %w", id, guardErr)
		}

		return g.emit(s.Member.Target)
	case shapeMap:
		for _, ref := range []reference{s.Key, s.Value} {
			if guardErr := checkTraits(ref.Traits); guardErr != nil {
				return fmt.Errorf("%s: %w", id, guardErr)
			}
		}

		return g.emit(s.Value.Target)
	case "structure", shapeUnion:
		fields := g.fields(id)
		for _, n := range fields {
			m, ok := s.Members[n]
			if !ok {
				return fmt.Errorf("unknown field %s.%s", id, n)
			}

			if guardErr := checkTraits(m.Traits); guardErr != nil {
				return guardErr
			}

			if guardErr := g.emit(m.Target); guardErr != nil {
				return guardErr
			}
		}

		fmt.Fprintf(&g.out, "type %s struct {\n", short(id))
		for _, n := range fields {
			m := s.Members[n]
			t, err := g.typeName(m.Target)
			if err != nil {
				return err
			}

			kind := g.model.Shapes[m.Target].Type
			if s.Type == shapeUnion || (kind != shapeList && kind != shapeMap && kind != "blob") {
				t = "*" + t
			}

			tag := n + ",omitempty"
			if short(id) == "ListTablesOutput" && n == "TableNames" {
				tag = n
			}

			if short(id) == "ScanOutput" && n == "Items" {
				t = "*" + t
			}

			queryTag := ""
			if g.query {
				if queryTag, err = g.queryTag(m, n); err != nil {
					return fmt.Errorf("%s.%s: %w", id, n, err)
				}
			}

			fmt.Fprintf(&g.out, "%s %s `json:\"%s\"%s`\n", n, t, tag, queryTag)
		}

		fmt.Fprintln(&g.out, "}")
		fmt.Fprintln(&g.out)
	}

	return nil
}

// errorBindings describes every modeled error of the enabled operations. Query
// codes are recorded for services that speak AWS Query.
func (g *generator) errorBindings(
	ops []string,
	prefix string,
	query bool,
) (map[string]catalog.ErrorBinding, error) {
	bindings := map[string]catalog.ErrorBinding{}
	for _, op := range ops {
		for _, ref := range g.model.Shapes[prefix+op].Errors {
			traits := g.model.Shapes[ref.Target].Traits
			var fault string
			if guardErr := json.Unmarshal(traits["smithy.api#error"], &fault); guardErr != nil {
				return nil, fmt.Errorf("%s: invalid error trait: %w", ref.Target, guardErr)
			}

			binding := catalog.ErrorBinding{StatusCode: 400}
			if fault == "server" {
				binding.StatusCode = 500
			}

			var queryError struct {
				Code             string `json:"code"`
				HTTPResponseCode int    `json:"httpResponseCode"`
			}

			if raw, ok := traits["aws.protocols#awsQueryError"]; ok {
				if guardErr := json.Unmarshal(raw, &queryError); guardErr != nil {
					return nil, fmt.Errorf("%s: invalid awsQueryError trait: %w", ref.Target, guardErr)
				}
			}

			if queryError.HTTPResponseCode != 0 {
				binding.StatusCode = queryError.HTTPResponseCode
			}

			if raw, ok := traits["smithy.api#httpError"]; ok {
				if guardErr := json.Unmarshal(raw, &binding.StatusCode); guardErr != nil {
					return nil, fmt.Errorf("%s: invalid httpError trait: %w", ref.Target, guardErr)
				}
			}

			if query {
				binding.QueryCode = short(ref.Target)
				if queryError.Code != "" {
					binding.QueryCode = queryError.Code
				}
			}

			bindings[short(ref.Target)] = binding
		}
	}

	return bindings, nil
}

// protocols selects the wire protocols of the service. AWS JSON 1.0 services
// add AWS Query when they are awsQueryCompatible and the profile supplies the
// legacy XML namespace; pure awsQuery services take it from the model.
func (g *generator) protocols(svc *shape) (protocols []string, namespace string, queryCompatible bool, err error) {
	_, jsonProtocol := svc.Traits["aws.protocols#awsJson1_0"]
	_, queryProtocol := svc.Traits["aws.protocols#awsQuery"]
	_, queryCompatible = svc.Traits["aws.protocols#awsQueryCompatible"]
	switch {
	case jsonProtocol && g.query:
		if !queryCompatible || svc.Version == "" || g.profile.Query.XMLNamespace == "" {
			return nil, "", false, fmt.Errorf(
				"query overlay needs an awsQueryCompatible model, API version, and XML namespace",
			)
		}

		return []string{catalog.AWSJSON10, catalog.AWSQuery}, g.profile.Query.XMLNamespace, true, nil
	case jsonProtocol:
		return []string{catalog.AWSJSON10}, "", queryCompatible, nil
	case queryProtocol:
		var ns struct{ URI string }
		if guardErr := json.Unmarshal(svc.Traits["smithy.api#xmlNamespace"], &ns); guardErr != nil || ns.URI == "" ||
			svc.Version == "" {
			return nil, "", false, fmt.Errorf("awsQuery model needs an API version and XML namespace")
		}

		if g.query {
			return nil, "", false, fmt.Errorf("awsQuery models declare their own namespace; remove the query overlay")
		}

		g.query = true
		return []string{catalog.AWSQuery}, ns.URI, false, nil
	default:
		return nil, "", false, fmt.Errorf("model is neither AWS JSON 1.0 nor AWS Query")
	}
}

// service returns the model's only service shape.
func (g *generator) service() (string, shape, error) {
	id := ""
	for shapeID := range g.model.Shapes {
		if g.model.Shapes[shapeID].Type == "service" {
			if id != "" {
				return "", shape{}, fmt.Errorf("model defines more than one service")
			}

			id = shapeID
		}
	}

	if id == "" {
		return "", shape{}, fmt.Errorf("model defines no service")
	}

	return id, g.model.Shapes[id], nil
}

func generate(name string, data []byte, profile serviceProfile) ([]byte, error) {
	g := generator{seen: map[string]bool{}, profile: profile, query: profile.Query != nil}
	if guardErr := json.Unmarshal(data, &g.model); guardErr != nil {
		return nil, guardErr
	}

	serviceID, svc, err := g.service()
	if err != nil {
		return nil, err
	}

	protocols, namespace, queryCompatible, err := g.protocols(&svc)
	if err != nil {
		return nil, err
	}

	var sigv4 struct{ Name string }
	if guardErr := json.Unmarshal(svc.Traits["aws.auth#sigv4"], &sigv4); guardErr != nil || sigv4.Name == "" {
		return nil, fmt.Errorf("model has no SigV4 signing name")
	}

	prefix, targetPrefix, _ := strings.Cut(serviceID, "#")
	prefix += "#"
	if !slices.Contains(protocols, catalog.AWSJSON10) {
		targetPrefix = ""
	}

	if len(profile.Operations) == 0 {
		return nil, fmt.Errorf("profile selects no operations")
	}

	fmt.Fprintln(
		&g.out,
		"// Code generated by generator/cmd; DO NOT EDIT.\npackage "+name+"\n\nimport ("+
			"\"context\"; \"github.com/amjadjibon/tinystack/internal/aws/catalog\")",
	)

	outputs := map[string]string{}
	for _, op := range profile.Operations {
		s, ok := g.model.Shapes[prefix+op]
		if !ok || s.Type != "operation" {
			return nil, fmt.Errorf("unknown operation %s", op)
		}

		if guardErr := checkTraits(s.Traits); guardErr != nil {
			return nil, guardErr
		}

		if s.Input.Target == unitShape {
			return nil, fmt.Errorf("%s: operations without input are not supported", op)
		}

		outputs[op] = short(s.Output.Target)
		ids := []string{s.Input.Target}
		if s.Output.Target == unitShape {
			outputs[op] = op + "Output"
			if _, exists := g.model.Shapes[prefix+outputs[op]]; exists {
				return nil, fmt.Errorf("%s: unit output name collides with a model shape", op)
			}

			fmt.Fprintf(&g.out, "type %s struct{}\n\n", outputs[op])
		} else {
			ids = append(ids, s.Output.Target)
		}

		for _, id := range ids {
			if guardErr := g.emit(id); guardErr != nil {
				return nil, guardErr
			}
		}
	}

	fmt.Fprintln(&g.out, "type API interface {")
	for _, op := range profile.Operations {
		s := g.model.Shapes[prefix+op]
		fmt.Fprintf(&g.out, "%s(context.Context,*%s)(*%s,error)\n", op, short(s.Input.Target), outputs[op])
	}

	fmt.Fprintln(&g.out, "}")
	errorBindings, err := g.errorBindings(profile.Operations, prefix, g.query)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(
		&g.out,
		"func D"+
			"escriptor(api API) catalog.Service {return catalog.Service{Name: %q, SigningName: %q,"+
			"TargetPrefix:%q,QueryCompatible:%t,XMLNamespace:%q,Protocols:%#v,Errors:%#v,Operations:map[stri"+
			"ng]catalog.Operation{\n",
		name,
		sigv4.Name,
		targetPrefix,
		queryCompatible,
		namespace,
		protocols,
		errorBindings,
	)

	for _, op := range profile.Operations {
		s := g.model.Shapes[prefix+op]
		bindings := map[string]catalog.Binding{}
		for _, protocol := range protocols {
			binding, bindingErr := compileBinding(g.model, prefix+op, protocol, targetPrefix, svc.Version)
			if bindingErr != nil {
				return nil, bindingErr
			}

			bindings[protocol] = binding
		}

		fmt.Fprintf(
			&g.out,
			"%q:{Name:%q,Bindings:%#v,NewI"+
				"nput:func()any{return &%s{}},Invoke:func(ctx context.Context,input any)(any"+
				",error){return api.%s(ctx,input.(*%s))}},\n",
			op,
			op,
			bindings,
			short(s.Input.Target),
			op,
			short(s.Input.Target),
		)
	}

	fmt.Fprintln(&g.out, "}}}")

	formatted, err := format.Source(g.out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w", err)
	}

	return formatted, nil
}

func run(root string) error {
	dir := filepath.Join(root, "generator", "model")
	var manifest struct {
		Models map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"models"`
	}

	// #nosec G304 -- manifest is read from the selected local model directory.
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}

	if guardErr := json.Unmarshal(b, &manifest); guardErr != nil {
		return guardErr
	}

	var profiles map[string]serviceProfile

	// #nosec G304 -- profile is a selected local generator input.
	b, err = os.ReadFile(filepath.Join(dir, "profile.json"))
	if err != nil {
		return err
	}

	if guardErr := json.Unmarshal(b, &profiles); guardErr != nil {
		return guardErr
	}

	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		pinned, ok := manifest.Models[name]
		if !ok {
			return fmt.Errorf("%s: model is not pinned in manifest.json", name)
		}

		// #nosec G304 -- the model name comes from the local generator profile.
		data, readErr := os.ReadFile(filepath.Join(dir, name+".json"))
		if readErr != nil {
			return readErr
		}

		if fmt.Sprintf("%x", sha256.Sum256(data)) != pinned.SHA256 {
			return fmt.Errorf("%s: model checksum mismatch", name)
		}

		source, genErr := generate(name, data, profiles[name])
		if genErr != nil {
			return fmt.Errorf("%s: %w", name, genErr)
		}

		out := filepath.Join(root, "internal", "aws", "generated", name)
		if guardErr := os.MkdirAll(out, 0o755); guardErr != nil {
			return guardErr
		}

		// #nosec G703 -- the output path is derived from local profile service names.
		if guardErr := os.WriteFile(filepath.Join(out, "api.go"), source, 0o644); guardErr != nil {
			return guardErr
		}
	}

	return nil
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()

	if guardErr := run(*root); guardErr != nil {
		fmt.Fprintln(os.Stderr, guardErr)
		os.Exit(1)
	}
}
