package sfn

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// optionalPath is an InputPath, OutputPath, or ResultPath: absent means "$",
// and JSON null discards (or, for ResultPath, keeps the input).
type optionalPath struct {
	path string
	set  bool
	null bool
}

func (p *optionalPath) UnmarshalJSON(data []byte) error {
	p.set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		p.null = true
		return nil
	}

	return json.Unmarshal(data, &p.path)
}

// get returns the path, "$" when it is absent.
func (p optionalPath) get() string {
	if !p.set {
		return "$"
	}

	return p.path
}

type retrier struct {
	MaxDelaySeconds *float64
	IntervalSeconds *float64
	MaxAttempts     *int
	BackoffRate     *float64
	JitterStrategy  string
	ErrorEquals     []string
}

type catcher struct {
	Assign      any
	Output      any
	Next        string
	ResultPath  optionalPath
	ErrorEquals []string
}

type processor struct {
	ProcessorConfig *struct {
		Mode          string
		ExecutionType string
	}
	machine
	raw json.RawMessage
}

// state is one state of a definition, of any type.
type state struct {
	Result                         any
	Parameters                     any
	ResultSelector                 any
	ItemSelector                   any
	Arguments                      any
	Output                         any
	Items                          any
	Assign                         any
	ItemReader                     *mapReader
	ResultWriter                   *mapWriter
	ItemProcessor                  *processor
	ItemBatcher                    *mapBatcher
	Label                          string
	MaxConcurrencyPath             string
	ToleratedFailurePercentage     number
	ToleratedFailurePercentagePath string
	ToleratedFailureCount          number
	ToleratedFailureCountPath      string
	Iterator                       *machine
	Type                           string
	Comment                        string
	Next                           string
	Resource                       string
	Cause                          string
	CausePath                      string
	Error                          string
	ErrorPath                      string
	Default                        string
	Timestamp                      string
	SecondsPath                    string
	TimestampPath                  string
	TimeoutSecondsPath             string
	HeartbeatSecondsPath           string
	QueryLanguage                  string
	ItemsPath                      string
	Choices                        []map[string]json.RawMessage
	OutputPath                     optionalPath
	ResultPath                     optionalPath
	InputPath                      optionalPath
	Branches                       []machine
	Catch                          []catcher
	Retry                          []retrier
	Seconds                        number
	MaxConcurrency                 number
	HeartbeatSeconds               number
	TimeoutSeconds                 number
	End                            bool
	// jsonata is set by validate for states that use JSONata.
	jsonata bool
}

// machine is a definition, or a Parallel branch or Map iterator.
type machine struct {
	States         map[string]*state
	TimeoutSeconds *int64
	StartAt        string
	Comment        string
	Version        string
	QueryLanguage  string
}

// parseDefinition parses and validates a definition, returning each problem
// found.
func parseDefinition(definition string) (def *machine, problems []string) {
	var m machine
	decoder := json.NewDecoder(strings.NewReader(definition))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return nil, []string{"SCHEMA_VALIDATION_FAILED: " + strings.TrimPrefix(err.Error(), "json: ")}
	}

	m.validate("", langJSONPath, func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	})
	labels := map[string]bool{}
	var visit func(*machine)
	visit = func(current *machine) {
		for _, s := range current.States {
			if s.Label != "" {
				if labels[s.Label] {
					problems = append(problems, "SCHEMA_VALIDATION_FAILED: Duplicate Map Label "+s.Label)
				}

				labels[s.Label] = true
			}

			for i := range s.Branches {
				visit(&s.Branches[i])
			}

			if s.Iterator != nil {
				visit(s.Iterator)
			}

			if s.ItemProcessor != nil {
				visit(&s.ItemProcessor.machine)
			}
		}
	}

	visit(new(m))
	return &m, problems
}

var stateTypes = []string{
	stateTask,
	statePass,
	stateChoice,
	stateWait,
	stateSucceed,
	stateFail,
	stateParallel,
	stateMap,
}

