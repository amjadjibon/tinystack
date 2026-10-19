package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	httpapi "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	httptypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

// policyChoice is the resource policy a delivery's target carries.
type policyChoice string

const (
	// noPolicy leaves the target without a policy for the service.
	noPolicy policyChoice = "no policy"
	// rightPolicy allows the service when the right resource delivers.
	rightPolicy policyChoice = "right source"
	// wrongPolicy allows the service only when another resource delivers.
	wrongPolicy policyChoice = "wrong source"
)

var (
	deliveryModes   = []string{config.AuthorizationEnforce, config.AuthorizationSoft, config.AuthorizationOff}
	deliveryChoices = []policyChoice{noPolicy, rightPolicy, wrongPolicy}
)

// deliveryServer builds a server whose "test" key is the default account's root and whose
// identity.authorization is mode.
func deliveryServer(t *testing.T, mode string) (*httptest.Server, *fakeRuntime) {
	t.Helper()
	runtime := &fakeRuntime{calls: make(chan invocation, 20)}
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Authorization = mode
	return serverConfig(t, cfg, runtime), runtime
}

// shapePolicy gives the target the policy choice describes, for a service that delivers from source.
func shapePolicy(t *testing.T, s *httptest.Server, target, principal, source string, choice policyChoice) {
	t.Helper()
	switch choice {
	case rightPolicy:
		allowDelivery(t, s, "test", target, principal, source)
	case wrongPolicy:
		allowDelivery(t, s, "test", target, principal, source+"-other")
	case noPolicy:
	}
}

// delivered is whether a delivery reaches its target: always, but when enforcement lacks a policy that allows it.
func delivered(mode string, choice policyChoice) bool {
	return mode != config.AuthorizationEnforce || choice == rightPolicy
}

// functionInvoked reports whether the runtime ran a function within a second.
func functionInvoked(runtime *fakeRuntime) bool {
	select {
	case <-runtime.calls:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// deliveryChecks returns the checks recorded for deliveries to target, newest first.
func deliveryChecks(t *testing.T, s *httptest.Server, target string) []gateway.Record {
	t.Helper()
	_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	var out []gateway.Record
	for i := range records {
		if records[i].Method == "DELIVER" && records[i].Path == target {
			out = append(out, records[i])
		}
	}

	return out
}

// wantDeliveryRecord checks what the request inspector shows for deliveries to target: nothing with authorization
// off, an allowed check when the policy allows the service, and otherwise a denied one that is only a warning in
// soft mode.
func wantDeliveryRecord(t *testing.T, s *httptest.Server, mode string, choice policyChoice, target string) {
	t.Helper()
	records := deliveryChecks(t, s, target)
	if mode == config.AuthorizationOff {
		if len(records) != 0 {
			t.Fatalf("authorization is off but the inspector shows %+v", records)
		}

		return
	}

	if len(records) == 0 {
		t.Fatalf("no check of the delivery to %s", target)
	}

	for i := range records {
		record := &records[i]
		auth := record.Authorization
		if auth == nil || len(auth.Checks) != 1 || auth.Checks[0].ResourcePolicy == nil || auth.Mode != mode {
			t.Fatalf("record %+v", record)
		}

		if choice == rightPolicy {
			if !auth.Allowed || record.Status != http.StatusOK {
				t.Fatalf("allowed delivery recorded as %+v", record)
			}

			continue
		}

		if auth.Allowed || !strings.Contains(auth.Message, "is not authorized to perform") {
			t.Fatalf("denied delivery recorded as %+v", auth)
		}

		if mode == config.AuthorizationSoft {
			if !strings.HasPrefix(auth.Message, "would deny: ") || record.Status != http.StatusOK {
				t.Fatalf("soft denial recorded as %+v", record)
			}
		} else if record.Status != http.StatusForbidden || record.ErrorCode != "AccessDenied" {
			t.Fatalf("enforced denial recorded as %+v", record)
		}
	}
}

func TestServicePrincipalSNS(t *testing.T) {
	for _, target := range []string{"sqs", "lambda"} {
		for _, mode := range deliveryModes {
			for _, choice := range deliveryChoices {
				t.Run(fmt.Sprintf("%s/%s/%s", target, mode, choice), func(t *testing.T) {
					s, runtime := deliveryServer(t, mode)
					ctx := context.Background()
					topics := snsClient(s, "test")
					queues := sqsClient(s, "test")
					topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("orders")})
					mustAuthorize(t, err)
					var endpoint, queueURL string
					if target == "sqs" {
						queueURL, endpoint = queueARN(t, queues, "inbox")
					} else {
						endpoint = *createFunction(t, lambdaClient(s, "test"), "inbox", "app.echo").FunctionArn
					}

					_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
						TopicArn: topic.TopicArn, Protocol: &target, Endpoint: &endpoint,
					})
					mustAuthorize(t, err)
					shapePolicy(t, s, endpoint, "sns.amazonaws.com", *topic.TopicArn, choice)

					// A failed delivery never fails Publish.
					_, err = topics.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("order 1")})
					mustAuthorize(t, err)
					var got bool
					if target == "sqs" {
						got = receiveOne(t, queues, queueURL) != ""
					} else {
						got = functionInvoked(runtime)
					}

					if want := delivered(mode, choice); got != want {
						t.Fatalf("delivered = %t, want %t", got, want)
					}

					wantDeliveryRecord(t, s, mode, choice, endpoint)
				})
			}
		}
	}
}

