package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cc "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
)

func controlClient(s *httptest.Server, key, region string) *cloudcontrol.Client {
	return cloudcontrol.New(cloudcontrol.Options{
		Region: region, BaseEndpoint: aws.String(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: key}, nil
		}),
	})
}

func controlCode(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func controlEvent(t *testing.T, e *cc.ProgressEvent, status cc.OperationStatus, code cc.HandlerErrorCode) {
	t.Helper()
	if e == nil || e.OperationStatus != status || e.ErrorCode != code ||
		aws.ToString(e.RequestToken) == "" || e.EventTime == nil {
		t.Fatalf("progress = %+v, want %s/%s", e, status, code)
	}
}

func controlModel(t *testing.T, c *cloudcontrol.Client, name, id string) map[string]any {
	t.Helper()
	out, err := c.GetResource(context.Background(), &cloudcontrol.GetResourceInput{TypeName: &name, Identifier: &id})
	if err != nil {
		t.Fatal(err)
	}

	var model map[string]any
	if err := json.Unmarshal([]byte(aws.ToString(out.ResourceDescription.Properties)), &model); err != nil {
		t.Fatal(err)
	}

	return model
}

func TestCloudControlBucketLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := controlClient(s, "test", "us-east-1")
	objects := s3Client(s, "test", "us-east-1")
	typeName := "AWS::S3::Bucket"
	in := &cloudcontrol.CreateResourceInput{
		TypeName: &typeName, ClientToken: aws.String("bucket-create"),
		DesiredState: aws.String(`{"BucketName":"control-files","VersioningConfiguration":{"Status":"Enabled"},` +
			`"Tags":[{"Key":"env","Value":"test"}]}`),
	}
	created, err := c.CreateResource(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, created.ProgressEvent, cc.OperationStatusSuccess, "")
	id := aws.ToString(created.ProgressEvent.Identifier)
	if id != "control-files" || created.ProgressEvent.Operation != cc.OperationCreate {
		t.Fatalf("created = %+v", created.ProgressEvent)
	}

	status, err := c.GetResourceRequestStatus(ctx, &cloudcontrol.GetResourceRequestStatusInput{
		RequestToken: created.ProgressEvent.RequestToken,
	})
	if err != nil || !reflect.DeepEqual(status.ProgressEvent, created.ProgressEvent) {
		t.Fatalf("status = %+v, err = %v", status, err)
	}

	retry, err := c.CreateResource(ctx, in)
	if err != nil || !reflect.DeepEqual(retry.ProgressEvent, created.ProgressEvent) {
		t.Fatalf("retry = %+v, err = %v", retry, err)
	}

	in.DesiredState = aws.String(`{"BucketName":"other-files"}`)
	_, err = c.CreateResource(ctx, in)
	controlCode(t, err, "ClientTokenConflictException")
	in.ClientToken = aws.String("duplicate")
	in.DesiredState = aws.String(`{"BucketName":"control-files"}`)
	duplicate, err := c.CreateResource(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, duplicate.ProgressEvent, cc.OperationStatusFailed, cc.HandlerErrorCodeAlreadyExists)
	model := controlModel(t, c, typeName, id)
	if model["Arn"] != "arn:aws:s3:::control-files" || model["VersioningConfiguration"] == nil {
		t.Fatalf("bucket model = %v", model)
	}

	updated, err := c.UpdateResource(ctx, &cloudcontrol.UpdateResourceInput{
		TypeName: &typeName, Identifier: &id, ClientToken: aws.String("bucket-update"),
		PatchDocument: aws.String(`[{"op":"test","path":"/Tags/0/Value","value":"test"},` +
			`{"op":"replace","path":"/Tags/0/Value","value":"prod"},` +
			`{"op":"replace","path":"/VersioningConfiguration/Status","value":"Suspended"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, updated.ProgressEvent, cc.OperationStatusSuccess, "")
	tags, err := objects.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: &id})
	if err != nil || len(tags.TagSet) != 1 || aws.ToString(tags.TagSet[0].Value) != "prod" {
		t.Fatalf("S3 tags = %+v, err = %v", tags, err)
	}

	_, err = objects.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket:  &id,
		Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("live"), Value: aws.String("yes")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	model = controlModel(t, c, typeName, id)
	if model["Tags"].([]any)[0].(map[string]any)["Key"] != "live" {
		t.Fatalf("live tags = %v", model)
	}

	for _, patch := range []string{
		`[{"op":"replace","path":"/BucketName","value":"renamed-bucket"}]`,
		`[{"op":"remove","path":"/Arn"}]`,
	} {
		out, operationErr := c.UpdateResource(ctx, &cloudcontrol.UpdateResourceInput{
			TypeName: &typeName, Identifier: &id, PatchDocument: &patch,
		})
		if operationErr != nil {
			t.Fatal(operationErr)
		}

		controlEvent(t, out.ProgressEvent, cc.OperationStatusFailed, cc.HandlerErrorCodeNotUpdatable)
	}

	put, err := objects.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: &id, Key: aws.String("hello"), Body: strings.NewReader("hi")},
	)
	if err != nil {
		t.Fatal(err)
	}

	nonempty, err := c.DeleteResource(ctx, &cloudcontrol.DeleteResourceInput{TypeName: &typeName, Identifier: &id})
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, nonempty.ProgressEvent, cc.OperationStatusFailed, cc.HandlerErrorCodeResourceConflict)
	_, err = objects.DeleteObject(
		ctx,
		&s3.DeleteObjectInput{Bucket: &id, Key: aws.String("hello"), VersionId: put.VersionId},
	)
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := c.DeleteResource(ctx, &cloudcontrol.DeleteResourceInput{TypeName: &typeName, Identifier: &id})
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, deleted.ProgressEvent, cc.OperationStatusSuccess, "")
	_, err = c.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: &typeName, Identifier: &id})
	controlCode(t, err, "ResourceNotFoundException")
}

func TestCloudControlQueueWorkflow(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := controlClient(s, "test", "us-east-1")
	queues := sqsClient(s, "test")
	typeName := "AWS::SQS::Queue"
	created, err := c.CreateResource(ctx, &cloudcontrol.CreateResourceInput{
		TypeName: &typeName, DesiredState: aws.String(`{"QueueName":"control-jobs","VisibilityTimeout":45,` +
			`"Tags":[{"Key":"env","Value":"dev"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, created.ProgressEvent, cc.OperationStatusSuccess, "")
	id := aws.ToString(created.ProgressEvent.Identifier)
	model := controlModel(t, c, typeName, id)
	if model["VisibilityTimeout"] != float64(45) || model["QueueUrl"] != id || model["QueueName"] != "control-jobs" {
		t.Fatalf("queue model = %v", model)
	}

	_, err = queues.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &id, MessageBody: aws.String("work")})
	if err != nil {
		t.Fatal(err)
	}

	received, err := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &id})
	if err != nil || len(received.Messages) != 1 || aws.ToString(received.Messages[0].Body) != "work" {
		t.Fatalf("messages = %+v, err = %v", received, err)
	}

	patch := `[{"op":"replace","path":"/VisibilityTimeout","value":90},` +
		`{"op":"remove","path":"/Tags"}]`
	update := &cloudcontrol.UpdateResourceInput{
		TypeName: &typeName, Identifier: &id, PatchDocument: &patch, ClientToken: aws.String("queue-update"),
	}
	out, err := c.UpdateResource(ctx, update)
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, out.ProgressEvent, cc.OperationStatusSuccess, "")
	retry, err := c.UpdateResource(ctx, update)
	if err != nil || !reflect.DeepEqual(out.ProgressEvent, retry.ProgressEvent) {
		t.Fatalf("update retry = %+v, err = %v", retry, err)
	}

	attrs, err := queues.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: &id, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameVisibilityTimeout},
	})
	if err != nil || attrs.Attributes["VisibilityTimeout"] != "90" {
		t.Fatalf("attributes = %+v, err = %v", attrs, err)
	}

	tags, err := queues.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: &id})
	if err != nil || len(tags.Tags) != 0 {
		t.Fatalf("tags = %+v, err = %v", tags, err)
	}

	for _, key := range []string{"team-b", "test"} {
		region := "us-east-1"
		if key == "test" {
			region = "eu-west-1"
		}

		other := controlClient(s, key, region)
		_, err = other.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: &typeName, Identifier: &id})
		controlCode(t, err, "ResourceNotFoundException")
		_, err = other.GetResourceRequestStatus(ctx, &cloudcontrol.GetResourceRequestStatusInput{
			RequestToken: created.ProgressEvent.RequestToken,
		})
		controlCode(t, err, "RequestTokenNotFoundException")
	}

	deleted, err := c.DeleteResource(ctx, &cloudcontrol.DeleteResourceInput{
		TypeName: &typeName, Identifier: &id, ClientToken: aws.String("queue-delete"),
	})
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, deleted.ProgressEvent, cc.OperationStatusSuccess, "")
	missing, err := c.DeleteResource(ctx, &cloudcontrol.DeleteResourceInput{TypeName: &typeName, Identifier: &id})
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, missing.ProgressEvent, cc.OperationStatusFailed, cc.HandlerErrorCodeNotFound)
}

