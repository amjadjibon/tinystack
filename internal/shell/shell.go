package shell

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// cliNames are the AWS CLI command names of the services whose catalog names
// differ. Every service is also reachable by its catalog name.
var cliNames = map[string]string{
	"acmpca":                    "acm-pca",
	"applicationautoscaling":    "application-autoscaling",
	"cognitoidentity":           "cognito-identity",
	"cognitoidp":                "cognito-idp",
	"config":                    "configservice",
	"costandusagereportservice": "cur",
	"costexplorer":              "ce",
	"emrserverless":             "emr-serverless",
	"eventbridge":               "events",
	"iotdataplane":              "iot-data",
	"iotjobsdataplane":          "iot-jobs-data",
	"s3":                        "s3api",
	"sfn":                       "stepfunctions",
	"ssoadmin":                  "sso-admin",
	"timestreamwrite":           "timestream-write",
}

const (
	wordAWS    = "aws"
	wordHelp   = "help"
	formatJSON = "json"
	formatText = "text"
)

// Shell resolves command lines against the catalog's services.
type Shell struct {
	byName map[string]*entry
	names  []string
}

// entry is one service as the shell names it.
type entry struct {
	service *catalog.Service
	cli     string
	ops     map[string]*catalog.Operation
	names   []string
}

// Command is a parsed command line.
type Command struct {
	// Input holds the operation's members keyed by their SDK names.
	Input map[string]any
	// Text is the whole output of a command that calls no operation, such as help.
	Text string
	// Builtin names a command the terminal itself runs: "clear" or "history".
	Builtin string
	// Service and Operation are the catalog's names, empty for a builtin.
	Service   string
	Operation string
	// Region overrides the session's Region when set.
	Region string
	// Output is "json" or "text".
	Output string
}

// UsageError is a command line the shell cannot run as written.
type UsageError struct{ Message string }

func (e *UsageError) Error() string { return e.Message }

func usage(format string, args ...any) error {
	return &UsageError{Message: fmt.Sprintf(format, args...)}
}

// New returns a Shell for services.
func New(services []catalog.Service) *Shell {
	sh := &Shell{byName: map[string]*entry{}}
	for i := range services {
		service := &services[i]
		e := &entry{service: service, cli: service.Name, ops: map[string]*catalog.Operation{}}
		if name, ok := cliNames[service.Name]; ok {
			e.cli = name
		}

		for name := range service.Operations {
			op := service.Operations[name]
			e.ops[normalize(name)] = &op
			e.names = append(e.names, kebab(name))
		}

		sort.Strings(e.names)
		sh.byName[e.cli] = e
		sh.byName[service.Name] = e
		sh.names = append(sh.names, e.cli)
	}

	sort.Strings(sh.names)

	return sh
}

// Parse reads a command line. A line that is empty yields an empty Command.
// Errors are *UsageError values, except for those of Tokenize.
func (sh *Shell) Parse(line string) (*Command, error) {
	tokens, err := Tokenize(line)
	if err != nil {
		return nil, usage("%s", err)
	}

	if len(tokens) == 0 {
		return &Command{}, nil
	}

	switch word := tokens[0].Text; word {
	case wordHelp:
		return &Command{Text: sh.Help()}, nil
	case "clear", "history":
		return &Command{Builtin: word}, nil
	case "whoami":
		if len(tokens) > 1 {
			return nil, usage("whoami takes no arguments")
		}

		return &Command{
			Service:   "sts",
			Operation: "GetCallerIdentity",
			Input:     map[string]any{},
			Output:    formatJSON,
		}, nil
	case wordAWS:
		return sh.parseAWS(tokens[1:])
	default:
		return nil, usage("%s: command not found. Commands are aws, help, whoami, history and clear; "+
			"type help for more.", word)
	}
}

func (sh *Shell) parseAWS(args []Token) (*Command, error) {
	if len(args) == 0 || (len(args) == 1 && args[0].Text == wordHelp) {
		return &Command{Text: sh.Help()}, nil
	}

	e := sh.byName[args[0].Text]
	if e == nil {
		return nil, usage("'%s' is not a service the CloudShell knows.%s", args[0].Text,
			suggest(args[0].Text, sh.names))
	}

	if len(args) == 1 || args[1].Text == wordHelp {
		return &Command{Text: e.help()}, nil
	}

	op := e.ops[normalize(args[1].Text)]
	if op == nil {
		hint := suggest(args[1].Text, e.names)
		if e.service.Name == "s3" {
			hint = " High-level s3 commands (ls, cp, sync, mb) are not supported; use aws s3api <operation>." + hint
		}

		return nil, usage("'%s' is not an operation of %s.%s", args[1].Text, e.cli, hint)
	}

	cmd := &Command{Service: e.service.Name, Operation: op.Name, Output: formatJSON}
	if len(args) == 3 && args[2].Text == wordHelp && !args[2].Quoted {
		cmd.Text = operationHelp(e, op)
		return cmd, nil
	}

	input, err := parseFlags(e, op, cmd, args[2:])
	if err != nil {
		return nil, err
	}

	cmd.Input = input

	return cmd, nil
}