// bucketTarget creates the queue, topic (with a queue behind it), or function that receives a bucket's
// notifications and returns its ARN, the notification configuration naming it, a way to set aside the test event
// that saving the configuration sends, and a way to see the notification of an object.
func bucketTarget(
	t *testing.T,
	s *httptest.Server,
	runtime *fakeRuntime,
	kind string,
) (arn string, cfg *s3types.NotificationConfiguration, settle, received func() bool) {
	t.Helper()
	ctx := context.Background()
	events := []s3types.Event{"s3:ObjectCreated:*"}
	queues := sqsClient(s, "test")
	if kind == "lambda" {
		fn := createFunction(t, lambdaClient(s, "test"), "uploads", "app.echo")
		cfg = &s3types.NotificationConfiguration{LambdaFunctionConfigurations: []s3types.LambdaFunctionConfiguration{{
			LambdaFunctionArn: fn.FunctionArn, Events: events,
		}}}
		return *fn.FunctionArn, cfg, func() bool { return true }, func() bool { return functionInvoked(runtime) }
	}

	url, queue := queueARN(t, queues, "uploads")
	arn = queue
	cfg = &s3types.NotificationConfiguration{
		QueueConfigurations: []s3types.QueueConfiguration{{QueueArn: &queue, Events: events}},
	}
	if kind == "sns" {
		topics := snsClient(s, "test")
		topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("uploads")})
		mustAuthorize(t, err)
		_, err = topics.Subscribe(
			ctx,
			&sns.SubscribeInput{TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &queue},
		)
		mustAuthorize(t, err)
		allowDelivery(t, s, "test", queue, "sns.amazonaws.com", *topic.TopicArn)
		arn = *topic.TopicArn
		cfg = &s3types.NotificationConfiguration{
			TopicConfigurations: []s3types.TopicConfiguration{{TopicArn: topic.TopicArn, Events: events}},
		}
	}

	settle = func() bool { return receiveOne(t, queues, url) != "" }
	received = func() bool { return strings.Contains(receiveOne(t, queues, url), "ObjectCreated:Put") }
	return arn, cfg, settle, received
}

