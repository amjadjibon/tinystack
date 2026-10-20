package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	schedules "github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedtypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

const deliveryAccount = "000000000000"

// roleChoice is what a delivery's role is set up to allow.
type roleChoice string

const (
	// roleAllowed trusts the service for the delivering resource and allows the call.
	roleAllowed roleChoice = "allowed"
	// roleNoPermission trusts the service but its identity policy allows other actions only.
	roleNoPermission roleChoice = "identity policy denies"
	// roleNotTrusted allows the call but trusts another service.
	roleNotTrusted roleChoice = "trust policy names another service"
	// roleWrongSource trusts the service only for another source resource.
	roleWrongSource roleChoice = "trust policy names another source"
)

var roleChoices = []roleChoice{roleAllowed, roleNoPermission, roleNotTrusted, roleWrongSource}

// documents are the role's trust and identity policy documents for the choice, for a delivery from source
// by principal that sends to queue.
func (c roleChoice) documents(principal, source, queue string) (trust, permissions string) {
	return c.documentsFor(principal, source, queue, "sqs:SendMessage")
}

// documentsFor are the role's documents for the choice, for a delivery from source by principal that makes the calls
// named by actions on resource.
func (c roleChoice) documentsFor(principal, source, resource string, actions ...string) (trust, permissions string) {
	trusted, from := principal, source
	switch c {
	case roleNotTrusted:
		trusted = "other.amazonaws.com"
	case roleWrongSource:
		from = source + "-other"
	case roleAllowed, roleNoPermission:
	}

	trust = fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":%q},`+
		`"Action":"sts:AssumeRole","Condition":{"ArnLike":{"aws:SourceArn":%q},`+
		`"StringEquals":{"aws:SourceAccount":%q}}}]}`, trusted, from, deliveryAccount)
	if c == roleNoPermission {
		actions = []string{"s3:GetObject"}
	}

	allowed, _ := json.Marshal(actions)
	permissions = fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":%s,"Resource":%q}]}`,
		allowed, resource)

	return trust, permissions
}

// createDeliveryRole creates a role with the documents and returns its ARN.
func createDeliveryRole(t *testing.T, s *httptest.Server, name, trust, permissions string) string {
	t.Helper()
	ctx := context.Background()
	c := iamClient(s, "test", "us-east-1")
	role, err := c.CreateRole(ctx, &iam.CreateRoleInput{RoleName: &name, AssumeRolePolicyDocument: &trust})
	mustAuthorize(t, err)
	_, err = c.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: &name, PolicyName: new("send"), PolicyDocument: &permissions,
	})
	mustAuthorize(t, err)

	return aws.ToString(role.Role.Arn)
}

// createSendMachine creates the state machine "orders", which sends a message to the queue as role.
func createSendMachine(t *testing.T, c *sfn.Client, role, queueURL string) *string {
	t.Helper()
	definition := `{"StartAt":"Send","States":{"Send":{"Type":"Task","Resource":"arn:aws:states:::sqs:sendMessage",` +
		`"Parameters":{"QueueUrl":"` + queueURL + `","MessageBody":"stepped"},"End":true}}}`
	machine, err := c.CreateStateMachine(context.Background(), &sfn.CreateStateMachineInput{
		Name: new("orders"), RoleArn: &role, Type: sfntypes.StateMachineTypeStandard, Definition: &definition,
	})
	mustAuthorize(t, err)

	return machine.StateMachineArn
}

// roleDelivery sets up one kind of service delivering a message to a queue under a role.
type roleDelivery struct {
	// deliver arranges for the service to send a message to the queue as role.
	deliver func(t *testing.T, s *httptest.Server, role, queueURL, queue string)
	name    string
	// principal is the service principal and source the aws:SourceArn the role's trust policy sees.
	principal, source string
}

