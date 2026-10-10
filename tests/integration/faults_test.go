package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/faults"
)

func faultAPI(t *testing.T, s *httptest.Server, method, path, body, origin string, status int) []byte {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status {
		t.Fatalf("%s %s: status %d, body %s, error %v", method, path, response.StatusCode, data, err)
	}

	return data
}

func fastFaultRetryer() aws.Retryer {
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = 3
		o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
	})
}

func TestFaultSDKRetries(t *testing.T) {
	for _, tc := range []struct {
		service, operation, code string
		status                   int
	}{
		{"dynamodb", "ListTables", "ThrottlingException", 400},
		{"dynamodb", "ListTables", "ProvisionedThroughputExceededException", 400},
		{"dynamodb", "ListTables", "UnexpectedFailure", 500},
		{"dynamodb", "ListTables", "UnexpectedFailure", 503},
		{"sns", "ListTopics", "ThrottlingException", 400},
		{"s3", "ListBuckets", "SlowDown", 503},
		{"sqs", "ListQueues", "ThrottlingException", 400},
		{"cloudwatch", "ListMetrics", "UnexpectedFailure", 503},
		{"ec2", "DescribeVpcs", "UnexpectedFailure", 503},
		{"lambda", "ListFunctions", "UnexpectedFailure", 503},
		{"eventbridge", "ListRules", "ThrottlingException", 400},
	} {
		t.Run(tc.service+tc.code+http.StatusText(tc.status), func(t *testing.T) {
			s := server(t)
			rules := []faults.Rule{{
				Service: tc.service, Operation: tc.operation,
				Error: &faults.Error{Code: tc.code, StatusCode: tc.status, Message: "retry me"},
			}}
			if tc.service == "eventbridge" {
				rules[0].Service = "events"
			}

			body, err := json.Marshal(rules)
			if err != nil {
				t.Fatal(err)
			}

			faultAPI(t, s, http.MethodPost, "/_tinystack/faults", string(body), "", http.StatusOK)
			switch tc.service {
			case "dynamodb":
				c := sdk.New(
					client(s, "test", "us-east-1").Options(),
					func(o *sdk.Options) { o.Retryer = fastFaultRetryer() },
				)
				_, err = c.ListTables(context.Background(), &sdk.ListTablesInput{})
			case "sns":
				c := sns.New(snsClient(s, "test").Options(), func(o *sns.Options) { o.Retryer = fastFaultRetryer() })
				_, err = c.ListTopics(context.Background(), &sns.ListTopicsInput{})
			case "sqs":
				c := sqs.New(sqsClient(s, "test").Options(), func(o *sqs.Options) { o.Retryer = fastFaultRetryer() })
				_, err = c.ListQueues(context.Background(), &sqs.ListQueuesInput{})
			case "cloudwatch":
				c := cloudwatch.New(
					cloudwatchClient(s).Options(),
					func(o *cloudwatch.Options) { o.Retryer = fastFaultRetryer() },
				)
				_, err = c.ListMetrics(context.Background(), &cloudwatch.ListMetricsInput{})
			case "ec2":
				c := ec2.New(
					ec2Client(s, "test", "us-east-1").Options(),
					func(o *ec2.Options) { o.Retryer = fastFaultRetryer() },
				)
				_, err = c.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{})
			case "eventbridge":
				c := eventbridge.New(
					eventsClient(s, "test").Options(),
					func(o *eventbridge.Options) { o.Retryer = fastFaultRetryer() },
				)
				_, err = c.ListRules(context.Background(), &eventbridge.ListRulesInput{})
			case "lambda":
				c := sdklambda.New(
					lambdaClient(s, "test").Options(),
					func(o *sdklambda.Options) { o.Retryer = fastFaultRetryer() },
				)
				_, err = c.ListFunctions(context.Background(), &sdklambda.ListFunctionsInput{})
			case "s3":
				c := s3Client(s, "test", "us-east-1", func(o *s3.Options) { o.Retryer = fastFaultRetryer() })
				_, err = c.ListBuckets(context.Background(), &s3.ListBucketsInput{})
			}

			code(t, err, tc.code)
			_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
			if len(records) != 3 {
				t.Fatalf("SDK made %d attempts, want 3: %+v", len(records), records)
			}

			for _, record := range records {
				wantProtocol := map[string]string{
					"dynamodb": catalog.AWSJSON10, "sqs": catalog.AWSJSON10, "sns": catalog.AWSQuery,
					"s3": catalog.RESTXML, "cloudwatch": catalog.RPCV2CBOR, "ec2": catalog.EC2Query,
					"lambda": catalog.RESTJSON, "eventbridge": catalog.AWSJSON11,
				}[tc.service]
				if !record.Injected || record.Protocol != wantProtocol || record.Status != tc.status ||
					record.ErrorMessage != "retry me" {
					t.Fatalf("fault record: %+v", record)
				}
			}

			metrics := faultAPI(t, s, http.MethodGet, "/_tinystack/metrics", "", "", http.StatusOK)
			if !strings.Contains(string(metrics), "tinystack_injected_requests_total{") ||
				!strings.Contains(string(metrics), "} 3\n") {
				t.Fatalf("injected metrics: %s", metrics)
			}
		})
	}
}