func TestServicePrincipalS3(t *testing.T) {
	for _, kind := range []string{"sqs", "sns", "lambda"} {
		for _, mode := range deliveryModes {
			for _, choice := range deliveryChoices {
				t.Run(fmt.Sprintf("%s/%s/%s", kind, mode, choice), func(t *testing.T) {
					s, runtime := deliveryServer(t, mode)
					ctx := context.Background()
					buckets := s3Client(s, "test", "us-east-1")
					_, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("photos")})
					mustAuthorize(t, err)
					target, cfg, settle, received := bucketTarget(t, s, runtime, kind)
					shapePolicy(t, s, target, "s3.amazonaws.com", "arn:aws:s3:::photos", choice)
					_, err = buckets.PutBucketNotificationConfiguration(
						ctx,
						&s3.PutBucketNotificationConfigurationInput{
							Bucket: new("photos"), NotificationConfiguration: cfg,
						},
					)
					if !delivered(mode, choice) {
						// S3 refuses a destination it cannot deliver to when the configuration is saved.
						code, status := s3Code(err)
						if code != "InvalidArgument" || status != http.StatusBadRequest ||
							!strings.Contains(
								err.Error(),
								"Unable to validate the following destination configurations: "+target,
							) {
							t.Fatalf("PutBucketNotificationConfiguration = %v", err)
						}

						return
					}

					mustAuthorize(t, err)
					// Saving the configuration sends a test event to a queue, or through a topic to its queue.
					settle()
					_, err = buckets.PutObject(ctx, &s3.PutObjectInput{
						Bucket: new("photos"), Key: new("a.jpg"), Body: bytes.NewReader([]byte("data")),
					})
					mustAuthorize(t, err)
					if !received() {
						t.Fatal("notification not delivered")
					}

					wantDeliveryRecord(t, s, mode, choice, target)
				})
			}
		}
	}
}

// A policy that stops allowing S3 after the configuration was saved stops its notifications too.
func TestServicePrincipalS3DeliveryTime(t *testing.T) {
	s, _ := deliveryServer(t, config.AuthorizationEnforce)
	ctx := context.Background()
	buckets := s3Client(s, "test", "us-east-1")
	queues := sqsClient(s, "test")
	_, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("photos")})
	mustAuthorize(t, err)
	url, queue := queueARN(t, queues, "uploads")
	allowDelivery(t, s, "test", queue, "s3.amazonaws.com", "arn:aws:s3:::photos")
	_, err = buckets.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
		Bucket: new("photos"), NotificationConfiguration: &s3types.NotificationConfiguration{
			QueueConfigurations: []s3types.QueueConfiguration{
				{QueueArn: &queue, Events: []s3types.Event{"s3:ObjectCreated:*"}},
			},
		},
	})
	mustAuthorize(t, err)
	if body := receiveOne(t, queues, url); !strings.Contains(body, "s3:TestEvent") {
		t.Fatalf("test event = %q", body)
	}

	allowDelivery(t, s, "test", queue, "s3.amazonaws.com", "arn:aws:s3:::someone-else")
	_, err = buckets.PutObject(ctx, &s3.PutObjectInput{
		Bucket: new("photos"), Key: new("a.jpg"), Body: bytes.NewReader([]byte("data")),
	})
	mustAuthorize(t, err)
	if body := receiveOne(t, queues, url); body != "" {
		t.Fatalf("denied notification delivered: %q", body)
	}

	denied := false
	for _, record := range deliveryChecks(t, s, queue) {
		denied = denied || (record.Authorization != nil && !record.Authorization.Allowed)
	}

	if !denied {
		t.Fatal("the denied notification left no failed check")
	}
}

