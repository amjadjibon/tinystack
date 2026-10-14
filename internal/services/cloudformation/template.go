package cloudformation

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxBody is the largest template TemplateBody takes; maxURLBody is the
// largest one TemplateURL names.
const (
	maxBody    = 51200
	maxURLBody = 1 << 20
)

// template is a parsed, validated template. Values are JSON-like: maps,
// lists, strings, float64 numbers, and booleans, with intrinsic functions as
// one-key maps ({fnRef: ...}, {"Fn::Sub": ...}).
type template struct {
	Metadata   any
	Parameters map[string]*parameterDecl
	Mappings   map[string]any
	Conditions map[string]any
	// Rules validate the parameters; see checkRules.
	Rules       map[string]any
	Resources   map[string]*resourceDecl
	Outputs     map[string]*outputDecl
	Version     string
	Description string
	Transforms  []string
	// order lists the resources so that each comes after those it depends on.
	order []string
}

type parameterDecl struct {
	MinLength             *float64
	MaxLength             *float64
	MinValue              *float64
	MaxValue              *float64
	Default               *string
	Type                  string
	Description           string
	AllowedPattern        string
	ConstraintDescription string
	AllowedValues         []string
	NoEcho                bool
}

type resourceDecl struct {
	Properties          map[string]any
	Metadata            any
	Type                string
	Condition           string
	DeletionPolicy      string
	UpdateReplacePolicy string
	DependsOn           []string
	// deps are the resources it references or depends on.
	deps []string
}

type outputDecl struct {
	Value       any
	Description any
	Export      any
	Condition   string
}

var (
	sections = []string{
		"AWSTemplateFormatVersion", memberDescription, "Metadata", "Parameters", "Mappings", "Conditions",
		"Rules", "Transform", keyResources, "Outputs",
	}
	resourceAttributes = []string{
		memberType, keyProperties, "DependsOn", keyCondition, "DeletionPolicy", "UpdateReplacePolicy", "Metadata",
	}
	parameterAttributes = []string{
		memberType, "Default", memberDescription, "NoEcho", "AllowedValues", "AllowedPattern", "MinLength", "MaxLength",
		"MinValue", "MaxValue", "ConstraintDescription",
	}
	logicalID = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	// functions are the intrinsic functions TinyStack evaluates.
	functions = []string{
		fnRef, fnGetAtt, fnSub, fnJoin, "Fn::Select", "Fn::Split", fnIf, "Fn::Equals", fnNot,
		fnAnd, "Fn::Or", "Fn::Contains", "Fn::FindInMap", fnGetAZs, "Fn::ImportValue", fnBase64, keyCondition,
	}
)

// pseudo are the pseudo parameters Ref takes.
var pseudo = []string{
	"AWS::AccountId", "AWS::Region", "AWS::StackName", "AWS::StackId", "AWS::Partition", "AWS::URLSuffix",
	"AWS::NoValue", "AWS::NotificationARNs",
}

func formatError(format string, args ...any) error {
	return validation("Template format error: "+format, args...)
}

func templateError(format string, args ...any) error {
	return validation("Template error: "+format, args...)
}

// decode parses a JSON or YAML template into JSON-like values, turning YAML's
// short-form functions (!Ref, !Sub, ...) into their long forms.
func decode(body string) (map[string]any, error) {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") {
		var v any
		if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
			return nil, formatError("JSON not well-formed. (%s)", err.Error())
		}

		m, ok := v.(map[string]any)
		if !ok {
			return nil, formatError("the template must be a JSON object")
		}

		return m, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, formatError("YAML not well-formed. (%s)", err.Error())
	}

	if len(doc.Content) == 0 {
		return nil, formatError("the template is empty")
	}

	v, err := fromYAML(doc.Content[0])
	if err != nil {
		return nil, formatError("%s", err.Error())
	}

	m, ok := v.(map[string]any)
	if !ok {
		return nil, formatError("the template must be a YAML mapping")
	}

	return m, nil
}

func fromYAML(n *yaml.Node) (any, error) {
	if n.Kind == yaml.AliasNode {
		return fromYAML(n.Alias)
	}

	if fn, ok := strings.CutPrefix(n.Tag, "!"); ok && !strings.HasPrefix(fn, "!") {
		return shortForm(fn, n)
	}

	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: mapping keys must be scalars", n.Content[i].Line)
			}

			v, err := fromYAML(n.Content[i+1])
			if err != nil {
				return nil, err
			}

			m[n.Content[i].Value] = v
		}

		return m, nil
	case yaml.SequenceNode:
		list := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromYAML(c)
			if err != nil {
				return nil, err
			}

			list = append(list, v)
		}

		return list, nil
	case yaml.ScalarNode:
		return scalar(n), nil
	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
	}
}

