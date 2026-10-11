package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func batchClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func batchDefinition() *sdk.RegisterJobDefinitionInput {
	return &sdk.RegisterJobDefinitionInput{
		JobDefinitionName: new("worker"),
		Type:              types.JobDefinitionTypeContainer,
		Parameters:        map[string]string{"input": "records"},
		Tags:              map[string]string{"owner": "platform"},
		ContainerProperties: &types.ContainerProperties{
			Image: new("busybox:1.37"),
			ResourceRequirements: []types.ResourceRequirement{
				{Type: types.ResourceTypeVcpu, Value: new("2")},
				{Type: types.ResourceTypeMemory, Value: new("128")},
			},
			Command:          []string{"echo", "Ref::input"},
			Environment:      []types.KeyValuePair{{Name: new("LOCAL"), Value: new("yes")}},
			JobRoleArn:       new("arn:aws:iam::000000000000:role/job"),
			ExecutionRoleArn: new("arn:aws:iam::000000000000:role/pull"),
		},
	}
}

func batchFixture(
	t *testing.T,
	s *httptest.Server,
) (*sdk.CreateComputeEnvironmentOutput, *sdk.CreateJobQueueOutput, *sdk.RegisterJobDefinitionOutput) {
	t.Helper()
	ctx := context.Background()
	net := ec2Client(s, "test", "us-east-1")
	c := batchClient(s, "test", "us-east-1")
	v, e := net.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.91.0.0/16")})
	if e != nil {
		t.Fatal(e)
	}
	sn, e := net.CreateSubnet(ctx, &ec2sdk.CreateSubnetInput{VpcId: v.Vpc.VpcId, CidrBlock: new("10.91.1.0/24")})
	if e != nil {
		t.Fatal(e)
	}
	g, e := net.CreateSecurityGroup(
		ctx,
		&ec2sdk.CreateSecurityGroupInput{VpcId: v.Vpc.VpcId, GroupName: new("batch"), Description: new("metadata")},
	)
	if e != nil {
		t.Fatal(e)
	}
	ce, e := c.CreateComputeEnvironment(
		ctx,
		&sdk.CreateComputeEnvironmentInput{
			ComputeEnvironmentName: new("managed"),
			Type:                   types.CETypeManaged,
			State:                  types.CEStateEnabled,
			Tags:                   map[string]string{"owner": "platform"},
			ComputeResources: &types.ComputeResource{
				Type:               types.CRTypeEc2,
				AllocationStrategy: types.CRAllocationStrategyBestFitProgressive,
				MinvCpus:           new(int32(2)),
				MaxvCpus:           new(int32(16)),
				DesiredvCpus:       new(int32(4)),
				InstanceTypes:      []string{"c5.large", "m5"},
				InstanceRole:       new("worker"),
				Subnets:            []string{*sn.Subnet.SubnetId},
				SecurityGroupIds:   []string{*g.GroupId},
			},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	q, e := c.CreateJobQueue(
		ctx,
		&sdk.CreateJobQueueInput{
			JobQueueName: new("queue"),
			Priority:     new(int32(9)),
			ComputeEnvironmentOrder: []types.ComputeEnvironmentOrder{
				{Order: new(int32(2)), ComputeEnvironment: ce.ComputeEnvironmentArn},
			},
			Tags: map[string]string{"owner": "platform"},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	jd, e := c.RegisterJobDefinition(ctx, batchDefinition())
	if e != nil {
		t.Fatal(e)
	}
	return ce, q, jd
}

func TestBatchSDKLifecycleScopeAndTagging(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := batchClient(s, "test", "us-east-1")
	ce, q, jd := batchFixture(t, s)
	envs, e := c.DescribeComputeEnvironments(
		ctx,
		&sdk.DescribeComputeEnvironmentsInput{ComputeEnvironments: []string{*ce.ComputeEnvironmentName}},
	)
	if e != nil || len(envs.ComputeEnvironments) != 1 {
		t.Fatalf("describe %v %v", envs, e)
	}
	env := envs.ComputeEnvironments[0]
	if env.EcsClusterArn != nil || env.Status != types.CEStatusValid || *env.ComputeResources.DesiredvCpus != 4 ||
		!strings.Contains(*env.StatusReason, "metadata") {
		t.Fatalf("metadata %#v", env)
	}
	queues, e := c.DescribeJobQueues(ctx, &sdk.DescribeJobQueuesInput{JobQueues: []string{*q.JobQueueArn}})
	if e != nil || *queues.JobQueues[0].ComputeEnvironmentOrder[0].ComputeEnvironment != *ce.ComputeEnvironmentArn {
		t.Fatalf("queue relation %v %v", queues, e)
	}
	_, e = c.CreateComputeEnvironment(
		ctx,
		&sdk.CreateComputeEnvironmentInput{
			ComputeEnvironmentName: ce.ComputeEnvironmentName,
			Type:                   types.CETypeUnmanaged,
			ServiceRole:            new("batch"),
		},
	)
	code(t, e, "ClientException")
	var client *types.ClientException
	if !errors.As(e, &client) {
		t.Fatalf("unmodeled error %v", e)
	}
	second, e := c.RegisterJobDefinition(ctx, batchDefinition())
	if e != nil || *second.Revision != 2 {
		t.Fatalf("revision %v %v", second, e)
	}
	if _, e = c.DeregisterJobDefinition(
		ctx,
		&sdk.DeregisterJobDefinitionInput{JobDefinition: jd.JobDefinitionArn},
	); e != nil {
		t.Fatal(e)
	}
	defs, e := c.DescribeJobDefinitions(ctx, &sdk.DescribeJobDefinitionsInput{JobDefinitionName: new("worker")})
	if e != nil || len(defs.JobDefinitions) != 2 || *defs.JobDefinitions[0].Status != "INACTIVE" ||
		defs.JobDefinitions[0].ContainerProperties.Environment[0].Value == nil {
		t.Fatalf("history %v %v", defs, e)
	}
	if _, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: jd.JobDefinitionArn, Tags: map[string]string{"revision": "one"}},
	); e != nil {
		t.Fatal(e)
	}
	tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: jd.JobDefinitionArn})
	if e != nil || tags.Tags["revision"] != "one" {
		t.Fatalf("tags %v %v", tags, e)
	}
	if _, e = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: jd.JobDefinitionArn, TagKeys: []string{"revision"}},
	); e != nil {
		t.Fatal(e)
	}
	tagging := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	inventory, e := tagging.GetResources(
		ctx,
		&resourcegroupstaggingapi.GetResourcesInput{
			ResourceTypeFilters: []string{"batch:compute-environment", "batch:job-queue", "batch:job-definition"},
		},
	)
	if e != nil || len(inventory.ResourceTagMappingList) != 4 {
		t.Fatalf("tagging inventory %v %v", inventory, e)
	}
	tagged, e := tagging.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{*q.JobQueueArn},
			Tags:            map[string]string{"via": "tagging"},
		},
	)
	if e != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("tagging mutation %v %v", tagged, e)
	}
	_, e = batchClient(
		s,
		"other",
		"us-west-2",
	).UpdateJobQueue(ctx, &sdk.UpdateJobQueueInput{JobQueue: q.JobQueueArn, Priority: new(int32(1))})
	code(t, e, "ClientException")
	_, e = c.DeleteComputeEnvironment(
		ctx,
		&sdk.DeleteComputeEnvironmentInput{ComputeEnvironment: ce.ComputeEnvironmentArn},
	)
	code(t, e, "ClientException")
	if _, e = c.UpdateComputeEnvironment(
		ctx,
		&sdk.UpdateComputeEnvironmentInput{
			ComputeEnvironment: ce.ComputeEnvironmentArn,
			State:              types.CEStateDisabled,
			ComputeResources:   &types.ComputeResourceUpdate{DesiredvCpus: new(int32(8))},
		},
	); e != nil {
		t.Fatal(e)
	}
	_, e = c.DeleteComputeEnvironment(
		ctx,
		&sdk.DeleteComputeEnvironmentInput{ComputeEnvironment: ce.ComputeEnvironmentArn},
	)
	code(t, e, "ClientException")
	_, e = c.DeleteJobQueue(ctx, &sdk.DeleteJobQueueInput{JobQueue: q.JobQueueArn})
	code(t, e, "ClientException")
	if _, e = c.UpdateJobQueue(
		ctx,
		&sdk.UpdateJobQueueInput{JobQueue: q.JobQueueArn, State: types.JQStateDisabled, Priority: new(int32(3))},
	); e != nil {
		t.Fatal(e)
	}
	if _, e = c.DeleteJobQueue(ctx, &sdk.DeleteJobQueueInput{JobQueue: q.JobQueueName}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.DeleteComputeEnvironment(
		ctx,
		&sdk.DeleteComputeEnvironmentInput{ComputeEnvironment: ce.ComputeEnvironmentName},
	); e != nil {
		t.Fatal(e)
	}
}

