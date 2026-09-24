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
	traitStreaming = "smithy.api#streaming"
	shapeUnion     = "union"
	shapeList      = "list"
	shapeMap       = "map"
	unitShape      = "smithy.api#Unit"
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
		model     model
		profile   serviceProfile
		seen      map[string]bool
		imports   map[string]bool
		roots     map[string]bool
		namespace string
		out       bytes.Buffer
		query     bool
		rest      bool
		xml       bool
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
	case "timestamp":
		// REST XML carries timestamps as ISO 8601 text and HTTP dates, which
		// time.Time encodes; the JSON protocols use epoch seconds.
		if g.xml {
			g.imports["time"] = true
			return "time.Time", nil
		}

		return "float64", nil
	case "double", "float":
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

// restTraits are the HTTP binding traits REST protocols support.
var restTraits = map[string]bool{
	"smithy.api#http":              true,
	"smithy.api#httpLabel":         true,
	"smithy.api#httpHeader":        true,
	"smithy.api#httpQuery":         true,
	"smithy.api#httpPayload":       true,
	"smithy.api#httpPrefixHeaders": true,
	"smithy.api#httpResponseCode":  true,
	traitStreaming:                 true,
}

// httpBound reports whether a member is placed in the HTTP message rather than
// the document body.
func httpBound(member reference) bool {
	for trait := range member.Traits {
		if trait != "smithy.api#http" && restTraits[trait] && trait != traitStreaming {
			return true
		}
	}

	return false
}

// checkTraits rejects wire traits the codecs do not implement. REST services
// also accept the HTTP binding traits.
func checkTraits(traits map[string]json.RawMessage, rest bool) error {
	for t := range traits {
		if rest && restTraits[t] {
			continue
		}

		if strings.HasPrefix(t, "smithy.api#http") || t == traitStreaming ||
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

// xmlTag records the encoding/xml element for a REST XML body member. Members
// bound to the HTTP message are skipped by the XML codec.
func (g *generator) xmlTag(member reference, name string) (string, error) {
	if httpBound(member) {
		return ` xml:"-"`, nil
	}

	wire, err := xmlName(member.Traits, name)
	if err != nil {
		return "", err
	}

	target := g.model.Shapes[member.Target]
	switch target.Type {
	case shapeMap:
		return "", fmt.Errorf("maps in REST XML bodies are not supported")
	case shapeList:
		if kind := g.model.Shapes[target.Member.Target].Type; kind == shapeList || kind == shapeMap {
			return "", fmt.Errorf("nested collections are not supported by the REST XML codec")
		}

		if _, flat := member.Traits["smithy.api#xmlFlattened"]; !flat {
			memberName, nameErr := xmlName(target.Member.Traits, "member")
			if nameErr != nil {
				return "", nameErr
			}

			wire += ">" + memberName
		}
	}

	return ` xml:"` + wire + `,omitempty"`, nil
}

// xmlRoot returns the XMLName field of a REST XML structure that is an XML
// document root: shapes with an xmlName trait, and operation inputs and outputs
// with body members.
func (g *generator) xmlRoot(id string, s *shape) (string, error) {
	name, err := xmlName(s.Traits, short(id))
	if err != nil {
		return "", err
	}

	_, named := s.Traits["smithy.api#xmlName"]
	body := false
	for _, n := range g.fields(id) {
		if tag, tagErr := g.xmlTag(s.Members[n], n); tagErr == nil && tag != ` xml:"-"` {
			body = true
		}
	}

	if !named && (!g.roots[id] || !body) {
		return "", nil
	}

	g.imports["encoding/xml"] = true
	return fmt.Sprintf("XMLName xml.Name `json:\"-\" xml:%q`\n", strings.TrimSpace(g.namespace+" "+name)), nil
}

func (g *generator) emit(id string) error {
	if g.seen[id] {
		return nil
	}

	g.seen[id] = true
	s := g.model.Shapes[id]
	if guardErr := checkTraits(s.Traits, g.rest); guardErr != nil {
		return fmt.Errorf("%s: %w", id, guardErr)
	}

	switch s.Type {
	case shapeList:
		if guardErr := checkTraits(s.Member.Traits, g.rest); guardErr != nil {
			return fmt.Errorf("%s: %w", id, guardErr)
		}

		return g.emit(s.Member.Target)
	case shapeMap:
		for _, ref := range []reference{s.Key, s.Value} {
			if guardErr := checkTraits(ref.Traits, g.rest); guardErr != nil {
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

			if guardErr := checkTraits(m.Traits, g.rest); guardErr != nil {
				return guardErr
			}

			if guardErr := g.emit(m.Target); guardErr != nil {
				return guardErr
			}
		}

		fmt.Fprintf(&g.out, "type %s struct {\n", short(id))
		if g.xml {
			root, err := g.xmlRoot(id, &s)
			if err != nil {
				return fmt.Errorf("%s: %w", id, err)
			}

			g.out.WriteString(root)
		}

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
			if g.rest && httpBound(m) {
				tag = "-"
			}

			if short(id) == "ListTablesOutput" && n == "TableNames" {
				tag = n
			}

			if short(id) == "ScanOutput" && n == "Items" {
				t = "*" + t
			}

			queryTag := ""
			switch {
			case g.query:
				queryTag, err = g.queryTag(m, n)
			case g.xml:
				queryTag, err = g.xmlTag(m, n)
			}

			if err != nil {
				return fmt.Errorf("%s.%s: %w", id, n, err)
			}

			fmt.Fprintf(&g.out, "%s %s `json:\"%s\"%s`\n", n, t, tag, queryTag)
		}

		fmt.Fprintln(&g.out, "}")
		fmt.Fprintln(&g.out)
	}

	return nil
}

// errorBindings describes every modeled error of the enabled operations. Query
// codes are recorded for services that speak AWS Query, and message members for
// REST JSON services, whose error bodies use the modeled member name.
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

			if g.rest && !g.xml {
				for member := range g.model.Shapes[ref.Target].Members {
					if strings.EqualFold(member, "message") {
						binding.MessageMember = member
					}
				}
			}

			bindings[short(ref.Target)] = binding
		}
	}

	return bindings, nil
}