var roleDeliveries = []roleDelivery{
	{
		name:      "scheduler",
		principal: "scheduler.amazonaws.com",
		source:    "arn:aws:scheduler:us-east-1:" + deliveryAccount + ":schedule-group/default",
		deliver: func(t *testing.T, s *httptest.Server, role, _, queue string) {
			t.Helper()
			_, err := schedulerClient(s).CreateSchedule(context.Background(), &schedules.CreateScheduleInput{
				Name: new("once"), ScheduleExpression: oneTime(time.Now().Add(-time.Minute)),
				FlexibleTimeWindow: &schedtypes.FlexibleTimeWindow{Mode: schedtypes.FlexibleTimeWindowModeOff},
				Target:             &schedtypes.Target{Arn: &queue, RoleArn: &role, Input: new("scheduled")},
			})
			mustAuthorize(t, err)
		},
	},
	{
		name:      "pipes",
		principal: "pipes.amazonaws.com",
		source:    "arn:aws:pipes:us-east-1:" + deliveryAccount + ":pipe/orders",
		deliver: func(t *testing.T, s *httptest.Server, role, _, queue string) {
			t.Helper()
			queues := sqsClient(s, "test")
			sourceURL, source := queueARN(t, queues, "pipe-source")
			_, err := queues.SendMessage(context.Background(), &sqs.SendMessageInput{
				QueueUrl: &sourceURL, MessageBody: new("piped"),
			})
			mustAuthorize(t, err)
			// The pipe's role also reads the source.
			name := role[strings.LastIndex(role, "/")+1:]
			reads := `{"Statement":[{"Effect":"Allow","Action":["sqs:ReceiveMessage","sqs:DeleteMessage",` +
				`"sqs:GetQueueAttributes"],"Resource":"` + source + `"}]}`
			_, err = iamClient(s, "test", "us-east-1").PutRolePolicy(context.Background(), &iam.PutRolePolicyInput{
				RoleName: &name, PolicyName: new("read"), PolicyDocument: &reads,
			})
			// With authorization off the role need not exist.
			if _, missing := errors.AsType[*iamtypes.NoSuchEntityException](err); !missing {
				mustAuthorize(t, err)
			}
			in := sqsPipe("orders", source, queue)
			in.RoleArn = &role
			_, err = pipesClient(s).CreatePipe(context.Background(), in)
			mustAuthorize(t, err)
		},
	},
	{
		name:      "stepfunctions",
		principal: "states.amazonaws.com",
		source:    "arn:aws:states:us-east-1:" + deliveryAccount + ":stateMachine:orders",
		deliver: func(t *testing.T, s *httptest.Server, role, queueURL, _ string) {
			t.Helper()
			c := sfnClient(s)
			machine := createSendMachine(t, c, role, queueURL)
			start(t, c, machine, `{}`)
		},
	},
	{
		name:      "eventbridge",
		principal: "events.amazonaws.com",
		source:    "arn:aws:events:us-east-1:" + deliveryAccount + ":rule/orders",
		deliver: func(t *testing.T, s *httptest.Server, role, _, queue string) {
			t.Helper()
			ctx := context.Background()
			events := eventsClient(s, "test")
			putRule(t, events, "orders", `{"source":["shop"]}`)
			_, err := events.PutTargets(ctx, &eventbridge.PutTargetsInput{
				Rule: new("orders"), Targets: []ebtypes.Target{{Id: new("target"), Arn: &queue, RoleArn: &role}},
			})
			mustAuthorize(t, err)
			_, err = events.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
				Source: new("shop"), DetailType: new("order"), Detail: new(`{}`),
			}}})
			mustAuthorize(t, err)
		},
	},
	{
		name:      "iot",
		principal: "iot.amazonaws.com",
		source:    "arn:aws:iot:us-east-1:" + deliveryAccount + ":rule/orders",
		deliver: func(t *testing.T, s *httptest.Server, role, queueURL, _ string) {
			t.Helper()
			createRule(t, iotClient(s, "test"), "orders", "SELECT * FROM 'shop/orders'", []iottypes.Action{{
				Sqs: &iottypes.SqsAction{QueueUrl: &queueURL, RoleArn: &role},
			}}, nil)
			_, err := dataClient(s, "test").Publish(context.Background(), &iotdataplane.PublishInput{
				Topic: new("shop/orders"), Payload: []byte(`{"id":1}`),
			})
			mustAuthorize(t, err)
		},
	},
	{
		// The result of an asynchronous invocation goes to the destination as the function's execution role.
		name:      "lambda destination",
		principal: "lambda.amazonaws.com",
		source:    "arn:aws:lambda:us-east-1:" + deliveryAccount + ":function:worker",
		deliver: func(t *testing.T, s *httptest.Server, role, _, queue string) {
			t.Helper()
			ctx := context.Background()
			c := lambdaClient(s, "test")
			createWorker(t, c, role)
			_, err := c.PutFunctionEventInvokeConfig(ctx, &sdklambda.PutFunctionEventInvokeConfigInput{
				FunctionName: new("worker"),
				DestinationConfig: &lambdatypes.DestinationConfig{
					OnSuccess: &lambdatypes.OnSuccess{Destination: &queue},
				},
			})
			mustAuthorize(t, err)
			_, err = c.Invoke(ctx, &sdklambda.InvokeInput{
				FunctionName: new("worker"), InvocationType: lambdatypes.InvocationTypeEvent, Payload: []byte(`{}`),
			})
			mustAuthorize(t, err)
		},
	},
}