func TestFaultMatchingLatencyAndCancellation(t *testing.T) {
	s := server(t)
	path := "/_tinystack/faults"
	faultAPI(t, s, http.MethodPost, path,
		`[{"operation":"ListTables","region":"eu-west-1","latencyMs":60}]`, "", http.StatusOK)
	c := client(s, "test", "eu-west-1")
	start := time.Now()
	if _, err := c.ListTables(context.Background(), &sdk.ListTablesInput{}); err != nil {
		t.Fatal(err)
	}

	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("latency was %s", elapsed)
	}

	if _, listErr := client(
		s,
		"test",
		"us-east-1",
	).ListTables(context.Background(), &sdk.ListTablesInput{}); listErr != nil {
		t.Fatal(listErr)
	}

	_, err := c.DescribeTable(context.Background(), &sdk.DescribeTableInput{TableName: aws.String("missing")})
	code(t, err, "ResourceNotFoundException")
	_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	if !records[2].Injected || records[1].Injected || records[0].Injected {
		t.Fatalf("matching records: %+v", records)
	}

	faultAPI(t, s, http.MethodPost, path,
		`[{"probability":0,"error":{"code":"ThrottlingException"}},{"error":{"code":"SlowDown"}}]`, "", http.StatusOK)
	for range 10 {
		if _, listErr := c.ListTables(context.Background(), &sdk.ListTablesInput{}); listErr != nil {
			t.Fatal(listErr)
		}
	}

	faultAPI(t, s, http.MethodPost, path, `[{"operation":"CreateTable","latencyMs":2000}]`, "", http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = create(ctx, c, "cancelled")
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("cancellation: %v after %s", err, time.Since(start))
	}

	// Wait beyond the configured delay: a cancelled sleep must never invoke CreateTable.
	time.Sleep(2100 * time.Millisecond)
	_, err = c.DescribeTable(context.Background(), &sdk.DescribeTableInput{TableName: aws.String("cancelled")})
	code(t, err, "ResourceNotFoundException")
}