var comparators = map[string]bool{
	"StringEquals": true, "StringLessThan": true, "StringGreaterThan": true, "StringLessThanEquals": true,
	"StringGreaterThanEquals": true, "StringMatches": true, "NumericEquals": true, "NumericLessThan": true,
	"NumericGreaterThan": true, "NumericLessThanEquals": true, "NumericGreaterThanEquals": true,
	"BooleanEquals": true, "TimestampEquals": true, "TimestampLessThan": true, "TimestampGreaterThan": true,
	"TimestampLessThanEquals": true, "TimestampGreaterThanEquals": true, "IsNull": true, "IsPresent": true,
	"IsNumeric": true, "IsString": true, "IsBoolean": true, "IsTimestamp": true,
}

// validate validates a machine whose states use lang unless it or they say
// otherwise.
func (m *machine) validate(where, lang string, add func(string, ...any)) {
	lang = language(m.QueryLanguage, lang, where, add)

	if len(m.States) == 0 {
		add("SCHEMA_VALIDATION_FAILED: The field 'States' should be non-empty at %s", or(where, "/"))
		return
	}

	if _, ok := m.States[m.StartAt]; !ok {
		add("MISSING_TRANSITION_TARGET: Missing 'Next' target: %s at %s/StartAt", m.StartAt, where)
	}

	names := make([]string, 0, len(m.States))
	for name := range m.States {
		names = append(names, name)
	}

	sort.Strings(names)
	for _, name := range names {
		s := m.States[name]
		at := where + "/States/" + name
		if len(name) > 80 {
			add("SCHEMA_VALIDATION_FAILED: State names must be at most 80 characters at %s", at)
		}

		s.validate(m, at, lang, add)
	}
}

// language returns the query language a definition, branch, or state sets,
// or the one it inherits. A JSONata definition's states cannot use JSONPath.
func language(set, inherited, at string, add func(string, ...any)) string {
	switch set {
	case "":
		return inherited
	case langJSONPath, langJSONata:
		if set == langJSONPath && inherited == langJSONata {
			add("SCHEMA_VALIDATION_FAILED: A JSONata state machine cannot have JSONPath states at %s/QueryLanguage", at)
		}

		return set
	}

	add("SCHEMA_VALIDATION_FAILED: The QueryLanguage must be JSONPath or JSONata at %s/QueryLanguage", at)
	return inherited
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}

	return s
}

