package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	eventbridgeapi "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/gateway"
	provider "github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/eventbridge"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	buckets "github.com/amjadjibon/tinystack/internal/services/s3"
	topics "github.com/amjadjibon/tinystack/internal/services/sns"
	queues "github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func server(t *testing.T) *httptest.Server {
	t.Helper()
	return serverWith(t, &fakeRuntime{})
}

// serverWith builds the server like start.go, with runtime running Lambda code.
func serverWith(t *testing.T, runtime lambda.Runtime) *httptest.Server {
	t.Helper()
	return serverConfig(t, testConfig(), runtime)
}

// testConfig maps the access keys tests use to accounts.
func testConfig() config.Config {
	cfg := config.Default()
	cfg.Identity.Accounts = map[string]string{"team-b": "111111111111", "alias": "000000000000"}
	return cfg
}

// serverConfig builds the server like start.go from cfg.
//
//nolint:gocritic // Tests build each configuration by value.
func serverConfig(t *testing.T, cfg config.Config, runtime lambda.Runtime) *httptest.Server {
	t.Helper()
	s, stop := serverStore(t, cfg, runtime, &memory.Store{})
	t.Cleanup(stop)
	return s
}

// serverStore builds the server like start.go, with every service sharing
// store. stop shuts the server down.
//
//nolint:gocritic // Tests build each configuration by value.
func serverStore(
	t *testing.T,
	cfg config.Config,
	runtime lambda.Runtime,
	store storage.Store,
) (s *httptest.Server, stop func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deliveries := delivery.New()
	sqsService := queues.New(store)
	functions := lambda.New(store, sqsService, runtime, logger, cfg.Identity.SecretKeys, deliveries)
	if err := functions.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	snsService := topics.New(store, sqsService, functions, deliveries)
	h, err := gateway.New(
		cfg,
		logger,
		api.Descriptor(provider.New(store)),
		sqsapi.Descriptor(sqsService),
		snsapi.Descriptor(snsService),
		s3api.Descriptor(buckets.New(store)),
		lambdaapi.Descriptor(functions),
		eventbridgeapi.Descriptor(eventbridge.New(store, sqsService, snsService, functions, deliveries)),
	)
	if err != nil {
		t.Fatal(err)
	}

	h.Handle("deliveries", deliveries)

	s = httptest.NewServer(h)
	return s, func() {
		sqsService.Stop()
		functions.Stop()
		s.Close()
	}
}

func client(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       region,
			BaseEndpoint: aws.String(s.URL),
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
			}),
			Retryer: aws.NopRetryer{},
		},
	)
}

func create(ctx context.Context, c *sdk.Client, name string) (*sdk.CreateTableOutput, error) {
	return c.CreateTable(
		ctx,
		&sdk.CreateTableInput{
			TableName:   aws.String(name),
			BillingMode: types.BillingModePayPerRequest,
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
			},
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
			},
		},
	)
}

func code(t *testing.T, err error, want string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != want {
		t.Fatalf("wanted %s, got %v", want, err)
	}
}

func key(v string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"PK": &types.AttributeValueMemberS{Value: v}}
}