func TestServicePrincipalEventBridge(t *testing.T) {
	for _, kind := range []string{"sqs", "sns", "lambda"} {
		for _, mode := range deliveryModes {
			for _, choice := range deliveryChoices {
				t.Run(fmt.Sprintf("%s/%s/%s", kind, mode, choice), func(t *testing.T) {
					s, runtime := deliveryServer(t, mode)
					ctx := context.Background()
					events := eventsClient(s, "test")
					queues := sqsClient(s, "test")
					url, queue := queueARN(t, queues, "inbox")
					target := queue
					switch kind {
					case "sns":
						topics := snsClient(s, "test")
						topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("orders")})
						mustAuthorize(t, err)
						_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
							TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &queue,
						})
						mustAuthorize(t, err)
						allowDelivery(t, s, "test", queue, "sns.amazonaws.com", *topic.TopicArn)
						target = *topic.TopicArn
					case "lambda":
						target = *createFunction(t, lambdaClient(s, "test"), "inbox", "app.echo").FunctionArn
					}

					rule := putRule(t, events, "orders", `{"source":["shop"]}`)
					_, err := events.PutTargets(ctx, &eventbridge.PutTargetsInput{
						Rule: new("orders"), Targets: []ebtypes.Target{{Id: new("target"), Arn: &target}},
					})
					mustAuthorize(t, err)
					shapePolicy(t, s, target, "events.amazonaws.com", rule, choice)
					// A target without a policy that allows the rule fails as an invocation, and PutEvents still succeeds.
					out, err := events.PutEvents(
						ctx,
						&eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
							Source: new("shop"), DetailType: new("order"), Detail: new(`{"id":1}`),
						}}},
					)
					mustAuthorize(t, err)
					if out.FailedEntryCount != 0 {
						t.Fatalf("%+v", out)
					}

					var got bool
					if kind == "lambda" {
						got = functionInvoked(runtime)
					} else {
						got = strings.Contains(receiveOne(t, queues, url), "shop")
					}

					if want := delivered(mode, choice); got != want {
						t.Fatalf("delivered = %t, want %t", got, want)
					}

					wantDeliveryRecord(t, s, mode, choice, target)
				})
			}
		}
	}
}

// A target the policy denies is a failed invocation that goes to the target's dead-letter queue, which needs its
// own policy for the rule.
func TestServicePrincipalEventBridgeDeadLetter(t *testing.T) {
	s, _ := deliveryServer(t, config.AuthorizationEnforce)
	ctx := context.Background()
	events := eventsClient(s, "test")
	queues := sqsClient(s, "test")
	url, queue := queueARN(t, queues, "inbox")
	deadURL, dead := queueARN(t, queues, "dead")
	rule := putRule(t, events, "orders", `{"source":["shop"]}`)
	_, err := events.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("orders"), Targets: []ebtypes.Target{{
			Id: new("target"), Arn: &queue, DeadLetterConfig: &ebtypes.DeadLetterConfig{Arn: &dead},
		}},
	})
	mustAuthorize(t, err)
	put := func() {
		_, putErr := events.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
			Source: new("shop"), DetailType: new("order"), Detail: new(`{"id":1}`),
		}}})
		mustAuthorize(t, putErr)
	}

	put()
	if body := receiveOne(t, queues, deadURL); body != "" {
		t.Fatalf("a dead-letter queue without a policy received %q", body)
	}

	allowDelivery(t, s, "test", dead, "events.amazonaws.com", rule)
	put()
	if body := receiveOne(t, queues, url); body != "" {
		t.Fatalf("denied target received %q", body)
	}

	letters, err := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &deadURL, MessageAttributeNames: []string{"All"}, MaxNumberOfMessages: 10,
	})
	mustAuthorize(t, err)
	if len(letters.Messages) != 1 || !strings.Contains(*letters.Messages[0].Body, `"id":1`) {
		t.Fatalf("dead letters = %+v", letters.Messages)
	}

	attributes := letters.Messages[0].MessageAttributes
	if aws.ToString(attributes["ERROR_CODE"].StringValue) != "AccessDenied" ||
		!strings.Contains(aws.ToString(attributes["ERROR_MESSAGE"].StringValue), "events.amazonaws.com") ||
		aws.ToString(attributes["RULE_ARN"].StringValue) != rule {
		t.Fatalf("attributes = %+v", attributes)
	}
}