func scalar(n *yaml.Node) any {
	switch n.Tag {
	case "!!null":
		return nil
	case "!!bool":
		b, err := strconv.ParseBool(strings.ToLower(n.Value))
		if err == nil {
			return b
		}
	case "!!int", "!!float":
		if f, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil {
			return f
		}
	default:
	}

	return n.Value
}

// shortForm turns a YAML tag such as !GetAtt into its long form.
func shortForm(fn string, n *yaml.Node) (any, error) {
	untagged := *n
	untagged.Tag = ""
	switch n.Kind {
	case yaml.ScalarNode:
		untagged.Tag = "!!str"
	case yaml.MappingNode:
		untagged.Tag = "!!map"
	case yaml.SequenceNode:
		untagged.Tag = "!!seq"
	default:
	}

	v, err := fromYAML(&untagged)
	if err != nil {
		return nil, err
	}

	switch fn {
	case fnRef, keyCondition:
		return map[string]any{fn: v}, nil
	case "GetAtt":
		if s, ok := v.(string); ok {
			resource, attribute, _ := strings.Cut(s, ".")
			v = []any{resource, attribute}
		}
	default:
	}

	return map[string]any{"Fn::" + fn: v}, nil
}

// parse decodes and validates a template.
func parse(body string) (*template, error) {
	doc, err := decode(body)
	if err != nil {
		return nil, err
	}

	transforms, err := stringList(doc["Transform"], "Transform")
	if err != nil {
		return nil, err
	}

	if doc, err = transform(doc, transforms); err != nil {
		return nil, err
	}

	for k := range doc {
		if !slices.Contains(sections, k) {
			return nil, formatError("Invalid template property or properties [%s]", k)
		}
	}

	t := &template{
		Metadata: doc["Metadata"], Parameters: map[string]*parameterDecl{}, Resources: map[string]*resourceDecl{},
		Outputs: map[string]*outputDecl{}, Transforms: transforms,
	}
	if t.Version, err = text(doc["AWSTemplateFormatVersion"], "AWSTemplateFormatVersion"); err != nil {
		return nil, err
	}

	if t.Version != "" && t.Version != "2010-09-09" {
		return nil, formatError("'%s' is not a supported AWSTemplateFormatVersion value", t.Version)
	}

	if t.Description, err = text(doc[memberDescription], memberDescription); err != nil {
		return nil, err
	}

	if t.Mappings, err = object(doc["Mappings"], "Mappings"); err != nil {
		return nil, err
	}

	if t.Conditions, err = object(doc["Conditions"], "Conditions"); err != nil {
		return nil, err
	}

	if t.Rules, err = object(doc["Rules"], "Rules"); err != nil {
		return nil, err
	}

	err = t.parseParameters(doc["Parameters"])
	if err != nil {
		return nil, err
	}

	err = t.parseResources(doc[keyResources])
	if err != nil {
		return nil, err
	}

	err = t.parseOutputs(doc["Outputs"])
	if err != nil {
		return nil, err
	}

	err = t.check()
	if err != nil {
		return nil, err
	}

	return t, nil
}

func text(v any, what string) (string, error) {
	switch s := v.(type) {
	case nil:
		return "", nil
	case string:
		return s, nil
	default:
		return "", formatError("%s must be a string", what)
	}
}

// stringList reads a string or a list of strings.
func stringList(v any, what string) ([]string, error) {
	switch s := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{s}, nil
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			str, ok := e.(string)
			if !ok {
				return nil, formatError("%s must be a string or a list of strings", what)
			}

			out = append(out, str)
		}

		return out, nil
	default:
		return nil, formatError("%s must be a string or a list of strings", what)
	}
}

func object(v any, what string) (map[string]any, error) {
	switch m := v.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return m, nil
	default:
		return nil, formatError("%s must be an object", what)
	}
}