func batchWire(t *testing.T, s *httptest.Server, method, path, body string) (status int, result map[string]any) {
	t.Helper()
	req, e := http.NewRequestWithContext(context.Background(), method, s.URL+path, bytes.NewBufferString(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/batch/aws4_request, SignedHeaders=host, Signature=dummy",
	)
	resp, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatalf("wire %s %v", raw, e)
		}
	}
	return resp.StatusCode, out
}

func TestBatchRawWireUnsupportedMembersAndLabels(t *testing.T) {
	s := server(t)
	status, out := batchWire(
		t,
		s,
		"POST",
		"/v1/createcomputeenvironment",
		`{"computeEnvironmentName":"raw","type":"UNMANAGED","serviceRole":"batch"}`,
	)
	if status != 200 || out["computeEnvironmentArn"] == nil {
		t.Fatalf("create %d %v", status, out)
	}
	arn := out["computeEnvironmentArn"].(string)
	path := "/v1/tags/" + url.PathEscape(arn)
	status, out = batchWire(t, s, "POST", path, `{"tags":{"wire":"yes"}}`)
	if status != 200 {
		t.Fatalf("label tag %d %v", status, out)
	}
	status, out = batchWire(t, s, "GET", path, "")
	tags, _ := out["tags"].(map[string]any)
	if status != 200 || tags["wire"] != "yes" {
		t.Fatalf("label get %d %v", status, out)
	}
	status, out = batchWire(t, s, "DELETE", path+"?tagKeys=wire&tagKeys=absent", "")
	if status != 200 {
		t.Fatalf("query untag %d %v", status, out)
	}
	for _, body := range []string{
		`{}`,
		`{"computeEnvironmentName":"","type":"UNMANAGED"}`,
		`{"computeEnvironmentName":"bad","type":"MANAGED",
 "computeResources":{"type":"EC2","maxvCpus":1.5}}`,
	} {
		status, out = batchWire(t, s, "POST", "/v1/createcomputeenvironment", body)
		errorType, _ := out["__type"].(string)
		if status != 400 || (errorType != "ClientException" && errorType != "SerializationException") {
			t.Fatalf("modeled validation %d %v", status, out)
		}
	}
	status, out = batchWire(t, s, "POST", path, `{"tags":{"duplicate":"first","duplicate":"second"}}`)
	if status != 400 || out["__type"] != "ClientException" {
		t.Fatalf("duplicate tags %d %v", status, out)
	}
	status, out = batchWire(t, s, "GET", path, "")
	if tags, _ := out["tags"].(map[string]any); status != 200 || len(tags) != 0 {
		t.Fatalf("failed duplicate tag mutation %d %v", status, out)
	}
	for _, test := range []struct{ path, body string }{
		{"createcomputeenvironment", `{"computeEnvironmentName":"future","type":"UNMANAGED",
 "serviceRole":"batch","ecsSettings":null}`},
		{"createcomputeenvironment", `{"computeEnvironmentName":"future","type":"MANAGED",
 "computeResources":{"type":"EC2","maxvCpus":2,"launchTemplate":{}}}`},
		{"registerjobdefinition", `{"jobDefinitionName":"future","type":"container",
 "containerProperties":{"image":"busybox","vcpus":1,"memory":128,"privileged":false}}`},
		{"registerjobdefinition", `{"jobDefinitionName":"future","type":"container",
 "containerProperties":{"image":"busybox","vcpus":1,"memory":128,"volumes":[]}}`},
		{"registerjobdefinition", `{"jobDefinitionName":"future","type":"container","eksProperties":null}`},
		{"createjobqueue", `{"jobQueueName":"future","priority":1,"computeEnvironmentOrder":[],"schedulingPolicyArn":""}`},
	} {
		status, out = batchWire(t, s, "POST", "/v1/"+test.path, test.body)
		if status != 501 {
			t.Fatalf("unsupported %s: %d %v", test.body, status, out)
		}
	}
	status, out = batchWire(t, s, "POST", "/v1/submitjob", `{"jobName":"fake"}`)
	if status < 400 {
		t.Fatalf("execution succeeded %d %v", status, out)
	}
	status, out = batchWire(t, s, "POST", "/v1/describecomputeenvironments", `{"maxResults":101}`)
	if status != 400 {
		t.Fatalf("limit %d %v", status, out)
	}
}