// A target that names a role is invoked as that role: only identity policies, never the resource policy, apply.
func TestServicePrincipalEventBridgeRoleTargetSkipsPolicy(t *testing.T) {
	s, _ := deliveryServer(t, config.AuthorizationEnforce)
	ctx := context.Background()
	events := eventsClient(s, "test")
	queues := sqsClient(s, "test")
	url, queue := queueARN(t, queues, "inbox")
	putRule(t, events, "orders", `{"source":["shop"]}`)
	_, err := events.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("orders"), Targets: []ebtypes.Target{{
			Id: new("target"), Arn: &queue, RoleArn: new("arn:aws:iam::000000000000:role/events"),
		}},
	})
	mustAuthorize(t, err)
	_, err = events.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
		Source: new("shop"), DetailType: new("order"), Detail: new(`{}`),
	}}})
	mustAuthorize(t, err)
	if !strings.Contains(receiveOne(t, queues, url), "shop") {
		t.Fatal("role-based target was not delivered")
	}

	if records := deliveryChecks(t, s, queue); len(records) != 0 {
		t.Fatalf("role-based target was checked against its resource policy: %+v", records)
	}
}

// createProxyAPI publishes GET /pets of a REST or HTTP API, in stage v1, as a Lambda proxy to function. A REST API
// integration invokes the function as role when one is given, and as API Gateway otherwise.
func createProxyAPI(t *testing.T, s *httptest.Server, kind string, function, role *string) string {
	t.Helper()
	ctx := context.Background()
	if kind == "REST" {
		c := restClient(s)
		api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("pets")})
		mustAuthorize(t, err)
		pets, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
			RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: new("pets"),
		})
		mustAuthorize(t, err)
		_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
			RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: new("GET"), AuthorizationType: new("NONE"),
		})
		mustAuthorize(t, err)
		_, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
			RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: new("GET"), Type: resttypes.IntegrationTypeAwsProxy,
			IntegrationHttpMethod: new("POST"), Uri: lambdaURI(function), Credentials: role,
		})
		mustAuthorize(t, err)
		_, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{RestApiId: api.Id, StageName: new("v1")})
		mustAuthorize(t, err)
		return aws.ToString(api.Id)
	}

	c := apigwClient(s)
	api, err := c.CreateApi(ctx, &httpapi.CreateApiInput{Name: new("pets"), ProtocolType: httptypes.ProtocolTypeHttp})
	mustAuthorize(t, err)
	integration, err := c.CreateIntegration(ctx, &httpapi.CreateIntegrationInput{
		ApiId: api.ApiId, IntegrationType: httptypes.IntegrationTypeAwsProxy, IntegrationUri: function,
		PayloadFormatVersion: new(strings.TrimPrefix(kind, "HTTP")),
	})
	mustAuthorize(t, err)
	_, err = c.CreateRoute(ctx, &httpapi.CreateRouteInput{
		ApiId:    api.ApiId,
		RouteKey: new("GET /pets"),
		Target:   new("integrations/" + aws.ToString(integration.IntegrationId)),
	})
	mustAuthorize(t, err)
	_, err = c.CreateStage(
		ctx,
		&httpapi.CreateStageInput{ApiId: api.ApiId, StageName: new("v1"), AutoDeploy: new(true)},
	)
	mustAuthorize(t, err)
	return aws.ToString(api.ApiId)
}