//nolint:gocyclo,cyclop // One check per rule of the States Language.
func (s *state) validate(m *machine, at, lang string, add func(string, ...any)) {
	if !slices.Contains(stateTypes, s.Type) {
		add("SCHEMA_VALIDATION_FAILED: The value for the field 'Type' must be one of %s at %s/Type",
			strings.Join(stateTypes, ", "), at)
		return
	}

	target := func(field, next string) {
		if _, ok := m.States[next]; !ok {
			add("MISSING_TRANSITION_TARGET: Missing '%s' target: %s at %s/%s", field, next, at, field)
		}
	}

	terminal := s.Type == stateSucceed || s.Type == stateFail || s.Type == stateChoice
	switch {
	case !terminal && s.End && s.Next != "":
		add("SCHEMA_VALIDATION_FAILED: Only one of 'Next' or 'End' may be set at %s", at)
	case !terminal && !s.End && s.Next == "":
		add("SCHEMA_VALIDATION_FAILED: One of 'Next' or 'End' must be set at %s", at)
	case !terminal && s.Next != "":
		target("Next", s.Next)
	case terminal && (s.End || s.Next != ""):
		add("SCHEMA_VALIDATION_FAILED: %s states cannot have 'Next' or 'End' at %s", s.Type, at)
	}

	if s.Type != stateMap && (s.ItemBatcher != nil || s.Label != "" || s.MaxConcurrencyPath != "" ||
		s.ToleratedFailureCount.set || s.ToleratedFailureCountPath != "" || s.ToleratedFailurePercentage.set ||
		s.ToleratedFailurePercentagePath != "" || s.ItemReader != nil || s.ResultWriter != nil) {
		add("SCHEMA_VALIDATION_FAILED: Distributed Map fields require a Map state at %s", at)
	}

	s.jsonata = language(s.QueryLanguage, lang, at, add) == langJSONata
	if s.jsonata {
		s.checkJSONata(at, add)
	} else {
		s.checkJSONPath(at, add)
	}

	for _, p := range []struct {
		name string
		path optionalPath
	}{{"InputPath", s.InputPath}, {"OutputPath", s.OutputPath}, {fieldResultPath, s.ResultPath}} {
		if p.path.set && !p.path.null {
			steps, err := parsePath(p.path.path)
			if err != nil || (p.name == "ResultPath" && !definite(steps)) {
				add("INVALID_JSON_PATH: Invalid JSON path %q at %s/%s", p.path.path, at, p.name)
			}
		}
	}

	for i, r := range s.Retry {
		if len(r.ErrorEquals) == 0 {
			add("SCHEMA_VALIDATION_FAILED: Retriers need ErrorEquals at %s/Retry[%d]", at, i)
		}
	}

	if s.Assign != nil && (s.Type == stateSucceed || s.Type == stateFail) {
		add("SCHEMA_VALIDATION_FAILED: %s states cannot have 'Assign' at %s", s.Type, at)
	}

	checkAssign(s.Assign, at, add)
	for i, c := range s.Catch {
		if len(c.ErrorEquals) == 0 {
			add("SCHEMA_VALIDATION_FAILED: Catchers need ErrorEquals at %s/Catch[%d]", at, i)
		}

		checkAssign(c.Assign, fmt.Sprintf("%s/Catch[%d]", at, i), add)

		target(fmt.Sprintf("Catch[%d]/Next", i), c.Next)
	}

	switch s.Type {
	case stateTask:
		if problem := checkResource(s.Resource); problem != "" {
			add("SCHEMA_VALIDATION_FAILED: %s at %s/Resource", problem, at)
		}
	case stateChoice:
		if len(s.Choices) == 0 {
			add("SCHEMA_VALIDATION_FAILED: Choice states need Choices at %s", at)
		}

		for i, rule := range s.Choices {
			ruleAt := fmt.Sprintf("%s/Choices[%d]", at, i)
			if s.jsonata {
				checkCondition(rule, ruleAt, add)
			} else {
				checkRule(rule, true, ruleAt, add)
			}

			checkAssign(member(rule, fieldAssign), ruleAt, add)
			var next string
			if json.Unmarshal(rule["Next"], &next) == nil && next != "" {
				target(fmt.Sprintf("Choices[%d]/Next", i), next)
			}
		}

		if s.Default != "" {
			target("Default", s.Default)
		}
	case stateWait:
		set := 0
		for _, v := range []bool{s.Seconds.set, s.Timestamp != "", s.SecondsPath != "", s.TimestampPath != ""} {
			if v {
				set++
			}
		}

		if set != 1 {
			add("SCHEMA_VALIDATION_FAILED: Wait states need exactly one of Seconds, Timestamp, SecondsPath, or "+
				"TimestampPath at %s", at)
		}

		if _, expr := expression(s.Timestamp); s.Timestamp != "" && !expr {
			if _, err := time.Parse(time.RFC3339, s.Timestamp); err != nil {
				add("SCHEMA_VALIDATION_FAILED: Invalid Timestamp at %s/Timestamp", at)
			}
		}
	case stateParallel:
		if len(s.Branches) == 0 {
			add("SCHEMA_VALIDATION_FAILED: Parallel states need Branches at %s", at)
		}

		for i := range s.Branches {
			s.Branches[i].validate(fmt.Sprintf("%s/Branches[%d]", at, i), lang, add)
		}
	case stateMap:
		s.validateDistributed(at, add)
		iterator := s.Iterator
		if s.ItemProcessor != nil {
			iterator = &s.ItemProcessor.machine
		}

		switch {
		case iterator == nil:
			add("SCHEMA_VALIDATION_FAILED: Map states need an ItemProcessor at %s", at)
		case !s.distributed() && (s.ItemReader != nil || s.ResultWriter != nil || s.ItemBatcher != nil):
			add("SCHEMA_VALIDATION_FAILED: ItemReader and ResultWriter are for distributed Map states at %s", at)
		default:
			iteratorLang := lang
			if s.jsonata {
				iteratorLang = langJSONata
			}

			iterator.validate(at+"/ItemProcessor", iteratorLang, add)
		}
	}
}