// Help is the text of the help command.
func (sh *Shell) Help() string {
	var b strings.Builder
	b.WriteString("TinyStack CloudShell: AWS CLI-style commands run in-process with your console identity.\n\n")
	b.WriteString("  aws <service> <operation> [--member value ...] [--region r] [--output json|text]\n")
	b.WriteString("  aws <service> help               list a service's operations\n")
	b.WriteString("  aws <service> <operation> help   list an operation's options\n")
	b.WriteString("  --cli-input-json '{...}'         set members from a JSON object\n")
	b.WriteString("  whoami  history  clear  help\n\n")
	b.WriteString("Lists take space-separated values; structures, maps and lists of them take JSON.\n")
	b.WriteString("Pipes, redirects, variables and && are not supported: run aws-local in a real shell.\n\n")
	b.WriteString("Services:\n")

	for _, name := range sh.names {
		b.WriteString("  " + name + "\n")
	}

	return b.String()
}

func (e *entry) help() string {
	var b strings.Builder
	fmt.Fprintf(&b, "aws %s <operation>\n\nOperations:\n", e.cli)

	for _, name := range e.names {
		b.WriteString("  " + name + "\n")
	}

	return b.String()
}

// Services lists the CLI names of the services, sorted.
func (sh *Shell) Services() []string { return slices.Clone(sh.names) }

// Complete lists what could finish the last word of line: services, then
// operations, then options. The words are not quoted.
func (sh *Shell) Complete(line string) []string {
	trailing := line == "" || strings.HasSuffix(line, " ") || strings.HasSuffix(line, "\t")
	tokens, err := Tokenize(line)
	if err != nil {
		return nil
	}

	current := ""
	if !trailing && len(tokens) > 0 {
		current = tokens[len(tokens)-1].Text
		tokens = tokens[:len(tokens)-1]
	}

	var candidates []string
	switch len(tokens) {
	case 0:
		candidates = []string{wordAWS, "clear", wordHelp, "history", "whoami"}
	case 1:
		if tokens[0].Text == wordAWS {
			candidates = append([]string{wordHelp}, sh.names...)
		}
	case 2:
		if tokens[0].Text == wordAWS {
			if e := sh.byName[tokens[1].Text]; e != nil {
				candidates = append([]string{wordHelp}, e.names...)
			}
		}
	default:
		if tokens[0].Text != wordAWS || !strings.HasPrefix(current, "-") {
			break
		}

		candidates = []string{"--cli-input-json", "--output", "--region"}
		if e := sh.byName[tokens[1].Text]; e != nil {
			if op := e.ops[normalize(tokens[2].Text)]; op != nil {
				for _, f := range fields(op) {
					candidates = append(candidates, "--"+f.flag)
				}
			}
		}
	}

	var out []string
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, current) {
			out = append(out, candidate)
		}
	}

	sort.Strings(out)

	return slices.Compact(out)
}

// field is one member of an operation's input.
type field struct {
	typ  reflect.Type
	name string
	flag string
}

// fields lists the members of an operation's input.
func fields(op *catalog.Operation) []field {
	t := reflect.TypeOf(op.NewInput()).Elem()
	var out []field
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() || f.Type.String() == "xml.Name" {
			continue
		}

		out = append(out, field{name: f.Name, flag: kebab(f.Name), typ: f.Type})
	}

	return out
}

// kebab names a member or operation as the AWS CLI does: DBInstanceName becomes
// db-instance-name and ListObjectsV2 list-objects-v2.
func kebab(name string) string {
	runes := []rune(name)
	var b strings.Builder
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			acronymEnd := unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || acronymEnd {
				b.WriteByte('-')
			}
		}

		b.WriteRune(unicode.ToLower(r))
	}

	return b.String()
}

// normalize makes names compare equal whatever their hyphens and case.
func normalize(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "-", ""))
}

// suggest names the choices that start like, or contain, word.
func suggest(word string, choices []string) string {
	var near []string
	for _, choice := range choices {
		if strings.Contains(choice, word) || (len(word) > 2 && strings.HasPrefix(choice, word[:3])) {
			near = append(near, choice)
		}

		if len(near) == 5 {
			break
		}
	}

	if len(near) == 0 {
		return " Type help to list them."
	}

	return " Did you mean: " + strings.Join(near, ", ") + "?"
}
