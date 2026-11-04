package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"

	batchapi "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
	batchservice "github.com/amjadjibon/tinystack/internal/services/batch"
	ec2service "github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
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

func fargateDefinition() *sdk.RegisterJobDefinitionInput {
	in := batchDefinition()
	in.JobDefinitionName = new("fargate")
	in.PlatformCapabilities = []types.PlatformCapability{types.PlatformCapabilityFargate}
	c := in.ContainerProperties
	c.ResourceRequirements = []types.ResourceRequirement{
		{Type: types.ResourceTypeVcpu, Value: new("0.25")},
		{Type: types.ResourceTypeMemory, Value: new("512")},
	}
	c.NetworkConfiguration = &types.NetworkConfiguration{AssignPublicIp: types.AssignPublicIpEnabled}
	c.FargatePlatformConfiguration = &types.FargatePlatformConfiguration{PlatformVersion: new("LATEST")}
	c.RuntimePlatform = &types.RuntimePlatform{CpuArchitecture: new("ARM64"), OperatingSystemFamily: new("LINUX")}
	return in
}

func TestBatchFargateSDK(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := batchClient(s, "test", "us-east-1")
	ec2, _, _ := batchFixture(t, s)
	envs, err := c.DescribeComputeEnvironments(ctx, &sdk.DescribeComputeEnvironmentsInput{})
	if err != nil {
		t.Fatal(err)
	}
	network := envs.ComputeEnvironments[0].ComputeResources
	names := []string{}
	for _, typ := range []types.CRType{types.CRTypeFargate, types.CRTypeFargateSpot} {
		in := &sdk.CreateComputeEnvironmentInput{
			ComputeEnvironmentName: new(string(typ)),
			Type:                   types.CETypeManaged,
			ComputeResources: &types.ComputeResource{
				Type:             typ,
				MaxvCpus:         new(int32(8)),
				Subnets:          network.Subnets,
				SecurityGroupIds: network.SecurityGroupIds,
			},
		}
		out, e := c.CreateComputeEnvironment(ctx, in)
		if e != nil {
			t.Fatal(e)
		}
		names = append(names, *out.ComputeEnvironmentName)
		for _, mutate := range []func(*types.ComputeResource){
			func(r *types.ComputeResource) { r.MinvCpus = new(int32(0)) },
			func(r *types.ComputeResource) { r.DesiredvCpus = new(int32(0)) },
			func(r *types.ComputeResource) { r.InstanceTypes = []string{"optimal"} },
			func(r *types.ComputeResource) { r.InstanceRole = new("worker") },
			func(r *types.ComputeResource) { r.AllocationStrategy = types.CRAllocationStrategyBestFit },
			func(r *types.ComputeResource) { r.MaxvCpus = new(int32(0)) },
			func(r *types.ComputeResource) { r.Subnets = []string{"subnet-12345678"} },
			func(r *types.ComputeResource) { r.SecurityGroupIds = []string{"sg-12345678"} },
		} {
			bad := *in.ComputeResources
			mutate(&bad)
			_, e = c.CreateComputeEnvironment(
				ctx,
				&sdk.CreateComputeEnvironmentInput{
					ComputeEnvironmentName: new("invalid"),
					Type:                   types.CETypeManaged,
					ComputeResources:       &bad,
				},
			)
			code(t, e, "ClientException")
		}
		_, e = c.UpdateComputeEnvironment(
			ctx,
			&sdk.UpdateComputeEnvironmentInput{
				ComputeEnvironment: out.ComputeEnvironmentArn,
				ComputeResources:   &types.ComputeResourceUpdate{MaxvCpus: new(int32(16))},
			},
		)
		if e != nil {
			t.Fatal(e)
		}

		current, e := c.DescribeComputeEnvironments(
			ctx,
			&sdk.DescribeComputeEnvironmentsInput{ComputeEnvironments: []string{*out.ComputeEnvironmentArn}},
		)
		if e != nil {
			t.Fatal(e)
		}
		ce := current.ComputeEnvironments[0]
		r := ce.ComputeResources
		if ce.Status != types.CEStatusValid || r.Type != typ || *r.MaxvCpus != 16 || r.MinvCpus != nil ||
			r.DesiredvCpus != nil ||
			r.AllocationStrategy != "" ||
			r.InstanceTypes != nil ||
			r.InstanceRole != nil {
			t.Fatalf("Fargate capacity roundtrip: %+v", r)
		}
		for _, u := range []*types.ComputeResourceUpdate{
			{MinvCpus: new(int32(0))},
			{DesiredvCpus: new(int32(0))},
			{MaxvCpus: new(int32(0))},
		} {
			_, e = c.UpdateComputeEnvironment(
				ctx,
				&sdk.UpdateComputeEnvironmentInput{ComputeEnvironment: out.ComputeEnvironmentArn, ComputeResources: u},
			)
			code(t, e, "ClientException")
		}
	}
	order := []types.ComputeEnvironmentOrder{
		{Order: new(int32(1)), ComputeEnvironment: new(names[0])},
		{Order: new(int32(2)), ComputeEnvironment: new(names[1])},
	}
	q, err := c.CreateJobQueue(
		ctx,
		&sdk.CreateJobQueueInput{JobQueueName: new("fargate"), Priority: new(int32(1)), ComputeEnvironmentOrder: order},
	)
	if err != nil {
		t.Fatal(err)
	}
	mixed := append([]types.ComputeEnvironmentOrder{}, order...)
	mixed[1].ComputeEnvironment = ec2.ComputeEnvironmentArn
	_, err = c.CreateJobQueue(
		ctx,
		&sdk.CreateJobQueueInput{JobQueueName: new("mixed"), Priority: new(int32(1)), ComputeEnvironmentOrder: mixed},
	)
	code(t, err, "ClientException")
	_, err = c.UpdateJobQueue(ctx, &sdk.UpdateJobQueueInput{JobQueue: q.JobQueueArn, ComputeEnvironmentOrder: mixed})
	code(t, err, "ClientException")
	defs := []struct{ cpu, memory string }{
		{"0.25", "512"},
		{"0.25", "1024"},
		{"0.25", "2048"},
		{"0.5", "4096"},
		{"1", "8192"},
		{"2", "16384"},
		{"4", "30720"},
		{"8", "61440"},
		{"16", "122880"},
		{"32", "61440"},
		{"32", "122880"},
		{"32", "249856"},
	}
	for _, pair := range defs {
		d := fargateDefinition()
		d.ContainerProperties.ResourceRequirements[0].Value = new(pair.cpu)
		d.ContainerProperties.ResourceRequirements[1].Value = new(pair.memory)
		if _, err = c.RegisterJobDefinition(ctx, d); err != nil {
			t.Fatal(pair, err)
		}
	}

	for _, size := range []struct {
		cpu            string
		min, max, step int
	}{
		{"0.5", 1024, 4096, 1024},
		{"1", 2048, 8192, 1024},
		{"2", 4096, 16384, 1024},
		{"4", 8192, 30720, 1024},
		{"8", 16384, 61440, 4096},
		{"16", 32768, 122880, 8192},
	} {
		for memoryMiB := size.min; memoryMiB <= size.max; memoryMiB += size.step {
			d := fargateDefinition()
			d.ContainerProperties.ResourceRequirements[0].Value = new(size.cpu)
			d.ContainerProperties.ResourceRequirements[1].Value = new(strconv.Itoa(memoryMiB))
			if _, err = c.RegisterJobDefinition(ctx, d); err != nil {
				t.Fatalf("CPU %s memory %d: %v", size.cpu, memoryMiB, err)
			}
		}
		for _, memoryMiB := range []int{size.min - 1, size.max + 1, size.min + 1} {
			d := fargateDefinition()
			d.ContainerProperties.ResourceRequirements[0].Value = new(size.cpu)
			d.ContainerProperties.ResourceRequirements[1].Value = new(strconv.Itoa(memoryMiB))
			_, err = c.RegisterJobDefinition(ctx, d)
			code(t, err, "ClientException")
		}
	}
	for _, os := range []string{
		"LINUX", "WINDOWS_SERVER_2019_CORE", "WINDOWS_SERVER_2019_FULL",
		"WINDOWS_SERVER_2022_CORE", "WINDOWS_SERVER_2022_FULL",
	} {
		d := fargateDefinition()
		d.ContainerProperties.RuntimePlatform = &types.RuntimePlatform{
			CpuArchitecture:       new("X86_64"),
			OperatingSystemFamily: new(os),
		}
		d.ContainerProperties.ResourceRequirements[0].Value = new("1")
		d.ContainerProperties.ResourceRequirements[1].Value = new("2048")
		if os != "LINUX" {
			d.ContainerProperties.FargatePlatformConfiguration.PlatformVersion = new("1.0.0")
		} else {
			d.ContainerProperties.FargatePlatformConfiguration.PlatformVersion = new("1.3.0")
		}
		if _, err = c.RegisterJobDefinition(ctx, d); err != nil {
			t.Fatal(os, err)
		}
	}
	d := fargateDefinition()
	registered, err := c.RegisterJobDefinition(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	described, err := c.DescribeJobDefinitions(
		ctx,
		&sdk.DescribeJobDefinitionsInput{JobDefinitions: []string{*registered.JobDefinitionArn}},
	)
	if err != nil || !reflect.DeepEqual(described.JobDefinitions[0].ContainerProperties, d.ContainerProperties) {
		t.Fatalf("Fargate roundtrip: %+v %v", described, err)
	}
	for _, mutate := range []func(*types.ContainerProperties){
		func(c *types.ContainerProperties) { c.ExecutionRoleArn = nil },
		func(c *types.ContainerProperties) {
			//nolint:staticcheck // Verify that deprecated EC2-only CPU input is rejected for Fargate.
			c.Vcpus = new(int32(1))
		},
		func(c *types.ContainerProperties) { c.ResourceRequirements[0].Value = new("0.75") },
		func(c *types.ContainerProperties) { c.ResourceRequirements[1].Value = new("1536") },
		func(c *types.ContainerProperties) { c.ResourceRequirements = c.ResourceRequirements[:1] },
		func(c *types.ContainerProperties) {
			c.ResourceRequirements = append(c.ResourceRequirements, c.ResourceRequirements[0])
		},
		func(c *types.ContainerProperties) { c.NetworkConfiguration.AssignPublicIp = "invalid" },
		func(c *types.ContainerProperties) { c.FargatePlatformConfiguration.PlatformVersion = new("invalid") },
		func(c *types.ContainerProperties) { c.FargatePlatformConfiguration.PlatformVersion = new("1.3.0") },
		func(c *types.ContainerProperties) { c.FargatePlatformConfiguration.PlatformVersion = new("1.0.0") },
		func(c *types.ContainerProperties) {
			c.RuntimePlatform.CpuArchitecture = new("X86_64")
			c.FargatePlatformConfiguration.PlatformVersion = new("1.3.0")
			c.ResourceRequirements[0].Value = new("8")
			c.ResourceRequirements[1].Value = new("16384")
		},
		func(c *types.ContainerProperties) { c.RuntimePlatform.CpuArchitecture = new("invalid") },
		func(c *types.ContainerProperties) { c.RuntimePlatform.OperatingSystemFamily = new("invalid") },
		func(c *types.ContainerProperties) {
			c.RuntimePlatform.OperatingSystemFamily = new("WINDOWS_SERVER_2022_CORE")
		},
	} {
		bad := fargateDefinition()
		mutate(bad.ContainerProperties)
		_, err = c.RegisterJobDefinition(ctx, bad)
		code(t, err, "ClientException")
	}

	for _, platform := range [][]types.PlatformCapability{
		{types.PlatformCapabilityEc2},
		{types.PlatformCapabilityFargate, types.PlatformCapabilityEc2},
		{"INVALID"},
	} {
		bad := fargateDefinition()
		bad.PlatformCapabilities = platform
		_, err = c.RegisterJobDefinition(ctx, bad)
		code(t, err, "ClientException")
	}
	in := &sdk.SubmitJobInput{
		JobName:       new("fargate"),
		JobQueue:      q.JobQueueArn,
		JobDefinition: registered.JobDefinitionArn,
	}
	job, err := c.SubmitJob(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got := describeBatch(t, c, *job.JobId)
	if got.Status != types.JobStatusRunnable || got.StartedAt != nil || len(got.Attempts) != 0 ||
		!reflect.DeepEqual(got.PlatformCapabilities, d.PlatformCapabilities) ||
		got.Container.RuntimePlatform == nil ||
		got.Container.NetworkConfiguration.AssignPublicIp != types.AssignPublicIpEnabled {
		t.Fatalf("records Fargate: %+v", got)
	}
	in.DependsOn = []types.JobDependency{{JobId: job.JobId}}
	dependent, err := c.SubmitJob(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if got = describeBatch(t, c, *dependent.JobId); got.Status != types.JobStatusPending {
		t.Fatal(got.Status)
	}
	in.DependsOn = nil
	in.ContainerOverrides = &types.ContainerOverrides{
		ResourceRequirements: []types.ResourceRequirement{{Type: types.ResourceTypeMemory, Value: new("1536")}},
	}
	_, err = c.SubmitJob(ctx, in)
	code(t, err, "ClientException")
	//nolint:staticcheck // Verify deprecated CPU overrides are rejected even with resource requirements.
	in.ContainerOverrides = &types.ContainerOverrides{
		Vcpus:                new(int32(1)),
		ResourceRequirements: []types.ResourceRequirement{{Type: types.ResourceTypeVcpu, Value: new("1")}},
	}
	_, err = c.SubmitJob(ctx, in)
	code(t, err, "ClientException")
	in.ContainerOverrides = nil
	in.JobQueue = new("queue")
	_, err = c.SubmitJob(ctx, in)
	code(t, err, "ClientException")
	in.JobQueue = q.JobQueueArn
	in.JobDefinition = new("worker")
	_, err = c.SubmitJob(ctx, in)
	code(t, err, "ClientException")
}

// Attach an in-memory Runtime to exercise Docker-mode rejection without invoking Docker.
func TestBatchFargateRuntimeRejectionSDK(t *testing.T) {
	ctx := context.Background()
	store := &memory.Store{}
	network := ec2service.New(store)
	service := batchservice.New(store, network)
	rt := &containerRuntime{containers: map[string]*fakeContainer{}}
	t.Cleanup(service.Stop)
	h, err := gateway.New(
		config.Default(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		batchapi.Descriptor(service),
		ec2api.Descriptor(network),
	)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	batchFixture(t, s)
	c := batchClient(s, "test", "us-east-1")
	envs, err := c.DescribeComputeEnvironments(ctx, &sdk.DescribeComputeEnvironmentsInput{})
	if err != nil {
		t.Fatal(err)
	}
	n := envs.ComputeEnvironments[0].ComputeResources
	ce, err := c.CreateComputeEnvironment(
		ctx,
		&sdk.CreateComputeEnvironmentInput{
			ComputeEnvironmentName: new("fg"),
			Type:                   types.CETypeManaged,
			ComputeResources: &types.ComputeResource{
				Type:             types.CRTypeFargate,
				MaxvCpus:         new(int32(4)),
				Subnets:          n.Subnets,
				SecurityGroupIds: n.SecurityGroupIds,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	q, err := c.CreateJobQueue(
		ctx,
		&sdk.CreateJobQueueInput{
			JobQueueName: new("fg"),
			Priority:     new(int32(1)),
			ComputeEnvironmentOrder: []types.ComputeEnvironmentOrder{
				{Order: new(int32(1)), ComputeEnvironment: ce.ComputeEnvironmentArn},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.RegisterJobDefinition(ctx, fargateDefinition())
	if err != nil {
		t.Fatal(err)
	}
	queued, err := c.SubmitJob(
		ctx,
		&sdk.SubmitJobInput{JobName: new("queued"), JobQueue: q.JobQueueArn, JobDefinition: d.JobDefinitionArn},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.UseRuntime(ctx, rt, nil); err != nil {
		t.Fatal(err)
	}
	service.Start()
	waitBatch(t, c, *queued.JobId, types.JobStatusFailed)
	_, err = c.SubmitJob(
		ctx,
		&sdk.SubmitJobInput{JobName: new("fg"), JobQueue: q.JobQueueArn, JobDefinition: d.JobDefinitionArn},
	)
	code(t, err, "ClientException")
	if !strings.Contains(err.Error(), "Fargate execution is unsupported") {
		t.Fatal(err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.containers) != 0 {
		t.Fatal("Fargate invoked runtime")
	}
}