var (
	functionARN = regexp.MustCompile(`^arn:aws[a-z-]*:lambda:[a-z0-9-]+:\d{12}:function:[\w-]+(:[\w$-]+)?$`)
	activityARN = regexp.MustCompile(`^arn:aws[a-z-]*:states:[a-z0-9-]+:\d{12}:activity:[\w-]+$`)
	integration = regexp.MustCompile(`^arn:aws[a-z-]*:states:::([a-z0-9-]+):([A-Za-z0-9]+)(\.[A-Za-z:0-9]+)?$`)
	sdkResource = regexp.MustCompile(
		`^arn:aws[a-z-]*:states:::aws-sdk:([a-z0-9]+):([A-Za-z0-9]+)(\.waitForTaskToken)?$`,
	)
)

// checkResource reports why a Task resource is not one TinyStack runs.
func checkResource(resource string) string {
	switch {
	case functionARN.MatchString(resource), activityARN.MatchString(resource), sdkResource.MatchString(resource):
		return ""
	case integration.MatchString(resource):
		m := integration.FindStringSubmatch(resource)
		key := m[1] + ":" + m[2]
		if _, ok := integrations[key]; !ok {
			return "TinyStack does not support the " + key + " integration"
		}

		if suffix := m[3]; suffix != "" && !slices.Contains(integrations[key], suffix) {
			return "The " + key + " integration does not support " + suffix
		}

		return ""
	}

	return "The resource provided " + resource + " is not a Lambda function, activity, or service integration"
}

// checkJSONPath rejects the fields of JSONata states in a JSONPath state.
func (s *state) checkJSONPath(at string, add func(string, ...any)) {
	fields := []struct {
		name string
		set  bool
	}{
		{"Arguments", s.Arguments != nil},
		{fieldOutput, s.Output != nil},
		{fieldItems, s.Items != nil},
		{"Seconds", s.Seconds.expr != ""},
		{"TimeoutSeconds", s.TimeoutSeconds.expr != ""},
		{"HeartbeatSeconds", s.HeartbeatSeconds.expr != ""},
		{"MaxConcurrency", s.MaxConcurrency.expr != ""},
	}
	for _, f := range fields {
		if f.set {
			add(
				"SCHEMA_VALIDATION_FAILED: The field '%s' is supported only by JSONata states at %s/%s",
				f.name,
				at,
				f.name,
			)
		}
	}

	for i, c := range s.Catch {
		if c.Output != nil {
			add(
				"SCHEMA_VALIDATION_FAILED: The field 'Output' is supported only by JSONata states at %s/Catch[%d]",
				at,
				i,
			)
		}
	}
}