func TestCloudControlListingAndExternalResources(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := controlClient(s, "test", "us-east-1")
	typeName := "AWS::SQS::Queue"
	queues := sqsClient(s, "test")
	id := createQueue(t, queues, "external", nil)
	model := controlModel(t, c, typeName, id)
	if model["QueueName"] != "external" {
		t.Fatalf("external model = %v", model)
	}

	stacks := cfnClient(s)
	_, err := stacks.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("control-stack"), TemplateBody: aws.String(`{"Resources":{"Queue":{"Type":` +
			`"AWS::SQS::Queue","Properties":{"QueueName":"stack-queue"}}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	settled(ctx, t, stacks, "control-stack")
	for i := range 2 {
		out, operationErr := c.CreateResource(ctx, &cloudcontrol.CreateResourceInput{
			TypeName: &typeName, DesiredState: aws.String(`{}`),
		})
		if operationErr != nil {
			t.Fatal(operationErr)
		}

		controlEvent(t, out.ProgressEvent, cc.OperationStatusSuccess, "")
		if i == 0 {
			if !strings.Contains(aws.ToString(out.ProgressEvent.Identifier), "cloudcontrol-resource-") {
				t.Fatalf("generated identifier = %v", out.ProgressEvent.Identifier)
			}
		}
	}

	var ids []string
	var token *string
	for {
		out, operationErr := c.ListResources(ctx, &cloudcontrol.ListResourcesInput{
			TypeName: &typeName, MaxResults: aws.Int32(1), NextToken: token,
		})
		if operationErr != nil {
			t.Fatal(operationErr)
		}

		for _, r := range out.ResourceDescriptions {
			ids = append(ids, aws.ToString(r.Identifier))
		}

		token = out.NextToken
		if token == nil {
			break
		}
	}

	if len(ids) != 4 {
		t.Fatalf("listed resources = %v", ids)
	}

	out, err := c.ListResourceRequests(ctx, &cloudcontrol.ListResourceRequestsInput{
		MaxResults: aws.Int32(1), ResourceRequestStatusFilter: &cc.ResourceRequestStatusFilter{
			Operations: []cc.Operation{
				cc.OperationCreate,
			},
			OperationStatuses: []cc.OperationStatus{cc.OperationStatusSuccess},
		},
	})
	if err != nil || len(out.ResourceRequestStatusSummaries) != 1 || out.NextToken == nil {
		t.Fatalf("request page = %+v, err = %v", out, err)
	}

	next, err := c.ListResourceRequests(ctx, &cloudcontrol.ListResourceRequestsInput{NextToken: out.NextToken})
	if err != nil || len(next.ResourceRequestStatusSummaries) != 1 {
		t.Fatalf("next request page = %+v, err = %v", next, err)
	}

	_, err = c.ListResources(ctx, &cloudcontrol.ListResourcesInput{TypeName: &typeName, NextToken: aws.String("!")})
	controlCode(t, err, "InvalidRequestException")
}

func TestCloudControlWireValidation(t *testing.T) {
	s := server(t)
	for _, tc := range []struct{ operation, body, code string }{
		{"CreateResource", `{"TypeName":"AWS::S3::Bucket","DesiredState":"null"}`, "InvalidRequestException"},
		{"CreateResource", `{"TypeName":"AWS::S3::Bucket","DesiredState":"{\"Unknown\":true}"}`, "InvalidRequestException"},
		{"CreateResource", `{"TypeName":"AWS::S3::Bucket","DesiredState":"{\"Tags\":null}"}`, "InvalidRequestException"},
		{
			"CreateResource", `{"TypeName":"AWS::S3::Bucket","DesiredState":"{\"BucketName\":\"UPPER\"}"}`,
			"InvalidRequestException",
		},
		{"CreateResource", `{"TypeName":"AWS::S3::Bucket","DesiredState":"{}","RoleArn":"role"}`, "ValidationException"},
		{"GetResource", `{"TypeName":"AWS::S3::Bucket","Identifier":"missing"}`, "ResourceNotFoundException"},
		{"GetResource", `{"TypeName":"AWS::DynamoDB::Table","Identifier":"missing"}`, "TypeNotFoundException"},
		{"GetResourceRequestStatus", `{"RequestToken":"missing"}`, "RequestTokenNotFoundException"},
		{"ListResourceRequests", `{"MaxResults":0}`, "InvalidRequestException"},
	} {
		t.Run(tc.operation+tc.code+tc.body, func(t *testing.T) {
			req, err := http.NewRequestWithContext(
				context.Background(),
				http.MethodPost,
				s.URL,
				strings.NewReader(tc.body),
			)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("Content-Type", "application/x-amz-json-1.0")
			req.Header.Set("X-Amz-Target", "CloudApiService."+tc.operation)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}

			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode < 400 || !strings.Contains(string(body), tc.code) {
				t.Fatalf("wire response = %d %s", resp.StatusCode, body)
			}
		})
	}
}

func TestCloudControlJSONPatch(t *testing.T) {
	s := server(t)
	c := controlClient(s, "test", "us-east-1")
	ctx := context.Background()
	typeName := "AWS::SQS::Queue"
	created, err := c.CreateResource(ctx, &cloudcontrol.CreateResourceInput{
		TypeName: &typeName, DesiredState: aws.String(`{"QueueName":"patch-queue","Tags":[` +
			`{"Key":"env","Value":"dev"},{"Key":"owner","Value":"alice"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, created.ProgressEvent, cc.OperationStatusSuccess, "")
	id := created.ProgressEvent.Identifier
	updated, err := c.UpdateResource(ctx, &cloudcontrol.UpdateResourceInput{
		TypeName: &typeName, Identifier: id, PatchDocument: aws.String(`[` +
			`{"op":"copy","from":"/Tags/0/Value","path":"/Tags/1/Value"},` +
			`{"op":"move","from":"/Tags/1","path":"/Tags/0"},` +
			`{"op":"test","path":"/Tags/0/Key","value":"owner"},` +
			`{"op":"remove","path":"/Tags/1"},` +
			`{"op":"add","path":"/Tags/-","value":{"Key":"new","Value":"value"}}]`),
	})
	if err != nil {
		t.Fatal(err)
	}

	controlEvent(t, updated.ProgressEvent, cc.OperationStatusSuccess, "")
	before := controlModel(t, c, typeName, *id)
	for _, patch := range []string{
		`[{"op":"replace","path":"/VisibilityTimeout","value":77},` +
			`{"op":"test","path":"/QueueName","value":"wrong"}]`,
		`[{"op":"remove","path":"/Tags/-1"}]`,
		`[{"op":"add","path":"/Unsupported","value":true}]`,
		`[{"op":"replace","path":"/DelaySeconds","value":-1}]`,
	} {
		failed, patchErr := c.UpdateResource(ctx, &cloudcontrol.UpdateResourceInput{
			TypeName: &typeName, Identifier: id, PatchDocument: &patch,
		})
		if patchErr != nil {
			t.Fatal(patchErr)
		}

		controlEvent(t, failed.ProgressEvent, cc.OperationStatusFailed, cc.HandlerErrorCodeInvalidRequest)
		if after := controlModel(t, c, typeName, *id); !reflect.DeepEqual(before, after) {
			t.Fatalf("failed patch changed the resource: before=%v after=%v", before, after)
		}
	}
}

func TestCloudControlPersistentRestart(t *testing.T) {
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := controlClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := &cloudcontrol.CreateResourceInput{
		TypeName: aws.String("AWS::SQS::Queue"), DesiredState: aws.String(`{"QueueName":"durable-control"}`),
		ClientToken: aws.String("durable-token"),
	}
	created, err := c.CreateResource(ctx, in)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	t.Cleanup(stop)
	c = controlClient(s, "test", "us-east-1")
	retry, err := c.CreateResource(ctx, in)
	if err != nil || !reflect.DeepEqual(created.ProgressEvent, retry.ProgressEvent) {
		t.Fatalf("restart retry = %+v, err = %v", retry, err)
	}

	model := controlModel(t, c, *in.TypeName, *created.ProgressEvent.Identifier)
	if model["QueueName"] != "durable-control" {
		t.Fatalf("restart model = %v", model)
	}
}