func TestSDKLifecycle(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	created, err := create(ctx, c, "users")
	if err != nil {
		t.Fatal(err)
	}

	if *created.TableDescription.TableArn != "arn:aws:dynamodb:us-east-1:000000000000:table/users" {
		t.Fatal(created.TableDescription)
	}

	_, err = create(ctx, c, "users")
	code(t, err, "ResourceInUseException")
	item := key("one")
	item["emptyMap"] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}}
	item["emptyList"] = &types.AttributeValueMemberL{Value: []types.AttributeValue{}}
	item["false"] = &types.AttributeValueMemberBOOL{Value: false}
	item["null"] = &types.AttributeValueMemberNULL{Value: true}
	item["binary"] = &types.AttributeValueMemberB{Value: []byte{0, 1, 255}}
	item["number"] = &types.AttributeValueMemberN{Value: "12345678901234567890123456789012345678"}
	item["set"] = &types.AttributeValueMemberSS{Value: []string{"a", "b"}}
	if _, err = c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("users"), Item: item}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetItem(
		ctx,
		&sdk.GetItemInput{TableName: aws.String("users"), Key: key("one"), ConsistentRead: aws.Bool(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(item, got.Item) {
		t.Fatalf("round trip changed item: %#v", got.Item)
	}

	old, err := c.PutItem(
		ctx,
		&sdk.PutItemInput{TableName: aws.String("users"), Item: key("one"), ReturnValues: types.ReturnValueAllOld},
	)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(old.Attributes, item) {
		t.Fatal("ALL_OLD mismatch")
	}

	for _, v := range []string{"two", "three"} {
		if _, err = c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("users"), Item: key(v)}); err != nil {
			t.Fatal(err)
		}
	}

	paginator := sdk.NewScanPaginator(c, &sdk.ScanInput{TableName: aws.String("users"), Limit: aws.Int32(1)})
	seen := map[string]bool{}
	for paginator.HasMorePages() {
		page, pageErr := paginator.NextPage(ctx)
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		if page.Count != 1 {
			t.Fatalf("wrong page count: %d", page.Count)
		}

		v := page.Items[0]["PK"].(*types.AttributeValueMemberS).Value
		if seen[v] {
			t.Fatal("duplicate pagination result")
		}

		seen[v] = true
	}

	if len(seen) != 3 {
		t.Fatal(seen)
	}

	count, err := c.Scan(ctx, &sdk.ScanInput{TableName: aws.String("users"), Select: types.SelectCount})
	if err != nil || count.Count != 3 || len(count.Items) != 0 {
		t.Fatalf("count: %v %v", count, err)
	}

	desc, err := c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: aws.String("users")})
	if err != nil || *desc.Table.ItemCount != 3 {
		t.Fatalf("describe: %v %v", desc, err)
	}

	deleted, err := c.DeleteItem(
		ctx,
		&sdk.DeleteItemInput{TableName: aws.String("users"), Key: key("one"), ReturnValues: types.ReturnValueAllOld},
	)
	if err != nil || len(deleted.Attributes) == 0 {
		t.Fatalf("delete: %v %v", deleted, err)
	}

	missing, err := c.GetItem(ctx, &sdk.GetItemInput{TableName: aws.String("users"), Key: key("one")})
	if err != nil || len(missing.Item) != 0 {
		t.Fatalf("missing item: %v %v", missing, err)
	}

	if _, err = c.DeleteTable(ctx, &sdk.DeleteTableInput{TableName: aws.String("users")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: aws.String("users")})
	code(t, err, "ResourceNotFoundException")
}

func TestIdentityAndListPagination(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	for _, name := range []string{"aaa", "bbb", "ccc"} {
		if _, err := create(ctx, c, name); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name, key, region string
		count             int
	}{
		{"default", "test", "us-east-1", 3},
		{"alias", "alias", "us-east-1", 3},
		{"unmapped", "unmapped", "us-east-1", 3},
		{"other account", "team-b", "us-east-1", 0},
		{"other region", "test", "eu-west-1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := sdk.NewListTablesPaginator(client(s, tc.key, tc.region), &sdk.ListTablesInput{Limit: aws.Int32(1)})
			var names []string
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					t.Fatal(err)
				}

				names = append(names, page.TableNames...)
			}

			if len(names) != tc.count {
				t.Fatal(names)
			}
		})
	}

	if _, err := create(ctx, client(s, "team-b", "us-east-1"), "aaa"); err != nil {
		t.Fatal(err)
	}

	status, body := raw(t, s, "ListTables", `{}`, nil)
	if status != 200 || !strings.Contains(body, `"aaa"`) {
		t.Fatalf("unsigned default: %d %s", status, body)
	}
}

func TestConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	var wg sync.WaitGroup
	var created atomic.Int32
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := create(ctx, c, "users"); err == nil {
				created.Add(1)
			} else {
				code(t, err, "ResourceInUseException")
			}
		}()
	}

	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created %d tables", created.Load())
	}

	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.PutItem(
				ctx,
				&sdk.PutItemInput{TableName: aws.String("users"), Item: key(fmt.Sprint(i))},
			); err != nil {
				t.Error(err)
			}
		}(i)
	}

	wg.Wait()
	out, err := c.Scan(ctx, &sdk.ScanInput{TableName: aws.String("users")})
	if err != nil || out.Count != 40 {
		t.Fatalf("lost writes: %v %v", out, err)
	}
}

