package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
)

// verifyingServer checks signatures. Test clients sign with the secret "test",
// so "wrong" signs with the wrong secret and "stranger" is not configured.
func verifyingServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test", "team-b": "test", "wrong": "other"}
	return serverConfig(t, cfg, &fakeRuntime{})
}

func TestSigV4VerifiedClients(t *testing.T) {
	s := verifyingServer(t)
	ctx := context.Background()
	if _, err := create(ctx, client(s, "team-b", "us-east-1"), "users"); err != nil {
		t.Fatal(err)
	}

	queue, err := sqsClient(s, "test").CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("jobs")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = sqsClient(s, "test").SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: queue.QueueUrl, MessageBody: aws.String("a b+c")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = snsClient(s, "test").CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("news")}); err != nil {
		t.Fatal(err)
	}

	if _, err = eventsClient(s, "test").ListRules(ctx, &eventbridge.ListRulesInput{}); err != nil {
		t.Fatal(err)
	}

	// S3 signs its URI path without a second encoding.
	buckets := s3Client(s, "test", "us-east-1")
	if _, err = buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("files")}); err != nil {
		t.Fatal(err)
	}

	key := "dir/a b+c:d~e(ü)=%.txt"
	_, err = buckets.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: aws.String("files"), Key: aws.String(key), Body: strings.NewReader("hello")},
	)
	if err != nil {
		t.Fatal(err)
	}

	object, err := buckets.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("files"), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}

	if body, _ := io.ReadAll(object.Body); string(body) != "hello" {
		t.Fatalf("%q", body)
	}

	// Function code signs calls back to TinyStack with its creator's secret.
	functions := lambdaClient(s, "test")
	created := createFunction(t, functions, "echo", "app.echo")
	out, err := functions.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: created.FunctionArn})
	if err != nil {
		t.Fatal(err)
	}

	var echoed struct{ Env map[string]string }
	if err = json.Unmarshal(out.Payload, &echoed); err != nil || echoed.Env["AWS_ACCESS_KEY_ID"] != "test" ||
		echoed.Env["AWS_SECRET_ACCESS_KEY"] != "test" {
		t.Fatalf("%s %v", out.Payload, err)
	}
}

func TestSigV4RejectedClients(t *testing.T) {
	s := verifyingServer(t)
	ctx := context.Background()
	for _, tc := range []struct {
		call        func(key string) error
		name        string
		wrong, gone string
	}{
		{
			name: "dynamodb", wrong: "InvalidSignatureException", gone: "UnrecognizedClientException",
			call: func(key string) error {
				_, err := client(s, key, "us-east-1").ListTables(ctx, &sdk.ListTablesInput{})
				return err
			},
		},
		{
			name: "sqs", wrong: "InvalidSignatureException", gone: "UnrecognizedClientException",
			call: func(key string) error {
				_, err := sqsClient(s, key).ListQueues(ctx, &sqs.ListQueuesInput{})
				return err
			},
		},
		{
			name: "sns", wrong: "SignatureDoesNotMatch", gone: "InvalidClientTokenId",
			call: func(key string) error {
				_, err := snsClient(s, key).ListTopics(ctx, &sns.ListTopicsInput{})
				return err
			},
		},
		{
			name: "s3", wrong: "SignatureDoesNotMatch", gone: "InvalidAccessKeyId",
			call: func(key string) error {
				_, err := s3Client(s, key, "us-east-1").ListBuckets(ctx, &s3.ListBucketsInput{})
				return err
			},
		},
		{
			name: "lambda", wrong: "InvalidSignatureException", gone: "UnrecognizedClientException",
			call: func(key string) error {
				_, err := lambdaClient(s, key).ListFunctions(ctx, &sdklambda.ListFunctionsInput{})
				return err
			},
		},
		{
			name: "eventbridge", wrong: "InvalidSignatureException", gone: "UnrecognizedClientException",
			call: func(key string) error {
				_, err := eventsClient(s, key).ListRules(ctx, &eventbridge.ListRulesInput{})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code(t, tc.call("wrong"), tc.wrong)
			code(t, tc.call("stranger"), tc.gone)
		})
	}
}

