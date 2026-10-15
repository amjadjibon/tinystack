package shell_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	dynamodbapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	eventsapi "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	rdsapi "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	stsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/shell"
)

func newShell() *shell.Shell {
	return shell.New([]catalog.Service{
		sqsapi.Descriptor(nil), dynamodbapi.Descriptor(nil), s3api.Descriptor(nil), stsapi.Descriptor(nil),
		eventsapi.Descriptor(nil), rdsapi.Descriptor(nil),
	})
}

func TestTokenize(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{`aws sqs list-queues`, []string{"aws", "sqs", "list-queues"}},
		{`  a   b  `, []string{"a", "b"}},
		{`--attributes '{"A":"1 2"}'`, []string{"--attributes", `{"A":"1 2"}`}},
		{`--m "say \"hi\" \\ \$x"`, []string{"--m", `say "hi" \ $x`}},
		{`--m "a\nb"`, []string{"--m", `a\nb`}},
		{`a\ b 'c'd"e"`, []string{"a b", "cde"}},
		{`--empty ''`, []string{"--empty", ""}},
		{`'a $b ` + "`c`" + ` | d'`, []string{"a $b `c` | d"}},
		{`"cost: $ 5"`, []string{"cost: $ 5"}},
	}
	for _, c := range cases {
		tokens, err := shell.Tokenize(c.line)
		if err != nil {
			t.Fatalf("%q: %v", c.line, err)
		}

		var got []string
		for _, token := range tokens {
			got = append(got, token.Text)
		}

		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestTokenizeQuotedFlagIsValue(t *testing.T) {
	tokens, err := shell.Tokenize(`--a '--b' --c`)
	if err != nil || len(tokens) != 3 || tokens[0].Quoted || !tokens[1].Quoted || tokens[2].Quoted {
		t.Fatalf("tokens = %+v %v", tokens, err)
	}
}

func TestTokenizeRejects(t *testing.T) {
	for line, want := range map[string]string{
		`aws s3api list-buckets | jq .`: "pipes",
		`aws a > out`:                   "redirects",
		`a && b`:                        "command lists",
		`a; b`:                          "command lists",
		`echo $(date)`:                  "variables",
		`echo $HOME`:                    "variables",
		`echo "$HOME"`:                  "variables",
		"echo `date`":                   "pipes",
		`echo "unclosed`:                "unterminated double quote",
		`echo 'unclosed`:                "unterminated single quote",
		`echo \`:                        "backslash",
	} {
		_, err := shell.Tokenize(line)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error = %v, want %q", line, err, want)
		}
	}

	if _, err := shell.Tokenize(`a | b`); err == nil || !strings.Contains(err.Error(), "aws-local") {
		t.Errorf("error = %v, want a pointer to aws-local", err)
	}
}

func TestParseMapsNamesAndValues(t *testing.T) {
	sh := newShell()
	cmd, err := sh.Parse(`aws sqs create-queue --queue-name q --attributes '{"DelaySeconds":"5"}' --tags a=b,c=d`)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"QueueName": "q", "Attributes": map[string]any{"DelaySeconds": "5"},
		"Tags": map[string]any{"a": "b", "c": "d"},
	}
	if cmd.Service != "sqs" || cmd.Operation != "CreateQueue" || !reflect.DeepEqual(cmd.Input, want) {
		t.Fatalf("command = %+v", cmd)
	}
}

func TestParseServiceAndOperationNames(t *testing.T) {
	sh := newShell()
	for line, want := range map[string][2]string{
		`aws s3api list-buckets`:                   {"s3", "ListBuckets"},
		`aws s3api list-objects-v2 --bucket b`:     {"s3", "ListObjectsV2"},
		`aws events list-event-buses`:              {"eventbridge", "ListEventBuses"},
		`aws eventbridge list-event-buses`:         {"eventbridge", "ListEventBuses"},
		`aws rds describe-db-instances`:            {"rds", "DescribeDBInstances"},
		`aws dynamodb list-tables`:                 {"dynamodb", "ListTables"},
		`aws sts get-caller-identity`:              {"sts", "GetCallerIdentity"},
		`whoami`:                                   {"sts", "GetCallerIdentity"},
		`aws dynamodb listtables --output text`:    {"dynamodb", "ListTables"},
		`aws rds describe-db-instances --region x`: {"rds", "DescribeDBInstances"},
	} {
		cmd, err := sh.Parse(line)
		if err != nil || cmd.Service != want[0] || cmd.Operation != want[1] {
			t.Errorf("%q = %+v %v, want %v", line, cmd, err, want)
		}
	}
}

func TestParseScalarsListsAndBooleans(t *testing.T) {
	sh := newShell()
	cmd, err := sh.Parse(`aws sqs receive-message --queue-url http://q --max-number-of-messages 5 ` +
		`--attribute-names All Foo --message-attribute-names '["A"]'`)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"QueueUrl": "http://q", "MaxNumberOfMessages": float64(5),
		"AttributeNames": []any{"All", "Foo"}, "MessageAttributeNames": []any{"A"},
	}
	if !reflect.DeepEqual(cmd.Input, want) {
		t.Fatalf("input = %#v", cmd.Input)
	}

	cmd, err = sh.Parse(`aws s3api put-object --bucket b --key k --body hello`)
	if err != nil || cmd.Input["Body"] != "aGVsbG8=" {
		t.Fatalf("blob = %+v %v", cmd, err)
	}

	cmd, err = sh.Parse(`aws sqs list-queues --max-results=3 --region eu-west-1 --output text`)
	if err != nil || cmd.Input["MaxResults"] != float64(3) || cmd.Region != "eu-west-1" || cmd.Output != "text" {
		t.Fatalf("global options = %+v %v", cmd, err)
	}
}

