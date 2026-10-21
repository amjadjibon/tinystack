package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	cfnapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/cloudformation"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	localsns "github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestCloudFormationNestedLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	objects := s3Client(s, "test", "us-east-1")
	_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("nested-templates")})
	if err != nil {
		t.Fatal(err)
	}

	child := `Parameters:
  Value: {Type: String}
Resources:
  Queue: {Type: 'AWS::SQS::Queue'}
Outputs:
  Value: {Value: !Ref Value}
`
	_, err = objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: new("nested-templates"), Key: new("child.yaml"),
		Body: strings.NewReader(child),
	})
	if err != nil {
		t.Fatal(err)
	}

	body := func(value string) string {
		return fmt.Sprintf(`Resources:
  Child:
    Type: AWS::CloudFormation::Stack
    Properties:
      TemplateURL: '%s/nested-templates/child.yaml'
      Parameters: {Value: '%s'}
      Tags: [{Key: owner, Value: parent}]
      TimeoutInMinutes: 2
Outputs:
  Result: {Value: !GetAtt Child.Outputs.Value}
  ChildId: {Value: !Ref Child}
`, s.URL, value)
	}

	created, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("nested-parent"), TemplateBody: new(body("one"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	parent := wantStatus(ctx, t, c, "nested-parent", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&parent)
	if outputs["Result"] != "one" {
		t.Fatal(outputs)
	}

	childID := outputs["ChildId"]
	desc, err := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new(childID)})
	if err != nil {
		t.Fatal(err)
	}

	nested := desc.Stacks[0]
	if nested.ParentId == nil || *nested.ParentId != *created.StackId || nested.RootId == nil ||
		*nested.RootId != *created.StackId || !strings.HasPrefix(*nested.StackName, "nested-parent-Child-") ||
		len(nested.Tags) != 1 || *nested.TimeoutInMinutes != 2 {
		t.Fatalf("nested stack: %+v", nested)
	}

	listed, err := c.ListStacks(ctx, &cfn.ListStacksInput{})
	if err != nil || len(listed.StackSummaries) != 2 {
		t.Fatalf("list: %+v %v", listed, err)
	}

	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: created.StackId, TemplateBody: new(body("two"))})
	if err != nil {
		t.Fatal(err)
	}

	parent = wantStatus(ctx, t, c, "nested-parent", cfntypes.StackStatusUpdateComplete)
	outputs = outputsOf(&parent)
	if outputs["Result"] != "two" || outputs["ChildId"] != childID {
		t.Fatal(outputs)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: created.StackId})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusDeleteComplete)
	wantStatus(ctx, t, c, childID, cfntypes.StackStatusDeleteComplete)
}

func TestCloudFormationNestedFailure(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	objects := s3Client(s, "test", "us-east-1")
	_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("nested-fail")})
	if err != nil {
		t.Fatal(err)
	}

	// This parses successfully but fails during resource creation.
	_, err = objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: new("nested-fail"), Key: new("child.yaml"),
		Body: strings.NewReader(`Resources:
  Broken:
    Type: AWS::SSM::Parameter
    Properties: {Type: invalid, Value: nope}
`),
	})
	if err != nil {
		t.Fatal(err)
	}

	c := cfnClient(s)
	created, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("nested-failure"), TemplateBody: new(fmt.Sprintf(
			`Resources:
  Child:
    Type: AWS::CloudFormation::Stack
    Properties: {TemplateURL: '%s/nested-fail/child.yaml'}
`, s.URL))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusRollbackComplete)
	listed, err := c.ListStacks(ctx, &cfn.ListStacksInput{})
	if err != nil {
		t.Fatal(err)
	}

	for _, summary := range listed.StackSummaries {
		if summary.ParentId != nil && *summary.ParentId == *created.StackId &&
			summary.StackStatus != cfntypes.StackStatusDeleteComplete {
			t.Fatalf("orphan child: %+v", summary)
		}
	}
}

type customResourceRuntime struct {
	requests []map[string]any
	mu       sync.Mutex
}

func (r *customResourceRuntime) Invoke(ctx context.Context, _ *lambda.Function, payload []byte) (lambda.Result, error) {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	r.mu.Lock()
	r.requests = append(r.requests, event)
	r.mu.Unlock()
	properties := event["ResourceProperties"].(map[string]any)
	physical := properties["Identity"].(string)
	status, reason := "SUCCESS", "done"
	if properties["Fail"] == event["RequestType"] {
		status, reason = "FAILED", "provider rejected request"
	}

	response := map[string]any{
		"Status": status, "Reason": reason, "PhysicalResourceId": physical,
		"StackId": event["StackId"], "RequestId": event["RequestId"], "LogicalResourceId": event["LogicalResourceId"],
		"Data": map[string]any{"Value": properties["Value"]},
	}

	body, err := json.Marshal(response)
	if err != nil {
		return lambda.Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, event["ResponseURL"].(string), bytes.NewReader(body))
	if err != nil {
		return lambda.Result{}, err
	}

	out, err := http.DefaultClient.Do(req)
	if err != nil {
		return lambda.Result{}, err
	}

	defer out.Body.Close()
	if out.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(out.Body)
		return lambda.Result{}, fmt.Errorf("response rejected: %s", body)
	}

	return lambda.Result{Payload: []byte(`null`)}, nil
}