// signedRequest builds a DynamoDB ListTables request signed by the AWS SDK signer as key "test".
func signedRequest(t *testing.T, url string, at time.Time) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), "POST", url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("X-Amz-Target", "DynamoDB_20120810.ListTables")
	r.Header.Set("Content-Type", "application/x-amz-json-1.0")

	sum := sha256.Sum256([]byte("{}"))
	payloadHash := hex.EncodeToString(sum[:])
	signer := v4.NewSigner()
	credentials := aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
	if err = signer.SignHTTP(
		context.Background(),
		credentials,
		r,
		payloadHash,
		"dynamodb",
		"us-east-1",
		at,
	); err != nil {
		t.Fatal(err)
	}

	return r
}

func send(t *testing.T, s *httptest.Server, r *http.Request) (status int, body string) {
	t.Helper()
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(b)
}

func TestSigV4RejectedRequests(t *testing.T) {
	s := verifyingServer(t)
	now := time.Now()
	for _, tc := range []struct {
		request func() *http.Request
		name    string
		want    string
		status  int
	}{
		{
			name: "unsigned", status: 403, want: "MissingAuthenticationTokenException",
			request: func() *http.Request {
				r := signedRequest(t, s.URL, now)
				r.Header.Del("Authorization")
				return r
			},
		},
		{
			// S3 routes no X-Amz-* query parameters, so presigned URLs stay unsupported.
			name: "malformed presigned", status: 400, want: "AuthorizationQueryParametersError",
			request: func() *http.Request {
				r, _ := http.NewRequestWithContext(
					context.Background(),
					"GET",
					s.URL+"/files/a?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=bad&X-Amz-Signature=abc",
					http.NoBody,
				)
				return r
			},
		},
		{
			name: "expired", status: 403, want: "Signature expired",
			request: func() *http.Request { return signedRequest(t, s.URL, now.Add(-time.Hour)) },
		},
		{
			name: "changed body", status: 403, want: "InvalidSignatureException",
			request: func() *http.Request {
				r := signedRequest(t, s.URL, now)
				r.Body = io.NopCloser(strings.NewReader(`{"Limit":1}`))
				r.ContentLength = 11
				r.Header.Del("Content-Length")
				return r
			},
		},
		{
			name: "unsigned host", status: 400, want: "IncompleteSignatureException",
			request: func() *http.Request {
				r := signedRequest(t, s.URL, now)
				auth := r.Header.Get("Authorization")
				r.Header.Set("Authorization", strings.Replace(auth, "SignedHeaders=", "SignedHeaders=x-amz-date;", 1))
				r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), ";host;", ";", 1))
				return r
			},
		},
		{
			name: "s3 content hash", status: 400, want: "XAmzContentSHA256Mismatch",
			request: func() *http.Request {
				r, _ := http.NewRequestWithContext(context.Background(), "PUT", s.URL+"/files", bytes.NewReader(nil))
				r.Header.Set("X-Amz-Content-Sha256", strings.Repeat("0", 64))
				return signedWithHeader(t, r, now)
			},
		},
		{
			name: "s3 signed chunks", status: 501, want: "NotImplemented",
			request: func() *http.Request {
				r, _ := http.NewRequestWithContext(context.Background(), "PUT", s.URL+"/files", bytes.NewReader(nil))
				r.Header.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
				return signedWithHeader(t, r, now)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := send(t, s, tc.request())
			if status != tc.status || !strings.Contains(body, tc.want) {
				t.Fatalf("%d %s", status, body)
			}
		})
	}
}

// signedWithHeader signs an S3 request with the payload hash in its
// X-Amz-Content-Sha256 header.
func signedWithHeader(t *testing.T, r *http.Request, at time.Time) *http.Request {
	t.Helper()
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	credentials := aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if err := signer.SignHTTP(r.Context(), credentials, r, payloadHash, "s3", "us-east-1", at); err != nil {
		t.Fatal(err)
	}

	return r
}