// checkJSONata rejects the fields of JSONPath states in a JSONata state and
// compiles its expressions.
func (s *state) checkJSONata(at string, add func(string, ...any)) {
	fields := []struct {
		name string
		set  bool
	}{
		{"InputPath", s.InputPath.set},
		{"OutputPath", s.OutputPath.set},
		{fieldResultPath, s.ResultPath.set},
		{"Parameters", s.Parameters != nil},
		{"ResultSelector", s.ResultSelector != nil},
		{"Result", s.Result != nil},
		{"ItemsPath", s.ItemsPath != ""},
		{"Iterator", s.Iterator != nil},
		{"SecondsPath", s.SecondsPath != ""},
		{"TimestampPath", s.TimestampPath != ""},
		{"TimeoutSecondsPath", s.TimeoutSecondsPath != ""},
		{"HeartbeatSecondsPath", s.HeartbeatSecondsPath != ""},
		{"ErrorPath", s.ErrorPath != ""},
		{"CausePath", s.CausePath != ""},
	}
	for _, f := range fields {
		if f.set {
			add(
				"SCHEMA_VALIDATION_FAILED: The field '%s' is not supported by JSONata states at %s/%s",
				f.name,
				at,
				f.name,
			)
		}
	}

	for name, n := range map[string]number{
		"Seconds": s.Seconds, "TimeoutSeconds": s.TimeoutSeconds, "HeartbeatSeconds": s.HeartbeatSeconds,
		"MaxConcurrency": s.MaxConcurrency,
	} {
		if _, ok := expression(n.expr); n.expr != "" && !ok {
			add("SCHEMA_VALIDATION_FAILED: %s must be a number or a JSONata expression at %s/%s", name, at, name)
		}

		checkJSONata(n.expr, at+"/"+name, add)
	}

	for name, v := range map[string]any{
		"Arguments": s.Arguments, fieldOutput: s.Output, fieldItems: s.Items, "ItemSelector": s.ItemSelector,
		fieldAssign: s.Assign, "Error": s.Error, "Cause": s.Cause, "Timestamp": s.Timestamp,
	} {
		checkJSONata(v, at+"/"+name, add)
	}

	for i, c := range s.Catch {
		catchAt := fmt.Sprintf("%s/Catch[%d]", at, i)
		if c.ResultPath.set {
			add("SCHEMA_VALIDATION_FAILED: The field 'ResultPath' is not supported by JSONata states at %s", catchAt)
		}

		checkJSONata(c.Output, catchAt+"/Output", add)
		checkJSONata(c.Assign, catchAt+"/Assign", add)
	}
}

// checkCondition validates a JSONata choice rule: a Condition expression and
// a Next.
func checkCondition(rule map[string]json.RawMessage, at string, add func(string, ...any)) {
	var text string
	if _, ok := expression(member(rule, "Condition")); !ok || json.Unmarshal(rule["Condition"], &text) != nil {
		add("SCHEMA_VALIDATION_FAILED: JSONata choice rules need a Condition expression at %s", at)
		return
	}

	if _, ok := rule["Next"]; !ok {
		add("SCHEMA_VALIDATION_FAILED: Choice rules need 'Next' at %s", at)
	}

	for k := range rule {
		if k != "Condition" && k != "Next" && k != "Assign" && k != "Output" && k != "Comment" {
			add("SCHEMA_VALIDATION_FAILED: The field '%s' is not supported by JSONata choice rules at %s", k, at)
		}
	}

	checkJSONata(text, at+"/Condition", add)
	checkJSONata(member(rule, fieldOutput), at+"/Output", add)
	checkJSONata(member(rule, fieldAssign), at+"/Assign", add)
}

// checkRule validates a choice rule: a comparison of a Variable, or And, Or,
// or Not of rules. Top-level rules need Next.
func checkRule(rule map[string]json.RawMessage, top bool, at string, add func(string, ...any)) {
	var boolean string
	for _, op := range []string{"And", "Or", "Not"} {
		if _, ok := rule[op]; ok {
			boolean = op
		}
	}

	if _, hasNext := rule["Next"]; top != hasNext {
		add("SCHEMA_VALIDATION_FAILED: Only top-level choice rules have 'Next' at %s", at)
	}

	switch boolean {
	case "And", "Or":
		var nested []map[string]json.RawMessage
		if json.Unmarshal(rule[boolean], &nested) != nil || len(nested) == 0 {
			add("SCHEMA_VALIDATION_FAILED: %s needs a list of rules at %s", boolean, at)
		}

		for i, n := range nested {
			checkRule(n, false, fmt.Sprintf("%s/%s[%d]", at, boolean, i), add)
		}
	case "Not":
		var nested map[string]json.RawMessage
		if json.Unmarshal(rule["Not"], &nested) != nil {
			add("SCHEMA_VALIDATION_FAILED: Not needs a rule at %s", at)
		}

		checkRule(nested, false, at+"/Not", add)
	default:
		if _, ok := rule["Variable"]; !ok {
			add("SCHEMA_VALIDATION_FAILED: Choice rules need a Variable at %s", at)
		}

		found := 0
		for k := range rule {
			if comparators[strings.TrimSuffix(k, "Path")] {
				found++
			}
		}

		if found != 1 {
			add("SCHEMA_VALIDATION_FAILED: Choice rules need exactly one comparison operator at %s", at)
		}
	}
}