// roleChecks returns the checks recorded for deliveries under the role, newest first.
func roleChecks(t *testing.T, s *httptest.Server, role string) []gateway.Record {
	t.Helper()
	_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	var out []gateway.Record
	for i := range records {
		if records[i].Method == "DELIVER" && strings.Contains(records[i].RequestBody, `"role":"`+role+`"`) {
			out = append(out, records[i])
		}
	}

	return out
}

// awaitMessage polls the queue for a message for up to wait.
func awaitMessage(t *testing.T, c *sqs.Client, queueURL string, wait time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		if receiveOne(t, c, queueURL) != "" {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// wantRoleCheck checks the inspector's account of a delivery under role: the check, who it was made as, and why a
// denied one failed.
func wantRoleCheck(t *testing.T, record *gateway.Record, mode string, choice roleChoice, role, queue string) {
	t.Helper()
	auth := record.Authorization
	if auth == nil || auth.Mode != mode {
		t.Fatalf("authorization = %+v", auth)
	}

	name := role[strings.LastIndex(role, "/")+1:]
	allowed := choice == roleAllowed
	if auth.Allowed != allowed {
		t.Fatalf("allowed = %t, want %t: %s", auth.Allowed, allowed, auth.Message)
	}

	wantStatus := http.StatusOK
	if !allowed && mode == config.AuthorizationEnforce {
		wantStatus = http.StatusForbidden
	}

	if record.Status != wantStatus || record.Method != "DELIVER" {
		t.Fatalf("record = %+v", record)
	}

	prefix := ""
	if !allowed && mode == config.AuthorizationSoft {
		prefix = "would deny: "
	}

	switch choice {
	case roleAllowed:
		check := auth.Checks[0]
		if !strings.HasPrefix(auth.Principal, "arn:aws:sts::"+deliveryAccount+":assumed-role/"+name+"/") ||
			check.Action != "sqs:SendMessage" || check.Resource != queue || record.Service != "sqs" {
			t.Fatalf("principal=%q check=%+v service=%q", auth.Principal, check, record.Service)
		}
	case roleNoPermission:
		if !strings.HasPrefix(auth.Message, prefix+"User: arn:aws:sts::"+deliveryAccount+":assumed-role/"+name+"/") ||
			!strings.Contains(auth.Message, "because no identity-based policy allows the sqs:SendMessage action") ||
			len(auth.Checks) != 1 || auth.Checks[0].Resource != queue {
			t.Fatalf("message = %q checks = %+v", auth.Message, auth.Checks)
		}
	case roleNotTrusted, roleWrongSource:
		if !strings.HasPrefix(auth.Message, prefix) ||
			!strings.Contains(auth.Message, "sts:AssumeRole on resource: "+role) ||
			record.Service != "sts" || record.Path != role {
			t.Fatalf("message = %q service = %q path = %q", auth.Message, record.Service, record.Path)
		}
	}
}

// A service that delivers under a role assumes it, which its trust policy must allow, and then acts as the role's
// session: the delivery is decided by the role's identity policies.
func TestRoleDeliveries(t *testing.T) {
	for _, delivery := range roleDeliveries {
		for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
			for _, choice := range roleChoices {
				t.Run(fmt.Sprintf("%s/%s/%s", delivery.name, mode, choice), func(t *testing.T) {
					t.Parallel()
					s, _ := deliveryServer(t, mode)
					queues := sqsClient(s, "test")
					queueURL, queue := queueARN(t, queues, "inbox")
					trust, permissions := choice.documents(delivery.principal, delivery.source, queue)
					role := createDeliveryRole(t, s, "delivery", trust, permissions)
					delivery.deliver(t, s, role, queueURL, queue)

					var records []gateway.Record
					waitFor(t, "a check of the delivery", func() bool {
						// A pipe's reads of its source are checked beside the delivery.
						records = slices.DeleteFunc(roleChecks(t, s, role), func(r gateway.Record) bool {
							return slices.Contains(
								[]string{"ReceiveMessage", "DeleteMessage", "GetQueueAttributes"},
								r.Operation,
							)
						})
						return len(records) > 0
					})
					wantRoleCheck(t, &records[0], mode, choice, role, queue)

					want := choice == roleAllowed || mode == config.AuthorizationSoft
					if got := awaitMessage(t, queues, queueURL, 1500*time.Millisecond); got != want {
						t.Fatalf("delivered = %t, want %t", got, want)
					}
				})
			}
		}
	}
}

// With authorization off a role is not consulted: it need not exist.
func TestRoleDeliveriesOff(t *testing.T) {
	for _, delivery := range roleDeliveries {
		t.Run(delivery.name, func(t *testing.T) {
			t.Parallel()
			s, _ := deliveryServer(t, config.AuthorizationOff)
			queues := sqsClient(s, "test")
			queueURL, queue := queueARN(t, queues, "inbox")
			role := "arn:aws:iam::" + deliveryAccount + ":role/missing"
			delivery.deliver(t, s, role, queueURL, queue)
			if !awaitMessage(t, queues, queueURL, 3*time.Second) {
				t.Fatal("the delivery was not made")
			}

			if records := roleChecks(t, s, role); len(records) != 0 {
				t.Fatalf("authorization is off but the inspector shows %+v", records)
			}
		})
	}
}

// A role that does not exist cannot be assumed.
func TestRoleDeliveryMissingRole(t *testing.T) {
	s, _ := deliveryServer(t, config.AuthorizationEnforce)
	queues := sqsClient(s, "test")
	queueURL, queue := queueARN(t, queues, "inbox")
	role := "arn:aws:iam::" + deliveryAccount + ":role/missing"
	roleDeliveries[0].deliver(t, s, role, queueURL, queue)
	var records []gateway.Record
	waitFor(t, "a check of the delivery", func() bool {
		records = roleChecks(t, s, role)
		return len(records) > 0
	})

	auth := records[0].Authorization
	if auth.Allowed || !strings.Contains(auth.Message, "sts:AssumeRole on resource: "+role) {
		t.Fatalf("authorization = %+v", auth)
	}

	if awaitMessage(t, queues, queueURL, 300*time.Millisecond) {
		t.Fatal("a missing role delivered")
	}
}

// A schedule whose role is denied sends its input to the dead-letter queue, with the denial for the code. The role
// that was denied sends there too, so it is allowed to.
func TestRoleDeliverySchedulerDeadLetter(t *testing.T) {
	s, _ := deliveryServer(t, config.AuthorizationEnforce)
	queues := sqsClient(s, "test")
	_, queue := queueARN(t, queues, "inbox")
	dlqURL, dlq := queueARN(t, queues, "dead")
	trust, _ := roleAllowed.documents("scheduler.amazonaws.com", roleDeliveries[0].source, queue)
	permissions := fmt.Sprintf(`{"Statement":[{"Effect":"Allow","Action":"sqs:SendMessage","Resource":%q}]}`, dlq)
	role := createDeliveryRole(t, s, "delivery", trust, permissions)
	_, err := schedulerClient(s).CreateSchedule(context.Background(), &schedules.CreateScheduleInput{
		Name: new("once"), ScheduleExpression: oneTime(time.Now().Add(-time.Minute)),
		FlexibleTimeWindow: &schedtypes.FlexibleTimeWindow{Mode: schedtypes.FlexibleTimeWindowModeOff},
		Target: &schedtypes.Target{
			Arn: &queue, RoleArn: &role, Input: new("lost"), DeadLetterConfig: &schedtypes.DeadLetterConfig{Arn: &dlq},
		},
	})
	mustAuthorize(t, err)
	m := receiveScheduled(t, queues, dlqURL)
	code := aws.ToString(m.MessageAttributes["ERROR_CODE"].StringValue)
	message := aws.ToString(m.MessageAttributes["ERROR_MESSAGE"].StringValue)
	if aws.ToString(m.Body) != "lost" || code != "AccessDeniedException" ||
		!strings.Contains(message, "because no identity-based policy allows the sqs:SendMessage action") {
		t.Fatalf("dead letter %q: %s %s", aws.ToString(m.Body), code, message)
	}
}

// A state machine whose role is denied fails the task with the denial, as AWS reports a Task's AccessDenied.
func TestRoleDeliveryStepFunctionsFailure(t *testing.T) {
	s, _ := deliveryServer(t, config.AuthorizationEnforce)
	queues := sqsClient(s, "test")
	queueURL, queue := queueARN(t, queues, "inbox")
	trust, permissions := roleNoPermission.documents("states.amazonaws.com", roleDeliveries[2].source, queue)
	role := createDeliveryRole(t, s, "delivery", trust, permissions)
	c := sfnClient(s)
	machine := createSendMachine(t, c, role, queueURL)
	done := finish(t, c, start(t, c, machine, `{}`))
	if done.Status != sfntypes.ExecutionStatusFailed || aws.ToString(done.Error) != "SQS.AccessDeniedException" ||
		!strings.Contains(aws.ToString(done.Cause), "no identity-based policy allows the sqs:SendMessage action") {
		t.Fatalf("execution = %s %s %s", done.Status, aws.ToString(done.Error), aws.ToString(done.Cause))
	}
}

// A target whose role is denied goes to the rule's dead-letter queue with the code EventBridge uses for missing
// permissions, or for a role it cannot assume.
func TestRoleDeliveryEventBridgeDeadLetter(t *testing.T) {
	for choice, want := range map[roleChoice]string{
		roleNoPermission: "NO_PERMISSIONS", roleNotTrusted: "FAILED_TO_ASSUME_ROLE",
	} {
		t.Run(string(choice), func(t *testing.T) {
			s, _ := deliveryServer(t, config.AuthorizationEnforce)
			ctx := context.Background()
			queues := sqsClient(s, "test")
			_, queue := queueARN(t, queues, "inbox")
			deadURL, dead := queueARN(t, queues, "dead")
			events := eventsClient(s, "test")
			rule := putRule(t, events, "orders", `{"source":["shop"]}`)
			allowDelivery(t, s, "test", dead, "events.amazonaws.com", rule)
			trust, permissions := choice.documents("events.amazonaws.com", roleDeliveries[3].source, queue)
			role := createDeliveryRole(t, s, "delivery", trust, permissions)
			letterbox := &ebtypes.DeadLetterConfig{Arn: &dead}
			_, err := events.PutTargets(ctx, &eventbridge.PutTargetsInput{
				Rule: new("orders"), Targets: []ebtypes.Target{{
					Id: new("target"), Arn: &queue, RoleArn: &role, DeadLetterConfig: letterbox,
				}},
			})
			mustAuthorize(t, err)
			_, err = events.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
				Source: new("shop"), DetailType: new("order"), Detail: new(`{"id":1}`),
			}}})
			mustAuthorize(t, err)
			var letters *sqs.ReceiveMessageOutput
			waitFor(t, "a dead letter", func() bool {
				letters, err = queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
					QueueUrl: &deadURL, MessageAttributeNames: []string{"All"}, MaxNumberOfMessages: 10,
				})
				mustAuthorize(t, err)
				return len(letters.Messages) > 0
			})

			attributes := letters.Messages[0].MessageAttributes
			if aws.ToString(attributes["ERROR_CODE"].StringValue) != want ||
				aws.ToString(attributes["RULE_ARN"].StringValue) != rule {
				t.Fatalf("attributes = %+v", attributes)
			}
		})
	}
}