func (t *template) parseParameters(v any) error {
	params, err := object(v, "Parameters")
	if err != nil {
		return err
	}

	if len(params) > 200 {
		return formatError("Parameters cannot exceed 200 entries")
	}

	for name, raw := range params {
		if !logicalID.MatchString(name) {
			return formatError("Parameter name %s is non alphanumeric.", name)
		}

		decl, ok := raw.(map[string]any)
		if !ok {
			return formatError("Parameter %s must be an object", name)
		}

		p, err := parameterOf(name, decl)
		if err != nil {
			return err
		}

		t.Parameters[name] = p
	}

	return nil
}

func parameterOf(name string, decl map[string]any) (*parameterDecl, error) {
	for k := range decl {
		if !slices.Contains(parameterAttributes, k) {
			return nil, formatError("Invalid template parameter property '%s'", k)
		}
	}

	p := &parameterDecl{}
	var err error
	if p.Type, err = text(decl[memberType], "Parameter "+name+" Type"); err != nil {
		return nil, err
	}

	if !supportedParameterType(p.Type) {
		return nil, validation("TinyStack does not support parameter type %s (parameter %s)", p.Type, name)
	}

	if d, ok := decl["Default"]; ok {
		s := scalarText(d)
		if list, isList := d.([]any); isList {
			parts := make([]string, len(list))
			for i, e := range list {
				parts[i] = scalarText(e)
			}

			s = strings.Join(parts, ",")
		}

		p.Default = &s
	}

	if p.Description, err = text(decl[memberDescription], "Parameter "+name+" Description"); err != nil {
		return nil, err
	}

	if p.AllowedPattern, err = text(decl["AllowedPattern"], "Parameter "+name+" AllowedPattern"); err != nil {
		return nil, err
	}

	if p.ConstraintDescription, err = text(decl["ConstraintDescription"], "ConstraintDescription"); err != nil {
		return nil, err
	}

	if p.AllowedPattern != "" {
		if _, err = regexp.Compile("^(?:" + p.AllowedPattern + ")$"); err != nil {
			return nil, formatError("Parameter %s has an invalid AllowedPattern", name)
		}
	}

	p.NoEcho = scalarText(decl["NoEcho"]) == textTrue
	if allowed, ok := decl["AllowedValues"].([]any); ok {
		for _, a := range allowed {
			p.AllowedValues = append(p.AllowedValues, scalarText(a))
		}
	}

	for _, n := range []struct {
		dst  **float64
		name string
	}{{&p.MinLength, "MinLength"}, {&p.MaxLength, "MaxLength"}, {&p.MinValue, "MinValue"}, {&p.MaxValue, "MaxValue"}} {
		raw, ok := decl[n.name]
		if !ok {
			continue
		}

		f, parseErr := strconv.ParseFloat(scalarText(raw), 64)
		if parseErr != nil {
			return nil, formatError("Parameter %s %s must be a number", name, n.name)
		}

		*n.dst = &f
	}

	return p, nil
}

// supportedParameterType reports whether TinyStack takes parameters of type
// t: plain values, lists, AWS-specific IDs (taken as text), and Systems
// Manager parameter names.
func supportedParameterType(t string) bool {
	switch {
	case t == typeString, t == typeNumber, t == "CommaDelimitedList", t == "List<Number>",
		t == "AWS::SSM::Parameter::Value<String>", t == "AWS::SSM::Parameter::Value<List<String>>",
		t == "AWS::SSM::Parameter::Value<CommaDelimitedList>", t == "AWS::SSM::Parameter::Name":
		return true
	case strings.HasPrefix(t, "AWS::EC2::"), strings.HasPrefix(t, "AWS::Route53::"):
		return true
	case strings.HasPrefix(t, "List<AWS::EC2::"), strings.HasPrefix(t, "List<AWS::Route53::"):
		return true
	default:
		return false
	}
}

// listParameter reports whether parameters of type t are lists.
func listParameter(t string) bool {
	return strings.HasPrefix(t, "List<") || t == "CommaDelimitedList" ||
		t == "AWS::SSM::Parameter::Value<List<String>>" || t == "AWS::SSM::Parameter::Value<CommaDelimitedList>"
}

// ssmParameter reports whether parameters of type t name Systems Manager
// parameters whose values the stack uses.
func ssmParameter(t string) bool { return strings.HasPrefix(t, "AWS::SSM::Parameter::Value<") }

// scalarText renders a scalar template value as text.
func scalarText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(s)
	case nil:
		return ""
	default:
		data, _ := json.Marshal(s)
		return string(data)
	}
}