func raw(t *testing.T, s *httptest.Server, op, body string, headers map[string]string) (status int, bodyText string) {
	t.Helper()
	r, err := http.NewRequest("POST", s.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("X-Amz-Target", "DynamoDB_20120810."+op)
	r.Header.Set("Content-Type", "application/x-amz-json-1.0")
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.Header.Get("x-amzn-RequestId") == "" {
		t.Fatal("missing request ID")
	}

	if resp.Header.Get("Content-Type") != "application/x-amz-json-1.0" {
		t.Fatal("wrong response protocol")
	}

	return resp.StatusCode, string(b)
}

func TestProtocolErrors(t *testing.T) {
	s := server(t)
	for _, tc := range []struct {
		headers map[string]string
		name    string
		op      string
		body    string
		code    string
	}{
		{name: "unknown operation", op: "BatchGetItem", body: `{}`, code: "UnknownOperationException"},
		{name: "null input", op: "ListTables", body: `null`, code: "SerializationException"},
		{name: "trailing input", op: "ListTables", body: `{} {}`, code: "SerializationException"},
		{name: "malformed JSON", op: "ListTables", body: `{`, code: "ValidationException"},
		{name: "missing fields", op: "CreateTable", body: `{}`, code: "ValidationException"},
		{
			name: "unsupported legacy condition", op: "PutItem",
			body: `{"TableName":"users","Item":{"PK":{"S":"one"}},"Expected":{"PK":{"Exists":false}}}`,
			code: "ValidationException",
		},
		{
			name: "wrong protocol", op: "ListTables", body: `{}`, code: "ValidationException",
			headers: map[string]string{"Content-Type": "application/x-amz-json-1.1"},
		},
		{
			name: "conflicting service", op: "ListTables", body: `{}`, code: "ValidationException",
			headers: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=test/20260923/us-east-1/sqs/aws4_request"},
		},
		{
			name: "bad auth", op: "ListTables", body: `{}`, code: "IncompleteSignatureException",
			headers: map[string]string{"Authorization": "bad"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := raw(t, s, tc.op, tc.body, tc.headers)
			var e struct {
				Type string `json:"__type"`
			}

			if err := json.Unmarshal([]byte(body), &e); err != nil {
				t.Fatal(err)
			}

			if status != 400 || e.Type != tc.code {
				t.Fatalf("%d %s", status, body)
			}
		})
	}
}

func TestNumericKeysAndValidation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	_, err := c.CreateTable(
		ctx,
		&sdk.CreateTableInput{
			TableName:   aws.String("numbers"),
			BillingMode: types.BillingModePayPerRequest,
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeN},
			},
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{"1.0", "1e0"} {
		if _, err = c.PutItem(
			ctx,
			&sdk.PutItemInput{
				TableName: aws.String("numbers"),
				Item:      map[string]types.AttributeValue{"PK": &types.AttributeValueMemberN{Value: n}},
			},
		); err != nil {
			t.Fatal(err)
		}
	}

	scan, err := c.Scan(ctx, &sdk.ScanInput{TableName: aws.String("numbers")})
	if err != nil || scan.Count != 1 {
		t.Fatalf("numeric identity: %v %v", scan, err)
	}

	for _, v := range []string{
		`{"N":"NaN"}`, `{"N":"1e99999999999999"}`, `{"S":"wrong"}`,
		`{"N":"2","S":"two"}`, `{"SS":[]}`, `{"NULL":false}`,
	} {
		status, body := raw(t, s, "PutItem", `{"TableName":"numbers","Item":{"PK":`+v+`}}`, nil)
		if status != 400 {
			t.Fatalf("invalid attribute accepted: %s %s", v, body)
		}
	}

	scan, err = c.Scan(ctx, &sdk.ScanInput{TableName: aws.String("numbers")})
	if err != nil || scan.Count != 1 {
		t.Fatal("invalid writes changed state")
	}
}