func TestParseBooleans(t *testing.T) {
	sh := newShell()
	for line, want := range map[string]bool{
		`aws dynamodb scan --table-name t --consistent-read`:          true,
		`aws dynamodb scan --table-name t --consistent-read true`:     true,
		`aws dynamodb scan --table-name t --consistent-read false`:    false,
		`aws dynamodb scan --table-name t --no-consistent-read`:       false,
		`aws dynamodb scan --table-name t --consistent-read=false`:    false,
		`aws dynamodb scan --consistent-read --table-name t`:          true,
		`aws dynamodb scan --table-name t --no-consistent-read false`: false,
	} {
		cmd, err := sh.Parse(line)
		if line == `aws dynamodb scan --table-name t --no-consistent-read false` {
			if err == nil {
				t.Errorf("%q: want an error for a negated flag with a value", line)
			}

			continue
		}

		if err != nil || cmd.Input["ConsistentRead"] != want {
			t.Errorf("%q = %+v %v, want %v", line, cmd, err, want)
		}
	}
}

func TestParseCLIInputJSON(t *testing.T) {
	sh := newShell()
	cmd, err := sh.Parse(`aws dynamodb put-item --cli-input-json '{"TableName":"a","Item":{"id":{"S":"1"}}}' ` +
		`--table-name b`)
	if err != nil {
		t.Fatal(err)
	}

	item := map[string]any{"id": map[string]any{"S": "1"}}
	if cmd.Input["TableName"] != "b" || !reflect.DeepEqual(cmd.Input["Item"], item) {
		t.Fatalf("input = %#v", cmd.Input)
	}
}