func TestFaultRuntimeAPIAndReset(t *testing.T) {
	cfg := testConfig()
	cfg.Faults = []faults.Rule{{Service: "s3", Error: &faults.Error{Code: "SlowDown"}}}
	s := serverConfig(t, cfg, &fakeRuntime{})
	path := "/_tinystack/faults"
	get := func(want int) {
		t.Helper()
		data := faultAPI(t, s, http.MethodGet, path, "", "", http.StatusOK)
		var rules []faults.Rule
		if err := json.Unmarshal(data, &rules); err != nil || len(rules) != want {
			t.Fatalf("rules %s, want %d: %v", data, want, err)
		}
	}
	get(1)
	faultAPI(t, s, http.MethodPost, path, `[{"latencyMs":1}]`, "", http.StatusOK)
	get(1)
	faultAPI(t, s, http.MethodPatch, path, `[{"service":"sns","latencyMs":2}]`, "", http.StatusOK)
	get(2)
	faultAPI(t, s, http.MethodDelete, path, `[{"latencyMs":1}]`, "", http.StatusOK)
	get(1)
	faultAPI(t, s, http.MethodDelete, path, `[]`, "", http.StatusOK)
	get(0)
	for _, body := range []string{
		`null`, `{}`, `[{}]`, `[{"latencyMs":-1}]`,
		`[{"latencyMs":1,"probability":2}]`, `[{"error":{"code":"X","statusCode":200}}]`,
		`[{"error":{"code":""}}]`, `[{"latencyMs":1,"unknown":true}]`, `[] []`,
	} {
		faultAPI(t, s, http.MethodPost, path, body, "", http.StatusBadRequest)
	}

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		faultAPI(t, s, method, path, `[]`, "https://evil.example", http.StatusForbidden)
	}

	faultAPI(t, s, http.MethodPut, path, `[]`, "", http.StatusMethodNotAllowed)
	faultAPI(t, s, http.MethodPost, "/_localstack/chaos/faults", `[{}]`, "", http.StatusOK)
	_, err := client(s, "test", "us-east-1").ListTables(context.Background(), &sdk.ListTablesInput{})
	code(t, err, "ServiceUnavailable")
	faultAPI(t, s, http.MethodPost, "/_localstack/chaos/effects", `{"latency":40}`, "", http.StatusOK)
	faultAPI(t, s, http.MethodPost, "/_tinystack/reset", "", "", http.StatusOK)
	get(1)
	effects := faultAPI(t, s, http.MethodGet, "/_localstack/chaos/effects", "", "", http.StatusOK)
	if !strings.Contains(string(effects), `"latency":0`) {
		t.Fatalf("reset effects: %s", effects)
	}

	if _, listErr := client(
		s,
		"test",
		"us-east-1",
	).ListTables(context.Background(), &sdk.ListTablesInput{}); listErr != nil {
		t.Fatal(listErr)
	}

	_, err = s3Client(s, "test", "us-east-1").ListBuckets(context.Background(), &s3.ListBucketsInput{})
	code(t, err, "SlowDown")
}

func TestFaultDebugAndConsoleExclusions(t *testing.T) {
	s := server(t)
	faultAPI(t, s, http.MethodPost, "/_localstack/chaos/effects", `{"latency":60}`, "", http.StatusOK)
	faultAPI(
		t,
		s,
		http.MethodPost,
		"/_tinystack/faults",
		`[{"error":{"code":"ThrottlingException"}}]`,
		"",
		http.StatusOK,
	)
	for _, path := range []string{"/_tinystack/health", "/_localstack/health", "/_tinystack/actions"} {
		faultAPI(t, s, http.MethodGet, path, "", "", http.StatusOK)
	}

	faultAPI(t, s, http.MethodGet, "/_tinystack/unknown", "", "", http.StatusNotFound)
	faultAPI(t, s, http.MethodGet, "/_localstack/unknown", "", "", http.StatusNotFound)
	faultAPI(t, s, http.MethodPost, "/_tinystack/actions",
		`{"service":"dynamodb","operation":"ListTables","account":"000000000000","region":"us-east-1","input":{}}`,
		"", http.StatusOK)
	_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	if len(records) != 1 || records[0].Injected || records[0].Protocol != "console" {
		t.Fatalf("console records: %+v", records)
	}
}

func TestFaultConfigValidation(t *testing.T) {
	for _, rule := range []faults.Rule{
		{LatencyMS: -1},
		{LatencyMS: 1, Probability: new(1.1)},
		{Error: &faults.Error{Code: "X", StatusCode: 600}},
		{LatencyMS: 1, Account: "123"},
	} {
		cfg := config.Default()
		cfg.Faults = []faults.Rule{rule}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "faults[0]") {
			t.Fatalf("invalid config accepted: %+v, %v", rule, err)
		}
	}
}