func TestBatchVerifySignatures(t *testing.T) {
	s := verifyingServer(t)
	ctx := context.Background()
	c := batchClient(s, "test", "us-east-1")
	ce, _, _ := batchFixture(t, s)
	if _, e := c.ListTagsForResource(
		ctx,
		&sdk.ListTagsForResourceInput{ResourceArn: ce.ComputeEnvironmentArn},
	); e != nil {
		t.Fatal(e)
	}
	if _, e := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: ce.ComputeEnvironmentArn, Tags: map[string]string{"signed": "yes"}},
	); e != nil {
		t.Fatal(e)
	}
	if _, e := c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: ce.ComputeEnvironmentArn, TagKeys: []string{"signed"}},
	); e != nil {
		t.Fatal(e)
	}
	_, e := batchClient(s, "wrong", "us-east-1").DescribeJobQueues(ctx, &sdk.DescribeJobQueuesInput{})
	code(t, e, "InvalidSignatureException")
}

type batchSnapshot struct {
	Environments []types.ComputeEnvironmentDetail
	Queues       []types.JobQueueDetail
	Definitions  []types.JobDefinition
}

func snapshotBatch(t *testing.T, c *sdk.Client) batchSnapshot {
	t.Helper()
	ctx := context.Background()
	ce, e := c.DescribeComputeEnvironments(ctx, &sdk.DescribeComputeEnvironmentsInput{})
	if e != nil {
		t.Fatal(e)
	}
	q, e := c.DescribeJobQueues(ctx, &sdk.DescribeJobQueuesInput{})
	if e != nil {
		t.Fatal(e)
	}
	d, e := c.DescribeJobDefinitions(ctx, &sdk.DescribeJobDefinitionsInput{})
	if e != nil {
		t.Fatal(e)
	}
	return batchSnapshot{ce.ComputeEnvironments, q.JobQueues, d.JobDefinitions}
}

func TestBatchPersistenceFixtureImportAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	ce, q, jd := batchFixture(t, s)
	c := batchClient(s, "test", "us-east-1")
	if _, e := c.DeregisterJobDefinition(
		ctx,
		&sdk.DeregisterJobDefinitionInput{JobDefinition: jd.JobDefinitionArn},
	); e != nil {
		t.Fatal(e)
	}
	if _, e := c.RegisterJobDefinition(ctx, batchDefinition()); e != nil {
		t.Fatal(e)
	}
	if _, e := c.UpdateComputeEnvironment(
		ctx,
		&sdk.UpdateComputeEnvironmentInput{ComputeEnvironment: ce.ComputeEnvironmentArn, State: types.CEStateDisabled},
	); e != nil {
		t.Fatal(e)
	}
	if _, e := c.UpdateJobQueue(
		ctx,
		&sdk.UpdateJobQueueInput{JobQueue: q.JobQueueArn, State: types.JQStateDisabled, Priority: new(int32(13))},
	); e != nil {
		t.Fatal(e)
	}
	if _, e := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: jd.JobDefinitionArn, Tags: map[string]string{"retained": "inactive"}},
	); e != nil {
		t.Fatal(e)
	}
	before := snapshotBatch(t, c)
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	after := snapshotBatch(t, batchClient(s, "test", "us-east-1"))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("SQLite restart lost metadata")
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "batch.tar.gz")
	if e := archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "child")
	if e := archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	imported := snapshotBatch(t, batchClient(s, "test", "us-east-1"))
	if !reflect.DeepEqual(before, imported) {
		t.Fatal("fixture import lost IDs, relations or revisions")
	}
	stop()
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c = batchClient(s, "test", "us-east-1")
	if !reflect.DeepEqual(before, snapshotBatch(t, c)) {
		t.Fatal("normal imported restart lost state")
	}
	next, e := c.RegisterJobDefinition(ctx, batchDefinition())
	if e != nil || *next.Revision != 3 {
		t.Fatalf("imported revision sequence %v %v", next, e)
	}
	status, body := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatalf("reset %d %s", status, body)
	}
	empty := snapshotBatch(t, c)
	if len(empty.Environments)+len(empty.Queues)+len(empty.Definitions) != 0 {
		t.Fatal("reset retained records")
	}
	resp, e := s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), *jd.JobDefinitionArn) {
		t.Fatal("reset retained inventory")
	}
}