func TestParseErrors(t *testing.T) {
	sh := newShell()
	for line, want := range map[string]string{
		`ls`:                                                 "command not found",
		`aws nosuch list`:                                    "not a service",
		`aws sqs nosuch`:                                     "not an operation",
		`aws s3 ls`:                                          "High-level s3 commands",
		`aws sqs list-queues --nosuch 1`:                     "Unknown option: --nosuch",
		`aws sqs list-queues extra`:                          "Unexpected argument",
		`aws sqs list-queues --max-results x`:                "not an integer",
		`aws sqs list-queues --max-results`:                  "needs a value",
		`aws sqs list-queues --output table`:                 "not supported yet",
		`aws sqs list-queues --output xml`:                   "must be json or text",
		`aws sqs list-queues --query Queues`:                 "--query is not supported",
		`aws sqs list-queues --profile p`:                    "not supported",
		`aws sqs create-queue --attributes {bad`:             "not valid JSON",
		`aws sqs create-queue --attributes a`:                "neither a JSON object",
		`aws sqs create-queue --queue-name a --queue-name b`: "more than once",
		`aws s3api put-object --body file://x`:               "no files",
		`aws sqs list-queues --max-results 1 2`:              "Unexpected argument",
		`aws sqs list-queues --cli-input-json [1]`:           "must be a JSON object",
		`aws sqs list-queues --region`:                       "needs a value",
		`whoami now`:                                         "no arguments",
	} {
		_, err := sh.Parse(line)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error = %v, want %q", line, err, want)
		}
	}
}

func TestHelpAndBuiltins(t *testing.T) {
	sh := newShell()
	for line, want := range map[string]string{
		`help`:                         "s3api",
		`aws`:                          "Services:",
		`aws help`:                     "Services:",
		`aws sqs help`:                 "create-queue",
		`aws sqs create-queue help`:    "--queue-name (string)",
		`aws sqs create-queue --help2`: "",
	} {
		cmd, err := sh.Parse(line)
		if want == "" {
			if err == nil {
				t.Errorf("%q: want an error", line)
			}

			continue
		}

		if err != nil || !strings.Contains(cmd.Text, want) {
			t.Errorf("%q = %q %v, want %q", line, cmd.Text, err, want)
		}
	}

	if cmd, err := sh.Parse(`clear`); err != nil || cmd.Builtin != "clear" {
		t.Errorf("clear = %+v %v", cmd, err)
	}

	if cmd, err := sh.Parse(`   `); err != nil || cmd.Service != "" || cmd.Text != "" || cmd.Builtin != "" {
		t.Errorf("empty = %+v %v", cmd, err)
	}
}

func TestComplete(t *testing.T) {
	sh := newShell()
	for line, want := range map[string][]string{
		`aw`:                       {"aws"},
		`aws sq`:                   {"sqs"},
		`aws sqs create-q`:         {"create-queue"},
		`aws sqs create-queue --q`: {"--queue-name"},
		`aws sqs create-queue --o`: {"--output"},
	} {
		if got := sh.Complete(line); !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %q, want %q", line, got, want)
		}
	}

	if got := sh.Complete(`aws s`); !reflect.DeepEqual(got, []string{"s3api", "sqs", "sts"}) {
		t.Errorf("services = %q", got)
	}

	if got := sh.Complete(`aws sqs `); len(got) < 10 || got[0] != "add-permission" {
		t.Errorf("operations = %q", got)
	}
}

func TestFormat(t *testing.T) {
	output := map[string]any{
		"Account": "123", "Arn": "arn:x", "Count": float64(2), "Ok": true,
		"Queues": []any{"a", "b"},
		"Items":  []any{map[string]any{"N": "1", "Size": float64(5)}},
	}
	got, err := shell.Format(&shell.Command{Output: "json"}, output)
	if err != nil || !strings.Contains(got, "\n    \"Account\": \"123\"") || !strings.HasSuffix(got, "}\n") {
		t.Fatalf("json = %q %v", got, err)
	}

	got, err = shell.Format(&shell.Command{Output: "text"}, output)
	want := "123\tarn:x\t2\tTrue\nITEMS\t1\t5\nQUEUES\ta\tb\n"
	if err != nil || got != want {
		t.Fatalf("text = %q %v, want %q", got, err, want)
	}

	if got, err = shell.Format(&shell.Command{Output: "json"}, map[string]any{}); err != nil || got != "" {
		t.Fatalf("empty = %q %v", got, err)
	}
}