func (*customResourceRuntime) Release(string) {}
func (*customResourceRuntime) Close()         {}

func (r *customResourceRuntime) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any{}, r.requests...)
}

func TestCloudFormationCustomLifecycle(t *testing.T) {
	runtime := &customResourceRuntime{}
	s := serverWith(t, runtime)
	ctx := context.Background()
	function := createFunction(t, lambdaClient(s, "test"), "custom-provider", "app.handler")
	c := cfnClient(s)
	body := func(identity, value, fail string) string {
		return fmt.Sprintf(`Resources:
  Provider:
    Type: Custom::Example
    Properties: {ServiceToken: '%s', Identity: '%s', Value: '%s', Fail: '%s'}
Outputs:
  Result: {Value: !GetAtt Provider.Value}
  Identity: {Value: !Ref Provider}
`, *function.FunctionArn, identity, value, fail)
	}

	created, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    new("custom-lifecycle"),
		TemplateBody: new(body("first", "one", "")),
	})
	if err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusCreateComplete)
	if outputsOf(&st)["Result"] != "one" {
		t.Fatal(st.Outputs)
	}

	for _, change := range []struct{ identity, value string }{{"first", "two"}, {"replacement", "three"}} {
		if change.identity == "replacement" {
			plan, planErr := c.CreateChangeSet(ctx, &cfn.CreateChangeSetInput{
				StackName:     created.StackId,
				ChangeSetName: new("custom-replacement"), ChangeSetType: cfntypes.ChangeSetTypeUpdate,
				TemplateBody: new(body(change.identity, change.value, "")),
			})
			if planErr != nil {
				t.Fatal(planErr)
			}

			_, err = c.ExecuteChangeSet(ctx, &cfn.ExecuteChangeSetInput{ChangeSetName: plan.Id})
		} else {
			_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
				StackName:    created.StackId,
				TemplateBody: new(body(change.identity, change.value, "")),
			})
		}
		if err != nil {
			t.Fatal(err)
		}

		st = wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusUpdateComplete)
		outputs := outputsOf(&st)
		if outputs["Result"] != change.value || outputs["Identity"] != change.identity {
			t.Fatal(outputs)
		}
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: created.StackId})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusDeleteComplete)
	requests := runtime.snapshot()
	var sequence []string
	for _, req := range requests {
		sequence = append(sequence, req["RequestType"].(string))
		if req["ServiceToken"] != *function.FunctionArn {
			t.Fatal(req)
		}
	}

	if !reflect.DeepEqual(sequence, []string{"Create", "Update", "Update", "Delete", "Delete"}) {
		t.Fatal(sequence)
	}

	if requests[1]["OldResourceProperties"].(map[string]any)["Value"] != "one" ||
		requests[3]["PhysicalResourceId"] != "first" || requests[4]["PhysicalResourceId"] != "replacement" {
		t.Fatal(requests)
	}

	// Failed Create still supplies an identity, so rollback deletes it.
	failed, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("custom-failed"),
		TemplateBody: new(strings.Replace(body("failed", "unused", "Create"), "Custom::Example",
			"AWS::CloudFormation::CustomResource", 1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, *failed.StackId, cfntypes.StackStatusRollbackComplete)
	events, err := c.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: failed.StackId})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, event := range events.StackEvents {
		if event.ResourceStatus == cfntypes.ResourceStatusCreateFailed && event.ResourceStatusReason != nil &&
			strings.Contains(*event.ResourceStatusReason, "provider rejected request") {
			found = true
		}
	}

	if !found {
		t.Fatal(events)
	}
}