func TestFaultInternalCallerExclusion(t *testing.T) {
	s := server(t)
	faultAPI(t, s, http.MethodPost, "/_tinystack/faults",
		`[{"service":"sqs","error":{"code":"ThrottlingException"}}]`, "", http.StatusOK)
	c := cfnClient(s)
	_, err := c.CreateStack(context.Background(), &cfn.CreateStackInput{
		StackName:    aws.String("fault-caller"),
		TemplateBody: aws.String(`{"Resources":{"Queue":{"Type":"AWS::SQS::Queue"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(context.Background(), t, c, "fault-caller", cfntypes.StackStatusCreateComplete)
	// CloudFormation used catalog.Caller to create the queue despite the SQS fault.
	_, err = sqsClient(s, "test").ListQueues(context.Background(), &sqs.ListQueuesInput{})
	code(t, err, "ThrottlingException")
}

func TestFaultLatencyPreservesServiceErrors(t *testing.T) {
	s := server(t)
	faultAPI(t, s, http.MethodPost, "/_tinystack/faults", `[{"service":"s3","latencyMs":1}]`, "", http.StatusOK)
	_, err := s3Client(s, "test", "us-east-1").GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("missing"), Key: aws.String("missing"),
	})
	gotCode, status := s3Code(err)
	if gotCode != "NoSuchBucket" || status != http.StatusNotFound {
		t.Fatalf("latency altered real service error: %v, status %d", err, status)
	}
}

func TestFaultYAMLConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.yaml")
	body := `faults:
  - service: dynamodb
    operation: ListTables
    probability: 0
    latencyMs: 1
    error:
      code: ThrottlingException
      statusCode: 400
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil || len(cfg.Faults) != 1 || cfg.Faults[0].Probability == nil || *cfg.Faults[0].Probability != 0 {
		t.Fatalf("YAML faults: %+v, %v", cfg.Faults, err)
	}

	s := serverConfig(t, cfg, &fakeRuntime{})
	if _, listErr := client(
		s,
		"test",
		"us-east-1",
	).ListTables(context.Background(), &sdk.ListTablesInput{}); listErr != nil {
		t.Fatal(listErr)
	}

	if err := os.WriteFile(path, []byte("faults:\n  - latencyMs: -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "latencyMs") {
		t.Fatalf("invalid YAML configuration: %v", err)
	}
}

func TestFaultLocalStackAliases(t *testing.T) {
	s := server(t)
	path := "/_localstack/chaos/faults"
	faultAPI(t, s, http.MethodPost, path,
		`[{"service":"dynamodb","operation":"ListTables","region":"us-east-1","probability":1,
 "error":{"statusCode":400,"code":"ThrottlingException"}}]`, "", http.StatusOK)
	_, err := client(s, "test", "us-east-1").ListTables(context.Background(), &sdk.ListTablesInput{})
	code(t, err, "ThrottlingException")
	faultAPI(t, s, http.MethodPatch, path, `[{"service":"sns"}]`, "", http.StatusOK)
	faultAPI(t, s, http.MethodDelete, path, `[{"service":"sns"}]`, "", http.StatusOK)
	faultAPI(t, s, http.MethodPost, path, `[]`, "", http.StatusOK)
	effects := "/_localstack/chaos/effects"
	faultAPI(t, s, http.MethodPost, effects, `{"latency":60}`, "", http.StatusOK)
	start := time.Now()
	if _, listErr := client(
		s,
		"test",
		"us-east-1",
	).ListTables(context.Background(), &sdk.ListTablesInput{}); listErr != nil {
		t.Fatal(listErr)
	}

	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("global latency was %s", elapsed)
	}

	for _, body := range []string{`null`, `[]`, `{"latency":-1}`, `{"latency":3600001}`, `{"other":1}`} {
		faultAPI(t, s, http.MethodPost, effects, body, "", http.StatusBadRequest)
	}

	faultAPI(t, s, http.MethodPost, effects, `{"latency":0}`, "https://evil.example", http.StatusForbidden)
	faultAPI(t, s, http.MethodPost, path, `[]`, "https://evil.example", http.StatusForbidden)
	faultAPI(t, s, http.MethodPost, effects, `{"latency":0}`, "", http.StatusOK)
}