func (t *template) parseResources(v any) error {
	res, ok := v.(map[string]any)
	if !ok || len(res) == 0 {
		return formatError("At least one Resources member must be defined.")
	}

	var unsupported []string
	for name, raw := range res {
		if !logicalID.MatchString(name) {
			return formatError("Resource name %s is non alphanumeric.", name)
		}

		decl, isMap := raw.(map[string]any)
		if !isMap {
			return formatError("[/Resources/%s] Every Resources object must contain a Type member.", name)
		}

		r, err := resourceOf(name, decl)
		if err != nil {
			return err
		}

		if _, known := types[r.Type]; !known {
			unsupported = append(unsupported, r.Type)
		}

		t.Resources[name] = r
	}

	if len(unsupported) > 0 {
		slices.Sort(unsupported)
		return validation("TinyStack does not support these resource types: [%s]",
			strings.Join(slices.Compact(unsupported), ", "))
	}

	return nil
}

func resourceOf(name string, decl map[string]any) (*resourceDecl, error) {
	for k := range decl {
		switch {
		case k == "CreationPolicy" || k == "UpdatePolicy":
			return nil, validation("TinyStack does not support the %s attribute (resource %s)", k, name)
		case !slices.Contains(resourceAttributes, k):
			return nil, formatError("[/Resources/%s] Invalid resource attribute %s", name, k)
		}
	}

	r := &resourceDecl{Metadata: decl["Metadata"]}
	var err error
	if r.Type, err = text(decl[memberType], memberType); err != nil || r.Type == "" {
		return nil, formatError("[/Resources/%s] Every Resources object must contain a Type member.", name)
	}

	if r.Properties, err = object(decl[keyProperties], keyProperties); err != nil {
		return nil, formatError("[/Resources/%s/Properties] Properties must be an object", name)
	}

	if r.DependsOn, err = stringList(decl["DependsOn"], "DependsOn"); err != nil {
		return nil, err
	}

	if r.Condition, err = text(decl[keyCondition], keyCondition); err != nil {
		return nil, err
	}

	if r.DeletionPolicy, err = policy(decl["DeletionPolicy"], name, "DeletionPolicy"); err != nil {
		return nil, err
	}

	if r.UpdateReplacePolicy, err = policy(decl["UpdateReplacePolicy"], name, "UpdateReplacePolicy"); err != nil {
		return nil, err
	}

	return r, nil
}

func policy(v any, name, what string) (string, error) {
	p, err := text(v, what)
	switch {
	case err != nil:
		return "", err
	case p == "", p == "Delete", p == policyRetain, p == "RetainExceptOnCreate":
		return p, nil
	case p == "Snapshot":
		return "", validation("TinyStack does not support the Snapshot %s (resource %s)", what, name)
	default:
		return "", formatError("[/Resources/%s] Unrecognized %s %s", name, what, p)
	}
}

func (t *template) parseOutputs(v any) error {
	outs, err := object(v, "Outputs")
	if err != nil {
		return err
	}

	for name, raw := range outs {
		if !logicalID.MatchString(name) {
			return formatError("Output name %s is non alphanumeric.", name)
		}

		decl, ok := raw.(map[string]any)
		if !ok {
			return formatError("[/Outputs/%s] Every Outputs member must contain a Value object", name)
		}

		for k := range decl {
			if !slices.Contains([]string{memberValue, memberDescription, "Export", keyCondition}, k) {
				return formatError("[/Outputs/%s] Invalid outputs property : [%s]", name, k)
			}
		}

		if _, hasValue := decl[memberValue]; !hasValue {
			return formatError("[/Outputs/%s] Every Outputs member must contain a Value object", name)
		}

		o := &outputDecl{Value: decl[memberValue], Description: decl[memberDescription], Export: decl["Export"]}
		if o.Condition, err = text(decl[keyCondition], keyCondition); err != nil {
			return err
		}

		if o.Export != nil {
			export, isMap := o.Export.(map[string]any)
			if !isMap || len(export) != 1 || export[memberName] == nil {
				return formatError("[/Outputs/%s/Export] Export must contain only a Name", name)
			}
		}

		t.Outputs[name] = o
	}

	return nil
}