func TestServicePrincipalAPIGateway(t *testing.T) {
	for _, kind := range []string{"REST", "HTTP1.0", "HTTP2.0"} {
		for _, mode := range deliveryModes {
			for _, choice := range deliveryChoices {
				t.Run(fmt.Sprintf("%s/%s/%s", kind, mode, choice), func(t *testing.T) {
					s, _ := deliveryServer(t, mode)
					fn := createFunction(t, lambdaClient(s, "test"), "pets", "app.respond")
					id, function := createProxyAPI(t, s, kind, fn.FunctionArn, nil), *fn.FunctionArn
					source := "arn:aws:execute-api:us-east-1:000000000000:" + id + "/v1/GET/pets"
					shapePolicy(t, s, function, "apigateway.amazonaws.com", source, choice)
					status, _, body := call(t, s, http.MethodGet, id, "/v1/pets", nil, "")
					want, wantBody := http.StatusCreated, ""
					if !delivered(mode, choice) {
						want, wantBody = http.StatusInternalServerError, `{"message":"Internal Server Error"}`
						if kind == "REST" {
							wantBody = `{"message":"Internal server error"}`
						}
					}

					if status != want || (wantBody != "" && body != wantBody) {
						t.Fatalf("status=%d body=%s, want %d %s", status, body, want, wantBody)
					}

					wantDeliveryRecord(t, s, mode, choice, function)
				})
			}
		}
	}
}

// A REST API integration with a credentials role invokes its function as that role, so the function's policy plays
// no part. HTTP API integrations cannot name a role.
func TestServicePrincipalAPIGatewayCredentialsRole(t *testing.T) {
	s, _ := deliveryServer(t, config.AuthorizationEnforce)
	fn := createFunction(t, lambdaClient(s, "test"), "pets", "app.respond")
	id := createProxyAPI(t, s, "REST", fn.FunctionArn, new("arn:aws:iam::000000000000:role/gateway"))
	status, _, body := call(t, s, http.MethodGet, id, "/v1/pets", nil, "")
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}

	if records := deliveryChecks(t, s, *fn.FunctionArn); len(records) != 0 {
		t.Fatalf("a role-based integration was checked against the function's policy: %+v", records)
	}
}

// Lambda's AddPermission for a service principal is what the delivery check reads.
func TestServicePrincipalLambdaAddPermission(t *testing.T) {
	s, runtime := deliveryServer(t, config.AuthorizationEnforce)
	ctx := context.Background()
	topics := snsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("orders")})
	mustAuthorize(t, err)
	fn := createFunction(t, lambdaClient(s, "test"), "inbox", "app.echo")
	_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("lambda"), Endpoint: fn.FunctionArn,
	})
	mustAuthorize(t, err)
	publish := func() {
		_, publishErr := topics.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("order")})
		mustAuthorize(t, publishErr)
	}

	publish()
	if functionInvoked(runtime) {
		t.Fatal("function invoked without a permission")
	}

	allowDelivery(t, s, "test", *fn.FunctionArn, "sns.amazonaws.com", *topic.TopicArn)
	publish()
	if !functionInvoked(runtime) {
		t.Fatal("function not invoked with a permission for the topic")
	}
}

// An explicit Deny in the target's policy wins over an Allow.
func TestServicePrincipalExplicitDeny(t *testing.T) {
	s, _ := deliveryServer(t, config.AuthorizationEnforce)
	ctx := context.Background()
	topics := snsClient(s, "test")
	queues := sqsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("orders")})
	mustAuthorize(t, err)
	url, queue := queueARN(t, queues, "inbox")
	_, err = topics.Subscribe(
		ctx,
		&sns.SubscribeInput{TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &queue},
	)
	mustAuthorize(t, err)
	for name, deny := range map[string]string{
		"deny":      `{"Effect":"Deny","Principal":{"Service":"sns.amazonaws.com"},"Action":"sqs:*","Resource":"*"}`,
		"deny by *": `{"Effect":"Deny","Principal":"*","Action":"sqs:SendMessage","Resource":"*"}`,
	} {
		policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
			`{"Effect":"Allow","Principal":{"Service":"sns.amazonaws.com"},"Action":"sqs:SendMessage","Resource":"*"},`+
			`%s]}`, deny)
		_, err = queues.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl: &url, Attributes: map[string]string{string(sqstypes.QueueAttributeNamePolicy): policy},
		})
		mustAuthorize(t, err)
		_, err = topics.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("order")})
		mustAuthorize(t, err)
		if body := receiveOne(t, queues, url); body != "" {
			t.Fatalf("%s: delivered %q", name, body)
		}
	}
}