func TestCloudFormationCustomSNS(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	topics, queues := snsClient(s, "test"), sqsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("custom-provider")})
	if err != nil {
		t.Fatal(err)
	}

	queue, err := queues.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("custom-requests")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"),
		Endpoint: new("arn:aws:sqs:us-east-1:000000000000:custom-requests"),
	})
	if err != nil {
		t.Fatal(err)
	}

	c := cfnClient(s)
	stack, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("custom-sns"), TemplateBody: new(fmt.Sprintf(
		`Resources:
  Provider:
    Type: AWS::CloudFormation::CustomResource
    Properties: {ServiceToken: '%s'}
`, *topic.TopicArn))})
	if err != nil {
		t.Fatal(err)
	}

	var request map[string]any
	waitFor(t, "SNS custom request", func() bool {
		messages, receiveErr := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: queue.QueueUrl})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}

		if len(messages.Messages) == 0 {
			return false
		}

		var envelope struct{ Message string }
		if decodeErr := json.Unmarshal([]byte(*messages.Messages[0].Body), &envelope); decodeErr != nil {
			t.Fatal(decodeErr)
		}

		if decodeErr := json.Unmarshal([]byte(envelope.Message), &request); decodeErr != nil {
			t.Fatal(decodeErr)
		}

		return true
	})
	if request["RequestType"] != "Create" || request["StackId"] != *stack.StackId {
		t.Fatal(request)
	}

	result, _ := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: stack.StackId})
	if result.Stacks[0].StackStatus != cfntypes.StackStatusCreateInProgress {
		t.Fatal(result)
	}

	response := map[string]any{
		"Status": "SUCCESS", "StackId": request["StackId"], "RequestId": request["RequestId"],
		"LogicalResourceId": request["LogicalResourceId"], "PhysicalResourceId": "sns-custom",
	}

	data, _ := json.Marshal(response)
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, request["ResponseURL"].(string), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	out, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}

	out.Body.Close()
	if out.StatusCode != http.StatusOK {
		t.Fatal(out.Status)
	}

	wantStatus(ctx, t, c, *stack.StackId, cfntypes.StackStatusCreateComplete)
}

// A small gateway lets the integration test shorten the service's provider
// timeout without adding a public configuration setting.
func TestCloudFormationCustomTimeout(t *testing.T) {
	store := &memory.Store{}
	stacks := cloudformation.New(store, nil)
	stacks.CustomResourceTimeout = 20 * time.Millisecond
	topics := localsns.New(store, nil, nil, nil)
	t.Cleanup(topics.Stop)
	services := []catalog.Service{cfnapi.Descriptor(stacks), snsapi.Descriptor(topics)}
	handler, err := gateway.New(testConfig(), nil, services...)
	if err != nil {
		t.Fatal(err)
	}

	stacks.UseCaller(catalog.NewCaller(services...))
	t.Cleanup(stacks.Stop)
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	ctx := context.Background()
	topic, err := snsClient(s, "test").CreateTopic(ctx, &sns.CreateTopicInput{Name: new("no-response")})
	if err != nil {
		t.Fatal(err)
	}

	c := cfnClient(s)
	created, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("custom-timeout"), TemplateBody: new(fmt.Sprintf(
			`Resources:
  Provider:
    Type: Custom::Timeout
    Properties: {ServiceToken: '%s'}
`, *topic.TopicArn))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusRollbackComplete)
	events, err := c.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: created.StackId})
	if err != nil {
		t.Fatal(err)
	}

	for _, event := range events.StackEvents {
		if event.ResourceStatusReason != nil && strings.Contains(*event.ResourceStatusReason, "response timed out") {
			return
		}
	}

	t.Fatal("missing timeout event", events)
}

func TestCloudFormationCustomDeleteFailure(t *testing.T) {
	runtime := &customResourceRuntime{}
	s := serverWith(t, runtime)
	function := createFunction(t, lambdaClient(s, "test"), "delete-provider", "app.handler")
	ctx := context.Background()
	c := cfnClient(s)
	created, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("custom-delete-failure"), TemplateBody: new(fmt.Sprintf(
			`Resources:
  Provider:
    Type: Custom::DeleteFailure
    Properties: {ServiceToken: '%s', Identity: delete-failure, Value: one, Fail: Delete}
`, *function.FunctionArn))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusCreateComplete)
	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: created.StackId})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusDeleteFailed)
}

func TestCloudFormationNestedAndCustomRejectUnsupported(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	for i, resource := range []string{
		`{"Type":"AWS::CloudFormation::Stack","Properties":{}}`,
		`{"Type":"AWS::CloudFormation::Stack","Properties":{` +
			`"TemplateURL":"https://s3.amazonaws.com/a/b","NotificationARNs":[]}}`,
		`{"Type":"Custom::Invalid","Properties":{}}`,
		`{"Type":"Custom::Invalid","Properties":{"ServiceToken":"arn:aws:sqs:us-east-1:000000000000:queue"}}`,
		`{"Type":"Custom::Invalid","Properties":{"ServiceToken":"arn:aws:lambda:us-east-1:000000000000:layer:bad"}}`,
	} {
		name := fmt.Sprintf("unsupported-async-%d", i)
		created, err := c.CreateStack(ctx, &cfn.CreateStackInput{
			StackName: new(name), DisableRollback: new(true),
			TemplateBody: new(`{"Resources":{"Invalid":` + resource + `}}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(ctx, t, c, *created.StackId, cfntypes.StackStatusCreateFailed)
	}
}