// check validates references and properties, and orders the resources.
func (t *template) check() error {
	for name := range t.Conditions {
		if _, ok := t.Parameters[name]; ok {
			return formatError("Condition %s has the name of a parameter", name)
		}
	}

	for name, r := range t.Resources {
		if _, ok := t.Parameters[name]; ok {
			return formatError("Resource %s has the name of a parameter", name)
		}

		if r.Condition != "" {
			if _, ok := t.Conditions[r.Condition]; !ok {
				return templateError("unresolved condition dependency %s in Fn::If", r.Condition)
			}
		}

		typ := types[r.Type]
		for p := range r.Properties {
			if !slices.Contains(typ.properties, p) {
				return validation("TinyStack does not support the %s property of %s (resource %s)", p, r.Type, name)
			}
		}

		deps := map[string]bool{}
		for _, d := range r.DependsOn {
			if _, ok := t.Resources[d]; !ok {
				return formatError("Unresolved resource dependencies [%s] in the Resources block of the template", d)
			}

			deps[d] = true
		}

		if err := t.walk(r.Properties, deps, "/Resources/"+name); err != nil {
			return err
		}

		delete(deps, name)
		r.deps = slices.Sorted(maps.Keys(deps))
	}

	for name, o := range t.Outputs {
		if o.Condition != "" {
			if _, ok := t.Conditions[o.Condition]; !ok {
				return templateError("unresolved condition dependency %s in Outputs", o.Condition)
			}
		}

		for _, v := range []any{o.Value, o.Description, o.Export} {
			if err := t.walk(v, map[string]bool{}, "/Outputs/"+name); err != nil {
				return err
			}
		}
	}

	for _, c := range t.Conditions {
		if err := t.walk(c, map[string]bool{}, "/Conditions"); err != nil {
			return err
		}
	}

	return t.sort()
}

// walk checks the functions in v and collects the resources it references.
func (t *template) walk(v any, deps map[string]bool, path string) error {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if err := t.walk(e, deps, path); err != nil {
				return err
			}
		}
	case map[string]any:
		if len(x) == 1 {
			for fn, arg := range x {
				if fn == fnRef || fn == keyCondition || strings.HasPrefix(fn, "Fn::") {
					return t.walkFunction(fn, arg, deps, path)
				}
			}
		}

		for _, e := range x {
			if err := t.walk(e, deps, path); err != nil {
				return err
			}
		}
	}

	return nil
}

func (t *template) walkFunction(fn string, arg any, deps map[string]bool, path string) error {
	if !slices.Contains(functions, fn) {
		return validation("TinyStack does not support the intrinsic function %s (%s)", fn, path)
	}

	switch fn {
	case fnRef:
		name, ok := arg.(string)
		if !ok {
			return templateError("Ref in %s must be a string", path)
		}

		return t.reference(name, deps, path)
	case fnGetAtt:
		parts, ok := arg.([]any)
		if !ok || len(parts) != 2 {
			return templateError("every Fn::GetAtt object requires two non-empty parameters (%s)", path)
		}

		name, _ := parts[0].(string)
		r, ok := t.Resources[name]
		if !ok {
			return templateError("instance of Fn::GetAtt references undefined resource %s", name)
		}

		if attribute, isText := parts[1].(string); isText && !slices.Contains(types[r.Type].attributes, attribute) {
			return templateError("resource %s does not support attribute type %s in Fn::GetAtt", name, attribute)
		}

		deps[name] = true
		return t.walk(parts[1], deps, path)
	case fnSub:
		return t.walkSub(arg, deps, path)
	case keyCondition:
		name, _ := arg.(string)
		if _, ok := t.Conditions[name]; !ok {
			return templateError("unresolved condition dependency %s", name)
		}

		return nil
	case fnIf:
		parts, ok := arg.([]any)
		if !ok || len(parts) != 3 {
			return templateError("Fn::If requires a list of three elements (%s)", path)
		}

		if name, _ := parts[0].(string); t.Conditions[name] == nil {
			return templateError("unresolved condition dependency %s in Fn::If", name)
		}

		return t.walk(parts[1:], deps, path)
	default:
		return t.walk(arg, deps, path)
	}
}

func (t *template) reference(name string, deps map[string]bool, path string) error {
	if _, ok := t.Parameters[name]; ok || slices.Contains(pseudo, name) {
		return nil
	}

	if _, ok := t.Resources[name]; ok {
		deps[name] = true
		return nil
	}

	return templateError("Unresolved resource dependencies [%s] in %s", name, path)
}