// protocols selects the wire protocols of the service. AWS JSON 1.0 services
// add AWS Query when they are awsQueryCompatible and the profile supplies the
// legacy XML namespace; pure awsQuery services take it from the model. AWS JSON
// 1.1 services differ from 1.0 only in content type. REST services bind members
// to the HTTP message.
func (g *generator) protocols(svc *shape) (protocols []string, namespace string, queryCompatible bool, err error) {
	_, jsonProtocol := svc.Traits["aws.protocols#awsJson1_0"]
	_, json11Protocol := svc.Traits["aws.protocols#awsJson1_1"]
	_, queryProtocol := svc.Traits["aws.protocols#awsQuery"]
	_, queryCompatible = svc.Traits["aws.protocols#awsQueryCompatible"]
	_, restXML := svc.Traits["aws.protocols#restXml"]
	_, restJSON := svc.Traits["aws.protocols#restJson1"]
	if (restXML || restJSON) && g.query {
		return nil, "", false, fmt.Errorf("REST models do not take a query overlay")
	}

	switch {
	case restXML:
		var ns struct{ URI string }
		if guardErr := json.Unmarshal(svc.Traits["smithy.api#xmlNamespace"], &ns); guardErr != nil || ns.URI == "" {
			return nil, "", false, fmt.Errorf("restXml model needs an XML namespace")
		}

		g.rest, g.xml = true, true
		return []string{catalog.RESTXML}, ns.URI, false, nil
	case restJSON:
		g.rest = true
		return []string{catalog.RESTJSON}, "", false, nil
	case jsonProtocol && g.query:
		if !queryCompatible || svc.Version == "" || g.profile.Query.XMLNamespace == "" {
			return nil, "", false, fmt.Errorf(
				"query overlay needs an awsQueryCompatible model, API version, and XML namespace",
			)
		}

		return []string{catalog.AWSJSON10, catalog.AWSQuery}, g.profile.Query.XMLNamespace, true, nil
	case jsonProtocol:
		return []string{catalog.AWSJSON10}, "", queryCompatible, nil
	case json11Protocol && (g.query || queryCompatible):
		return nil, "", false, fmt.Errorf("AWS Query compatibility needs an AWS JSON 1.0 model")
	case json11Protocol:
		return []string{catalog.AWSJSON11}, "", false, nil
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
		return nil, "", false, fmt.Errorf("model is not AWS JSON, AWS Query, REST XML, or REST JSON")
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
	g := generator{
		seen:    map[string]bool{},
		imports: map[string]bool{},
		roots:   map[string]bool{},
		profile: profile,
		query:   profile.Query != nil,
	}
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

	g.namespace = namespace

	var sigv4 struct{ Name string }
	if guardErr := json.Unmarshal(svc.Traits["aws.auth#sigv4"], &sigv4); guardErr != nil || sigv4.Name == "" {
		return nil, fmt.Errorf("model has no SigV4 signing name")
	}

	prefix, targetPrefix, _ := strings.Cut(serviceID, "#")
	prefix += "#"
	if !slices.Contains(protocols, catalog.AWSJSON10) && !slices.Contains(protocols, catalog.AWSJSON11) {
		targetPrefix = ""
	}

	if len(profile.Operations) == 0 {
		return nil, fmt.Errorf("profile selects no operations")
	}

	outputs := map[string]string{}
	for _, op := range profile.Operations {
		s, ok := g.model.Shapes[prefix+op]
		if !ok || s.Type != "operation" {
			return nil, fmt.Errorf("unknown operation %s", op)
		}

		if guardErr := checkTraits(s.Traits, g.rest); guardErr != nil {
			return nil, guardErr
		}

		if s.Input.Target == unitShape {
			return nil, fmt.Errorf("%s: operations without input are not supported", op)
		}

		g.roots[s.Input.Target], g.roots[s.Output.Target] = true, true
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
			binding, bindingErr := compileBinding(g.model, prefix+op, protocol, targetPrefix, svc.Version, g.fields)
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

	imports := []string{"context", "github.com/amjadjibon/tinystack/internal/aws/catalog"}
	for pkg := range g.imports {
		imports = append(imports, pkg)
	}

	sort.Strings(imports)
	var source bytes.Buffer
	source.WriteString("// Code generated by generator/cmd; DO NOT EDIT.\npackage " + name + "\n\nimport (\n")
	for _, pkg := range imports {
		fmt.Fprintf(&source, "%q\n", pkg)
	}

	source.WriteString(")\n\n")
	source.Write(g.out.Bytes())
	formatted, err := format.Source(source.Bytes())
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
