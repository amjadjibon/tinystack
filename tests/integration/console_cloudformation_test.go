package integration_test

import (
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
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

const consoleStackTemplate = `{"Parameters": {"Secret": {"Type": "String", "NoEcho": true}},
  "Resources": {"Topic": {"Type": "AWS::SNS::Topic"}},
  "Outputs": {"TopicName": {"Value": {"Fn::GetAtt": ["Topic", "TopicName"]}}}}`

func TestConsoleCloudFormationPage(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	const account, region = "000000000000", "us-east-1"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, s,
			consoleLambdaRequest(t, s, "cloudformation", operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	var capabilities map[string]any
	if internal(t, s, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["cloudformation"] != true {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	created, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("console-stack"), TemplateBody: aws.String(consoleStackTemplate),
		Parameters: []cfntypes.Parameter{{ParameterKey: aws.String("Secret"), ParameterValue: aws.String("hunter2")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "console-stack", cfntypes.StackStatusCreateComplete)
	id := aws.ToString(created.StackId)
	summaries := call("ListStacks", map[string]any{})["StackSummaries"].([]any)
	if len(summaries) != 1 || summaries[0].(map[string]any)["StackId"] != id ||
		summaries[0].(map[string]any)["StackStatus"] != "CREATE_COMPLETE" {
		t.Fatalf("summaries = %+v", summaries)
	}

	described := call("DescribeStacks", map[string]any{"StackName": id})
	data, err := json.Marshal(described)
	if err != nil || strings.Contains(string(data), "hunter2") {
		t.Fatalf("NoEcho value reached the console: %s, %v", data, err)
	}

	stack := described["Stacks"].([]any)[0].(map[string]any)
	if stack["Parameters"].([]any)[0].(map[string]any)["ParameterValue"] != "****" ||
		len(stack["Outputs"].([]any)) != 1 {
		t.Fatalf("stack = %+v", stack)
	}

	if len(call("DescribeStackEvents", map[string]any{"StackName": id})["StackEvents"].([]any)) == 0 {
		t.Fatal("no events")
	}

	resources := call("ListStackResources", map[string]any{"StackName": id})["StackResourceSummaries"].([]any)
	if len(resources) != 1 || resources[0].(map[string]any)["ResourceType"] != "AWS::SNS::Topic" {
		t.Fatalf("resources = %+v", resources)
	}

	for _, stage := range []string{"Original", "Processed"} {
		body := call("GetTemplate", map[string]any{"StackName": id, "TemplateStage": stage})["TemplateBody"]
		if text, ok := body.(string); !ok || !strings.Contains(text, "AWS::SNS::Topic") {
			t.Fatalf("%s template = %+v", stage, body)
		}
	}

	changed := strings.Replace(consoleStackTemplate, `"Resources": {`,
		`"Resources": {"Queue": {"Type": "AWS::SQS::Queue"}, `, 1)
	for _, name := range []string{"add-queue", "discard"} {
		if _, err = c.CreateChangeSet(ctx, &cfn.CreateChangeSetInput{
			StackName: aws.String("console-stack"), ChangeSetName: aws.String(name), TemplateBody: aws.String(changed),
			Parameters: []cfntypes.Parameter{{ParameterKey: aws.String("Secret"), UsePreviousValue: aws.Bool(true)}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	sets := call("ListChangeSets", map[string]any{"StackName": id})["Summaries"].([]any)
	if len(sets) != 2 {
		t.Fatalf("change sets = %+v", sets)
	}

	set := call("DescribeChangeSet", map[string]any{"StackName": id, "ChangeSetName": "add-queue"})
	if set["Status"] != "CREATE_COMPLETE" || set["ExecutionStatus"] != "AVAILABLE" || len(set["Changes"].([]any)) != 1 {
		t.Fatalf("change set = %+v", set)
	}

	call("DeleteChangeSet", map[string]any{"StackName": id, "ChangeSetName": "discard"})
	if sets = call("ListChangeSets", map[string]any{"StackName": id})["Summaries"].([]any); len(sets) != 1 {
		t.Fatalf("change sets after delete = %+v", sets)
	}

	call("ExecuteChangeSet", map[string]any{"StackName": id, "ChangeSetName": "add-queue"})
	wantStatus(ctx, t, c, "console-stack", cfntypes.StackStatusUpdateComplete)

	// Only the page's operations run; creating and updating stacks stays with the SDK.
	for _, operation := range []string{
		"CreateStack", "UpdateStack", "CreateChangeSet", "UpdateTerminationProtection",
		"DetectStackDrift", "ContinueUpdateRollback", "CancelUpdateStack",
	} {
		out := consoleResponse(t, s,
			consoleLambdaRequest(t, s, "cloudformation", operation, account, region, map[string]any{}),
			http.StatusBadRequest)
		if out["code"] != "UnknownOperationException" {
			t.Fatalf("%s: %+v", operation, out)
		}
	}

	if _, err = c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("rejected")}); err == nil {
		t.Fatal("a rejected operation created a stack")
	}

	call("DeleteStack", map[string]any{"StackName": id})
	wantStatus(ctx, t, c, id, cfntypes.StackStatusDeleteComplete)
	var deleted map[string]any
	for _, summary := range call("ListStacks", map[string]any{})["StackSummaries"].([]any) {
		if summary.(map[string]any)["StackId"] == id {
			deleted = summary.(map[string]any)
		}
	}

	if deleted == nil || deleted["StackStatus"] != "DELETE_COMPLETE" || deleted["DeletionTime"] == nil {
		t.Fatalf("deleted stack = %+v", deleted)
	}

	if call("DescribeStacks", map[string]any{"StackName": id})["Stacks"].([]any)[0].(map[string]any)["StackStatus"] !=
		"DELETE_COMPLETE" {
		t.Fatal("deleted stack is not described by ID")
	}
}

func TestConsoleCloudFormationDeleteAuthorization(t *testing.T) {
	ctx := context.Background()
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	c := cfnClient(s)
	created, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String(
			"denied",
		),
		TemplateBody: aws.String(`{"Resources": {"Topic": {"Type": "AWS::SNS::Topic"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "denied", cfntypes.StackStatusCreateComplete)
	key := authorizationUser(t, s)
	authorizationPolicy(t, s, `{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":"cloudformation:*","Resource":"*"},`+
		`{"Effect":"Deny","Action":"cloudformation:DeleteStack","Resource":"*"}]}`)
	credentials := aws.Credentials{
		AccessKeyID: aws.ToString(key.AccessKeyId), SecretAccessKey: aws.ToString(key.SecretAccessKey),
	}
	// The same principal may read, but IAM denies the delete.
	input := map[string]any{"StackName": aws.ToString(created.StackId)}
	consoleResponse(t, s, signedCloudFormationRequest(t, s, &credentials, "DescribeStacks", input), http.StatusOK)
	consoleResponse(t, s, signedCloudFormationRequest(t, s, &credentials, "DeleteStack", input),
		http.StatusForbidden)
	st := settled(ctx, t, c, "denied")
	if st.StackStatus != cfntypes.StackStatusCreateComplete {
		t.Fatalf("denied delete changed the stack: %s", st.StackStatus)
	}
}

func signedCloudFormationRequest(
	t *testing.T,
	s *httptest.Server,
	credentials *aws.Credentials,
	operation string,
	input map[string]any,
) *http.Request {
	t.Helper()
	r := consoleLambdaRequest(t, s, "cloudformation", operation, "000000000000", "us-east-1", input)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}

	r.Body = io.NopCloser(strings.NewReader(string(body)))
	sum := sha256.Sum256(body)
	if err = v4.NewSigner().
		SignHTTP(context.Background(), *credentials, r, hex.EncodeToString(sum[:]), "cloudformation",
			"us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	return r
}