// subVariable matches a ${...} in an Fn::Sub string; ${!...} is literal.
var subVariable = regexp.MustCompile(`\$\{([^}]*)\}`)

func (t *template) walkSub(arg any, deps map[string]bool, path string) error {
	str, vars := "", map[string]any{}
	switch x := arg.(type) {
	case string:
		str = x
	case []any:
		if len(x) != 2 {
			return templateError("Fn::Sub takes a string or a list of a string and a map (%s)", path)
		}

		s, ok := x[0].(string)
		m, isMap := x[1].(map[string]any)
		if !ok || !isMap {
			return templateError("Fn::Sub takes a string or a list of a string and a map (%s)", path)
		}

		str, vars = s, m
		if err := t.walk(m, deps, path); err != nil {
			return err
		}
	default:
		return templateError("Fn::Sub takes a string or a list of a string and a map (%s)", path)
	}

	for _, m := range subVariable.FindAllStringSubmatch(str, -1) {
		name := strings.TrimSpace(m[1])
		if strings.HasPrefix(name, "!") {
			continue
		}

		if _, ok := vars[name]; ok {
			continue
		}

		resource, attribute, isAttribute := strings.Cut(name, ".")
		if !isAttribute {
			if err := t.reference(name, deps, path); err != nil {
				return err
			}

			continue
		}

		r, ok := t.Resources[resource]
		if !ok {
			return templateError("Unresolved resource dependencies [%s] in %s", resource, path)
		}

		if !slices.Contains(types[r.Type].attributes, attribute) {
			return templateError("resource %s does not support attribute type %s in Fn::Sub", resource, attribute)
		}

		deps[resource] = true
	}

	return nil
}

// sort orders the resources by their dependencies, rejecting cycles.
func (t *template) sort() error {
	const (
		unvisited = iota
		visiting
		visited
	)

	state := map[string]int{}
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case visited:
			return nil
		case visiting:
			return validation("Circular dependency between resources: [%s]", strings.Join(path, ", "))
		default:
		}

		state[name] = visiting
		for _, d := range t.Resources[name].deps {
			if err := visit(d, append(path, d)); err != nil {
				return err
			}
		}

		state[name] = visited
		t.order = append(t.order, name)
		return nil
	}

	for _, name := range slices.Sorted(maps.Keys(t.Resources)) {
		if err := visit(name, []string{name}); err != nil {
			return err
		}
	}

	return nil
}

// capabilities returns the capabilities a template needs and why.
func (t *template) capabilities() (required []string, reason string) {
	named := false
	var iam []string
	for _, r := range t.Resources {
		typ := types[r.Type]
		if !typ.iam {
			continue
		}

		if !slices.Contains(iam, r.Type) {
			iam = append(iam, r.Type)
		}

		if typ.name != "" && r.Properties[typ.name] != nil {
			named = true
		}
	}

	if len(iam) == 0 {
		return nil, ""
	}

	slices.Sort(iam)
	reason = "The following resource(s) require capabilities: [" + strings.Join(iam, ", ") + "]"
	if named {
		return []string{"CAPABILITY_NAMED_IAM"}, reason
	}

	return []string{capabilityIAM}, reason
}

// requireExpand checks that given acknowledges t's transforms, which
// CreateStack and UpdateStack require; change sets show the expansion.
func (t *template) requireExpand(given []string) error {
	if len(t.Transforms) > 0 && !slices.Contains(given, capabilityAutoExpand) {
		return insufficientCapabilities(capabilityAutoExpand)
	}

	return nil
}

// requireCapabilities checks that given acknowledges what t needs.
func (t *template) requireCapabilities(given []string) error {
	for _, c := range given {
		if !slices.Contains([]string{capabilityIAM, "CAPABILITY_NAMED_IAM", capabilityAutoExpand}, c) {
			return validation("Invalid capability %s", c)
		}
	}

	needed, _ := t.capabilities()
	switch {
	case len(needed) == 0, slices.Contains(given, "CAPABILITY_NAMED_IAM"):
		return nil
	case needed[0] == capabilityIAM && slices.Contains(given, capabilityIAM):
		return nil
	default:
		return insufficientCapabilities(needed[0])
	}
}

func resourceTypes(t *template) []string {
	var out []string
	for _, r := range t.Resources {
		out = append(out, r.Type)
	}

	slices.Sort(out)
	return slices.Compact(out)
}
