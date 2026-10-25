package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol/types"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgtatypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
)

const (
	agentImage      = "public.ecr.aws/tinystack/agent:latest"
	agentToken      = "0123456789012345678901234567890123456789"
	agentTokenOther = "9876543210987654321098765432109876543210"
)

func agentCoreControl(s *httptest.Server, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{}},
	)
}

func agentCoreRole(t *testing.T, s *httptest.Server, name string) string {
	t.Helper()
	out, err := iamClient(s, "test", "us-east-1").CreateRole(context.Background(), &iam.CreateRoleInput{
		RoleName: new(name),
		AssumeRolePolicyDocument: new(
			`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
				`"Principal":{"Service":"bedrock-agentcore.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	return *out.Role.Arn
}

func container(uri string) types.AgentRuntimeArtifact {
	return &types.AgentRuntimeArtifactMemberContainerConfiguration{
		Value: types.ContainerConfiguration{ContainerUri: new(uri)},
	}
}

func agentRuntimeInput(name, role string) *sdk.CreateAgentRuntimeInput {
	return &sdk.CreateAgentRuntimeInput{
		AgentRuntimeName: new(name), AgentRuntimeArtifact: container(agentImage), RoleArn: new(role),
	}
}

func wantAgentRuntime(
	t *testing.T,
	c *sdk.Client,
	id *string,
	want types.AgentRuntimeStatus,
) *sdk.GetAgentRuntimeOutput {
	t.Helper()
	var out *sdk.GetAgentRuntimeOutput
	waitFor(t, "runtime "+*id+" to be "+string(want), func() bool {
		var err error
		out, err = c.GetAgentRuntime(context.Background(), &sdk.GetAgentRuntimeInput{AgentRuntimeId: id})
		if want == "" {
			if err == nil {
				return false
			}

			wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

			return true
		}

		if err != nil {
			t.Fatal(err)
		}

		return out.Status == want
	})

	return out
}

func wantAgentEndpoint(
	t *testing.T,
	c *sdk.Client,
	id, name *string,
	want types.AgentRuntimeEndpointStatus,
) *sdk.GetAgentRuntimeEndpointOutput {
	t.Helper()
	var out *sdk.GetAgentRuntimeEndpointOutput
	waitFor(t, "endpoint "+*name+" to be "+string(want), func() bool {
		var err error
		out, err = c.GetAgentRuntimeEndpoint(
			context.Background(), &sdk.GetAgentRuntimeEndpointInput{AgentRuntimeId: id, EndpointName: name})
		if want == "" {
			if err == nil {
				return false
			}

			wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

			return true
		}

		if err != nil {
			t.Fatal(err)
		}

		return out.Status == want
	})

	return out
}

func wantAgentMemory(t *testing.T, c *sdk.Client, id *string, want types.MemoryStatus) *types.Memory {
	t.Helper()
	var out *types.Memory
	waitFor(t, "memory "+*id+" to be "+string(want), func() bool {
		got, err := c.GetMemory(context.Background(), &sdk.GetMemoryInput{MemoryId: id})
		if want == "" {
			if err == nil {
				return false
			}

			wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

			return true
		}

		if err != nil {
			t.Fatal(err)
		}

		out = got.Memory

		return out.Status == want
	})

	return out
}

func TestAgentCoreRuntimeLifecycle(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := agentCoreControl(s, "us-east-1")
	ctx := context.Background()
	role := agentCoreRole(t, s, "agent")
	input := agentRuntimeInput("support_agent", role)
	input.ClientToken = new(agentToken)
	input.Description = new("answers tickets")
	input.EnvironmentVariables = map[string]string{"MODE": "test"}
	input.Tags = map[string]string{"team": "core"}
	input.ProtocolConfiguration = &types.ProtocolConfiguration{ServerProtocol: types.ServerProtocolHttp}
	made, err := c.CreateAgentRuntime(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	if made.Status != types.AgentRuntimeStatusCreating || aws.ToString(made.AgentRuntimeVersion) != "1" ||
		*made.AgentRuntimeArn != "arn:aws:bedrock-agentcore:us-east-1:000000000000:runtime/"+*made.AgentRuntimeId {
		t.Fatalf("create: %+v", made)
	}

	again, err := c.CreateAgentRuntime(ctx, input)
	if err != nil || *again.AgentRuntimeId != *made.AgentRuntimeId {
		t.Fatalf("replay: %+v %v", again, err)
	}

	other := agentRuntimeInput("support_agent", role)
	other.ClientToken = new(agentToken)
	_, err = c.CreateAgentRuntime(ctx, other)
	wantAccessAnalyzerError(t, err, "ConflictException")
	other.ClientToken = new(agentTokenOther)
	_, err = c.CreateAgentRuntime(ctx, other)
	wantAccessAnalyzerError(t, err, "ConflictException")

	live := wantAgentRuntime(t, c, made.AgentRuntimeId, types.AgentRuntimeStatusReady)
	if aws.ToString(live.Description) != "answers tickets" || live.EnvironmentVariables["MODE"] != "test" ||
		*live.AgentRuntimeName != "support_agent" || *live.RoleArn != role ||
		aws.ToInt32(live.LifecycleConfiguration.IdleRuntimeSessionTimeout) != 900 ||
		aws.ToInt32(live.LifecycleConfiguration.MaxLifetime) != 28800 ||
		live.ProtocolConfiguration.ServerProtocol != types.ServerProtocolHttp {
		t.Fatalf("get: %+v", live)
	}

	endpoint := wantAgentEndpoint(t, c, made.AgentRuntimeId, new("DEFAULT"), types.AgentRuntimeEndpointStatusReady)
	if aws.ToString(endpoint.LiveVersion) != "1" || aws.ToString(endpoint.TargetVersion) != "1" {
		t.Fatalf("default endpoint: %+v", endpoint)
	}

	// Update creates version 2, takes the runtime through UPDATING and moves the DEFAULT endpoint.
	update := &sdk.UpdateAgentRuntimeInput{
		AgentRuntimeId: made.AgentRuntimeId, AgentRuntimeArtifact: container(agentImage + ".v2"), RoleArn: new(role),
		LifecycleConfiguration: &types.LifecycleConfiguration{
			IdleRuntimeSessionTimeout: new(int32(120)), MaxLifetime: new(int32(3600)),
		},
	}

	updated, err := c.UpdateAgentRuntime(ctx, update)
	if err != nil || updated.Status != types.AgentRuntimeStatusUpdating ||
		aws.ToString(updated.AgentRuntimeVersion) != "2" {
		t.Fatalf("update: %+v %v", updated, err)
	}

	_, err = c.UpdateAgentRuntime(ctx, update)
	wantAccessAnalyzerError(t, err, "ConflictException")
	moving, err := c.GetAgentRuntimeEndpoint(ctx, &sdk.GetAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, EndpointName: new("DEFAULT"),
	})
	if err != nil || moving.Status != types.AgentRuntimeEndpointStatusUpdating ||
		aws.ToString(moving.LiveVersion) != "1" || aws.ToString(moving.TargetVersion) != "2" {
		t.Fatalf("moving endpoint: %+v %v", moving, err)
	}

	latest := wantAgentRuntime(t, c, made.AgentRuntimeId, types.AgentRuntimeStatusReady)
	if aws.ToString(latest.AgentRuntimeVersion) != "2" || aws.ToString(latest.Description) != "" ||
		len(latest.EnvironmentVariables) != 0 ||
		aws.ToInt32(latest.LifecycleConfiguration.IdleRuntimeSessionTimeout) != 120 {
		t.Fatalf("update replaces the configuration: %+v", latest)
	}

	first, err := c.GetAgentRuntime(ctx, &sdk.GetAgentRuntimeInput{
		AgentRuntimeId: made.AgentRuntimeId, AgentRuntimeVersion: new("1"),
	})
	if err != nil || aws.ToString(first.Description) != "answers tickets" ||
		first.Status != types.AgentRuntimeStatusReady {
		t.Fatalf("version 1: %+v %v", first, err)
	}

	moved := wantAgentEndpoint(t, c, made.AgentRuntimeId, new("DEFAULT"), types.AgentRuntimeEndpointStatusReady)
	if aws.ToString(moved.LiveVersion) != "2" {
		t.Fatalf("default endpoint follows the latest version: %+v", moved)
	}

	versions, err := c.ListAgentRuntimeVersions(ctx, &sdk.ListAgentRuntimeVersionsInput{
		AgentRuntimeId: made.AgentRuntimeId, MaxResults: new(int32(1)),
	})
	if err != nil || len(versions.AgentRuntimes) != 1 || *versions.AgentRuntimes[0].AgentRuntimeVersion != "1" ||
		versions.NextToken == nil {
		t.Fatalf("versions page: %+v %v", versions, err)
	}

	rest, err := c.ListAgentRuntimeVersions(ctx, &sdk.ListAgentRuntimeVersionsInput{
		AgentRuntimeId: made.AgentRuntimeId, NextToken: versions.NextToken,
	})
	if err != nil || len(rest.AgentRuntimes) != 1 || *rest.AgentRuntimes[0].AgentRuntimeVersion != "2" ||
		rest.NextToken != nil {
		t.Fatalf("versions rest: %+v %v", rest, err)
	}

	// A named endpoint serves the version it was given and moves only when told to.
	prod, err := c.CreateAgentRuntimeEndpoint(ctx, &sdk.CreateAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, Name: new("prod"), AgentRuntimeVersion: new("1"),
		Description: new("stable"), Tags: map[string]string{"stage": "prod"},
	})
	if err != nil || prod.Status != types.AgentRuntimeEndpointStatusCreating ||
		aws.ToString(prod.TargetVersion) != "1" ||
		*prod.AgentRuntimeEndpointArn != *made.AgentRuntimeArn+"/runtime-endpoint/prod" {
		t.Fatalf("create endpoint: %+v %v", prod, err)
	}

	_, err = c.CreateAgentRuntimeEndpoint(ctx, &sdk.CreateAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, Name: new("prod"),
	})
	wantAccessAnalyzerError(t, err, "ConflictException")
	_, err = c.CreateAgentRuntimeEndpoint(ctx, &sdk.CreateAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, Name: new("DEFAULT"),
	})
	wantAccessAnalyzerError(t, err, "ConflictException")
	_, err = c.CreateAgentRuntimeEndpoint(ctx, &sdk.CreateAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, Name: new("future"), AgentRuntimeVersion: new("9"),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	ready := wantAgentEndpoint(t, c, made.AgentRuntimeId, new("prod"), types.AgentRuntimeEndpointStatusReady)
	if aws.ToString(ready.LiveVersion) != "1" || aws.ToString(ready.Description) != "stable" {
		t.Fatalf("endpoint: %+v", ready)
	}

	// Version 1 is in use by prod; version 2 by DEFAULT.
	_, err = c.DeleteAgentRuntime(ctx, &sdk.DeleteAgentRuntimeInput{
		AgentRuntimeId: made.AgentRuntimeId, AgentRuntimeVersion: new("1"),
	})
	wantAccessAnalyzerError(t, err, "ConflictException")
	moving2, err := c.UpdateAgentRuntimeEndpoint(ctx, &sdk.UpdateAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, EndpointName: new("prod"), AgentRuntimeVersion: new("2"),
		Description: new("promoted"),
	})
	if err != nil || moving2.Status != types.AgentRuntimeEndpointStatusUpdating ||
		aws.ToString(moving2.LiveVersion) != "1" || aws.ToString(moving2.TargetVersion) != "2" {
		t.Fatalf("update endpoint: %+v %v", moving2, err)
	}

	_, err = c.UpdateAgentRuntimeEndpoint(ctx, &sdk.UpdateAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, EndpointName: new("prod"), Description: new("again"),
	})
	wantAccessAnalyzerError(t, err, "ConflictException")
	promoted := wantAgentEndpoint(t, c, made.AgentRuntimeId, new("prod"), types.AgentRuntimeEndpointStatusReady)
	if aws.ToString(promoted.LiveVersion) != "2" || aws.ToString(promoted.Description) != "promoted" {
		t.Fatalf("promoted: %+v", promoted)
	}

	_, err = c.UpdateAgentRuntimeEndpoint(ctx, &sdk.UpdateAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, EndpointName: new("DEFAULT"), AgentRuntimeVersion: new("1"),
	})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.DeleteAgentRuntimeEndpoint(ctx, &sdk.DeleteAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, EndpointName: new("DEFAULT"),
	})
	wantAccessAnalyzerError(t, err, "ValidationException")

	endpoints, err := c.ListAgentRuntimeEndpoints(ctx, &sdk.ListAgentRuntimeEndpointsInput{
		AgentRuntimeId: made.AgentRuntimeId, MaxResults: new(int32(1)),
	})
	if err != nil || len(endpoints.RuntimeEndpoints) != 1 || *endpoints.RuntimeEndpoints[0].Name != "DEFAULT" ||
		endpoints.NextToken == nil {
		t.Fatalf("endpoints page: %+v %v", endpoints, err)
	}

	endpoints, err = c.ListAgentRuntimeEndpoints(ctx, &sdk.ListAgentRuntimeEndpointsInput{
		AgentRuntimeId: made.AgentRuntimeId, NextToken: endpoints.NextToken,
	})
	if err != nil || len(endpoints.RuntimeEndpoints) != 1 || *endpoints.RuntimeEndpoints[0].Name != "prod" {
		t.Fatalf("endpoints rest: %+v %v", endpoints, err)
	}

	// Tags live on the runtime and on the endpoint.
	for _, arn := range []*string{made.AgentRuntimeArn, prod.AgentRuntimeEndpointArn} {
		if _, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: arn, Tags: map[string]string{"extra": "yes"}},
		); err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		if e != nil || tags.Tags["extra"] != "yes" {
			t.Fatalf("tags of %s: %+v %v", *arn, tags, e)
		}

		if _, err = c.UntagResource(
			ctx,
			&sdk.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"extra"}},
		); err != nil {
			t.Fatal(err)
		}
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: made.AgentRuntimeArn})
	if err != nil || tags.Tags["team"] != "core" || len(tags.Tags) != 1 {
		t.Fatalf("runtime tags: %+v %v", tags, err)
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: new("arn:aws:bedrock-agentcore:us-east-1:000000000000:runtime/missing-0123456789"),
		Tags:        map[string]string{"a": "b"},
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: made.AgentRuntimeArn, Tags: map[string]string{"aws:x": "b"}},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")

	// The tagging API lists and tags the runtime, its endpoint and a memory.
	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	found, err := tagging.GetResources(ctx, &rgta.GetResourcesInput{
		TagFilters: []rgtatypes.TagFilter{{Key: new("stage"), Values: []string{"prod"}}},
	})
	if err != nil || len(found.ResourceTagMappingList) != 1 ||
		*found.ResourceTagMappingList[0].ResourceARN != *prod.AgentRuntimeEndpointArn {
		t.Fatalf("tagging: %+v %v", found, err)
	}

	// Deleting a version needs it unused; version 1 is free now.
	gone, err := c.DeleteAgentRuntime(ctx, &sdk.DeleteAgentRuntimeInput{
		AgentRuntimeId: made.AgentRuntimeId, AgentRuntimeVersion: new("1"),
	})
	if err != nil || gone.Status != types.AgentRuntimeStatusDeleting {
		t.Fatalf("delete version: %+v %v", gone, err)
	}

	_, err = c.GetAgentRuntime(
		ctx,
		&sdk.GetAgentRuntimeInput{AgentRuntimeId: made.AgentRuntimeId, AgentRuntimeVersion: new("1")},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	deleted, err := c.DeleteAgentRuntimeEndpoint(ctx, &sdk.DeleteAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, EndpointName: new("prod"),
	})
	if err != nil || deleted.Status != types.AgentRuntimeEndpointStatusDeleting {
		t.Fatalf("delete endpoint: %+v %v", deleted, err)
	}

	wantAgentEndpoint(t, c, made.AgentRuntimeId, new("prod"), "")
	removing, err := c.DeleteAgentRuntime(ctx, &sdk.DeleteAgentRuntimeInput{AgentRuntimeId: made.AgentRuntimeId})
	if err != nil || removing.Status != types.AgentRuntimeStatusDeleting {
		t.Fatalf("delete: %+v %v", removing, err)
	}

	wantAgentRuntime(t, c, made.AgentRuntimeId, "")
	_, err = c.GetAgentRuntimeEndpoint(ctx, &sdk.GetAgentRuntimeEndpointInput{
		AgentRuntimeId: made.AgentRuntimeId, EndpointName: new("DEFAULT"),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	listed, err := c.ListAgentRuntimes(ctx, &sdk.ListAgentRuntimesInput{})
	if err != nil || len(listed.AgentRuntimes) != 0 {
		t.Fatalf("list after delete: %+v %v", listed, err)
	}
}

func TestAgentCoreRuntimeValidation(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := agentCoreControl(s, "us-east-1")
	ctx := context.Background()
	role := agentCoreRole(t, s, "agent")
	network := ec2Client(s, "test", "us-east-1")
	vpc := createVPC(t, network, "10.50.0.0/16")
	subnet, err := network.CreateSubnet(ctx, &ec2sdk.CreateSubnetInput{
		VpcId: new(vpc), CidrBlock: new("10.50.1.0/24"), AvailabilityZone: new("us-east-1a"),
	})
	if err != nil {
		t.Fatal(err)
	}

	group, err := network.CreateSecurityGroup(ctx, &ec2sdk.CreateSecurityGroupInput{
		VpcId: new(vpc), GroupName: new("agents"), Description: new("agents"),
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*sdk.CreateAgentRuntimeInput){
		"model: name pattern": func(in *sdk.CreateAgentRuntimeInput) { in.AgentRuntimeName = new("not-valid") },
		"model: container uri": func(in *sdk.CreateAgentRuntimeInput) {
			in.AgentRuntimeArtifact = container("docker.io/library/nginx")
		},
		"model: idle range": func(in *sdk.CreateAgentRuntimeInput) {
			in.LifecycleConfiguration = &types.LifecycleConfiguration{IdleRuntimeSessionTimeout: new(int32(5))}
		},
		"role: missing": func(in *sdk.CreateAgentRuntimeInput) { in.RoleArn = new("arn:aws:iam::000000000000:role/missing") },
		"idle above lifetime": func(in *sdk.CreateAgentRuntimeInput) {
			in.LifecycleConfiguration = &types.LifecycleConfiguration{
				IdleRuntimeSessionTimeout: new(int32(900)), MaxLifetime: new(int32(300)),
			}
		},
		"code artifact": func(in *sdk.CreateAgentRuntimeInput) { in.AgentRuntimeArtifact = codeArtifact() },
		"agui protocol": func(in *sdk.CreateAgentRuntimeInput) {
			in.ProtocolConfiguration = &types.ProtocolConfiguration{ServerProtocol: types.ServerProtocolAgui}
		},
		"public with vpc config": func(in *sdk.CreateAgentRuntimeInput) {
			in.NetworkConfiguration = networkConfiguration(
				types.NetworkModePublic, []string{*subnet.Subnet.SubnetId}, []string{*group.GroupId},
			)
		},
		"vpc without config": func(in *sdk.CreateAgentRuntimeInput) {
			in.NetworkConfiguration = &types.NetworkConfiguration{NetworkMode: types.NetworkModeVpc}
		},
		"vpc subnet missing": func(in *sdk.CreateAgentRuntimeInput) {
			in.NetworkConfiguration = networkConfiguration(
				types.NetworkModeVpc, []string{"subnet-0123456789abcdef0"}, []string{*group.GroupId},
			)
		},
		"vpc group missing": func(in *sdk.CreateAgentRuntimeInput) {
			in.NetworkConfiguration = networkConfiguration(
				types.NetworkModeVpc, []string{*subnet.Subnet.SubnetId}, []string{"sg-0123456789abcdef0"},
			)
		},
		"reserved tag": func(in *sdk.CreateAgentRuntimeInput) { in.Tags = map[string]string{"aws:team": "x"} },
	} {
		input := agentRuntimeInput("checked", role)
		mutate(input)
		_, err = c.CreateAgentRuntime(ctx, input)
		want := "ValidationException"
		if name == "role: missing" {
			want = "ResourceNotFoundException"
		}

		if err == nil {
			t.Errorf("%s: accepted", name)

			continue
		}

		wantAccessAnalyzerError(t, err, want)
	}

	// A VPC runtime whose subnets and groups exist is stored and described.
	vpcInput := agentRuntimeInput("private_agent", role)
	vpcInput.NetworkConfiguration = &types.NetworkConfiguration{
		NetworkMode: types.NetworkModeVpc,
		NetworkModeConfig: &types.VpcConfig{
			Subnets: []string{*subnet.Subnet.SubnetId}, SecurityGroups: []string{*group.GroupId},
		},
	}

	vpcInput.AuthorizerConfiguration = &types.AuthorizerConfigurationMemberCustomJWTAuthorizer{
		Value: types.CustomJWTAuthorizerConfiguration{
			DiscoveryUrl:    new("https://idp.example.com/.well-known/openid-configuration"),
			AllowedAudience: []string{"agents"},
		},
	}

	vpcInput.RequestHeaderConfiguration = &types.RequestHeaderConfigurationMemberRequestHeaderAllowlist{
		Value: []string{"X-Team"},
	}

	made, err := c.CreateAgentRuntime(ctx, vpcInput)
	if err != nil {
		t.Fatal(err)
	}

	got := wantAgentRuntime(t, c, made.AgentRuntimeId, types.AgentRuntimeStatusReady)
	jwt, ok := got.AuthorizerConfiguration.(*types.AuthorizerConfigurationMemberCustomJWTAuthorizer)
	headers, headerOK := got.RequestHeaderConfiguration.(*types.RequestHeaderConfigurationMemberRequestHeaderAllowlist)
	if got.NetworkConfiguration.NetworkMode != types.NetworkModeVpc ||
		got.NetworkConfiguration.NetworkModeConfig.Subnets[0] != *subnet.Subnet.SubnetId ||
		!ok || jwt.Value.AllowedAudience[0] != "agents" || !headerOK || headers.Value[0] != "X-Team" {
		t.Fatalf("vpc runtime: %+v", got)
	}

	// Members TinyStack does not implement fail instead of being ignored.
	unsupported := agentRuntimeInput("platform_agent", role)
	unsupported.PlatformVersion = new("1")
	_, err = c.CreateAgentRuntime(ctx, unsupported)
	wantAccessAnalyzerError(t, err, "NotImplemented")

	_, err = c.GetAgentRuntime(ctx, &sdk.GetAgentRuntimeInput{AgentRuntimeId: new("missing-0123456789")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.GetAgentRuntime(ctx, &sdk.GetAgentRuntimeInput{AgentRuntimeId: new("bad id")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.GetAgentRuntime(
		ctx,
		&sdk.GetAgentRuntimeInput{AgentRuntimeId: made.AgentRuntimeId, AgentRuntimeVersion: new("7")},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.UpdateAgentRuntime(ctx, &sdk.UpdateAgentRuntimeInput{
		AgentRuntimeId: new("missing-0123456789"), AgentRuntimeArtifact: container(agentImage), RoleArn: new(role),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.DeleteAgentRuntime(ctx, &sdk.DeleteAgentRuntimeInput{AgentRuntimeId: new("missing-0123456789")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.ListAgentRuntimeEndpoints(
		ctx,
		&sdk.ListAgentRuntimeEndpointsInput{AgentRuntimeId: new("missing-0123456789")},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.ListAgentRuntimes(ctx, &sdk.ListAgentRuntimesInput{NextToken: new("!")})
	wantAccessAnalyzerError(t, err, "ValidationException")
}

func TestAgentCoreRuntimePagination(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := agentCoreControl(s, "us-east-1")
	ctx := context.Background()
	role := agentCoreRole(t, s, "agent")
	var ids []string
	for i := range 3 {
		made, err := c.CreateAgentRuntime(ctx, agentRuntimeInput("agent_"+strconv.Itoa(i), role))
		if err != nil {
			t.Fatal(err)
		}

		ids = append(ids, *made.AgentRuntimeId)
	}

	var listed []string
	var token *string
	for pages := 0; ; pages++ {
		page, err := c.ListAgentRuntimes(ctx, &sdk.ListAgentRuntimesInput{MaxResults: new(int32(2)), NextToken: token})
		if err != nil {
			t.Fatal(err)
		}

		if len(page.AgentRuntimes) > 2 || pages > 2 {
			t.Fatalf("page %d: %+v", pages, page)
		}

		for _, r := range page.AgentRuntimes {
			listed = append(listed, *r.AgentRuntimeId)
		}

		if token = page.NextToken; token == nil {
			break
		}
	}

	if len(listed) != 3 || listed[0] != ids[0] || listed[1] != ids[1] || listed[2] != ids[2] {
		t.Fatalf("listed %v, created %v", listed, ids)
	}
}

func TestAgentCoreMemoryLifecycle(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := agentCoreControl(s, "us-east-1")
	ctx := context.Background()
	role := agentCoreRole(t, s, "memory")
	key, err := kmsClient(s, "test", "us-east-1").CreateKey(ctx, &kms.CreateKeyInput{})
	if err != nil {
		t.Fatal(err)
	}

	input := &sdk.CreateMemoryInput{
		Name: new("chat_memory"), EventExpiryDuration: new(int32(30)), ClientToken: new("memory-token"),
		Description: new("chats"), MemoryExecutionRoleArn: new(role), EncryptionKeyArn: key.KeyMetadata.Arn,
		Tags: map[string]string{"team": "core"},
	}

	made, err := c.CreateMemory(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	m := made.Memory
	arn := "arn:aws:bedrock-agentcore:us-east-1:000000000000:memory/" + *m.Id
	if m.Status != types.MemoryStatusCreating || *m.Arn != arn || aws.ToInt32(m.EventExpiryDuration) != 30 ||
		*m.MemoryExecutionRoleArn != role || len(m.Strategies) != 0 {
		t.Fatalf("create: %+v", m)
	}

	again, err := c.CreateMemory(ctx, input)
	if err != nil || *again.Memory.Id != *m.Id {
		t.Fatalf("replay: %+v %v", again, err)
	}

	_, err = c.CreateMemory(ctx, &sdk.CreateMemoryInput{
		Name: new("chat_memory"), EventExpiryDuration: new(int32(7)), ClientToken: new("memory-token"),
	})
	wantAccessAnalyzerError(t, err, "ConflictException")
	_, err = c.CreateMemory(ctx, &sdk.CreateMemoryInput{Name: new("chat_memory"), EventExpiryDuration: new(int32(7))})
	wantAccessAnalyzerError(t, err, "ConflictException")
	live := wantAgentMemory(t, c, m.Id, types.MemoryStatusActive)
	if aws.ToString(live.Description) != "chats" || aws.ToString(live.EncryptionKeyArn) != *key.KeyMetadata.Arn {
		t.Fatalf("get: %+v", live)
	}

	for name, in := range map[string]*sdk.CreateMemoryInput{
		"model: expiry range": {Name: new("short"), EventExpiryDuration: new(int32(1))},
		"model: name pattern": {Name: new("bad name"), EventExpiryDuration: new(int32(7))},
		"strategies": {
			Name: new("strategic"), EventExpiryDuration: new(int32(7)),
			MemoryStrategies: []types.MemoryStrategyInput{&types.MemoryStrategyInputMemberSummaryMemoryStrategy{
				Value: types.SummaryMemoryStrategyInput{Name: new("summaries")},
			}},
		},
		"role missing": {
			Name: new("norole"), EventExpiryDuration: new(int32(7)),
			MemoryExecutionRoleArn: new("arn:aws:iam::000000000000:role/missing"),
		},
		"key missing": {
			Name: new("nokey"), EventExpiryDuration: new(int32(7)),
			EncryptionKeyArn: new("arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000"),
		},
	} {
		_, err = c.CreateMemory(ctx, in)
		want := "ValidationException"
		if name == "role missing" || name == "key missing" {
			want = "ResourceNotFoundException"
		}

		if err == nil {
			t.Errorf("%s: accepted", name)

			continue
		}

		wantAccessAnalyzerError(t, err, want)
	}

	updated, err := c.UpdateMemory(ctx, &sdk.UpdateMemoryInput{
		MemoryId: m.Id, EventExpiryDuration: new(int32(60)), Description: new("all chats"),
	})
	if err != nil || updated.Memory.Status != types.MemoryStatusUpdating ||
		aws.ToInt32(updated.Memory.EventExpiryDuration) != 60 {
		t.Fatalf("update: %+v %v", updated, err)
	}

	_, err = c.UpdateMemory(ctx, &sdk.UpdateMemoryInput{MemoryId: m.Id, EventExpiryDuration: new(int32(61))})
	wantAccessAnalyzerError(t, err, "ConflictException")
	live = wantAgentMemory(t, c, m.Id, types.MemoryStatusActive)
	if aws.ToString(live.Description) != "all chats" || aws.ToInt32(live.EventExpiryDuration) != 60 {
		t.Fatalf("updated: %+v", live)
	}

	_, err = c.UpdateMemory(ctx, &sdk.UpdateMemoryInput{
		MemoryId: m.Id,
		MemoryStrategies: &types.ModifyMemoryStrategies{
			DeleteMemoryStrategies: []types.DeleteMemoryStrategyInput{{MemoryStrategyId: new("any")}},
		},
	})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.GetMemory(ctx, &sdk.GetMemoryInput{MemoryId: new("missing-0123456789")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	second, err := c.CreateMemory(ctx, &sdk.CreateMemoryInput{Name: new("second"), EventExpiryDuration: new(int32(7))})
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.ListMemories(ctx, &sdk.ListMemoriesInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Memories) != 1 || page.NextToken == nil || *page.Memories[0].Id != *m.Id {
		t.Fatalf("page: %+v %v", page, err)
	}

	page, err = c.ListMemories(ctx, &sdk.ListMemoriesInput{NextToken: page.NextToken})
	if err != nil || len(page.Memories) != 1 || *page.Memories[0].Id != *second.Memory.Id || page.NextToken != nil {
		t.Fatalf("rest: %+v %v", page, err)
	}

	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: m.Arn, Tags: map[string]string{"extra": "yes"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: m.Arn})
	if err != nil || tags.Tags["team"] != "core" || tags.Tags["extra"] != "yes" {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	if _, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: m.Arn, TagKeys: []string{"team"}},
	); err != nil {
		t.Fatal(err)
	}

	removed, err := c.DeleteMemory(ctx, &sdk.DeleteMemoryInput{MemoryId: m.Id})
	if err != nil || removed.Status != types.MemoryStatusDeleting {
		t.Fatalf("delete: %+v %v", removed, err)
	}

	wantAgentMemory(t, c, m.Id, "")
	_, err = c.DeleteMemory(ctx, &sdk.DeleteMemoryInput{MemoryId: m.Id})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func TestAgentCoreScopes(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	role := agentCoreRole(t, s, "agent")
	east := agentCoreControl(s, "us-east-1")
	west := agentCoreControl(s, "us-west-2")
	made, err := east.CreateAgentRuntime(ctx, agentRuntimeInput("scoped", role))
	if err != nil {
		t.Fatal(err)
	}

	_, err = west.GetAgentRuntime(ctx, &sdk.GetAgentRuntimeInput{AgentRuntimeId: made.AgentRuntimeId})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = west.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: made.AgentRuntimeArn, Tags: map[string]string{"a": "b"}},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	listed, err := west.ListAgentRuntimes(ctx, &sdk.ListAgentRuntimesInput{})
	if err != nil || len(listed.AgentRuntimes) != 0 {
		t.Fatalf("west: %+v %v", listed, err)
	}
}

func TestAgentCoreSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := agentCoreControl(s, "us-east-1")
	role := agentCoreRole(t, s, "agent")
	input := agentRuntimeInput("durable", role)
	input.ClientToken = new(agentToken)
	input.Tags = map[string]string{"restart": "yes"}
	made, err := c.CreateAgentRuntime(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	memory, err := c.CreateMemory(
		ctx,
		&sdk.CreateMemoryInput{Name: new("durable_chat"), EventExpiryDuration: new(int32(7))},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Stop while both are still settling: the restart finishes them.
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = agentCoreControl(s, "us-east-1")
	live := wantAgentRuntime(t, c, made.AgentRuntimeId, types.AgentRuntimeStatusReady)
	if *live.AgentRuntimeName != "durable" {
		t.Fatalf("restarted runtime: %+v", live)
	}

	wantAgentEndpoint(t, c, made.AgentRuntimeId, new("DEFAULT"), types.AgentRuntimeEndpointStatusReady)
	wantAgentMemory(t, c, memory.Memory.Id, types.MemoryStatusActive)
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: made.AgentRuntimeArn})
	if err != nil || tags.Tags["restart"] != "yes" {
		t.Fatalf("restarted tags: %+v %v", tags, err)
	}

	replay, err := c.CreateAgentRuntime(ctx, input)
	if err != nil || *replay.AgentRuntimeId != *made.AgentRuntimeId {
		t.Fatalf("persistent token: %+v %v", replay, err)
	}

	if _, err = c.DeleteAgentRuntime(
		ctx,
		&sdk.DeleteAgentRuntimeInput{AgentRuntimeId: made.AgentRuntimeId},
	); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()

	wantAgentRuntime(t, agentCoreControl(s, "us-east-1"), made.AgentRuntimeId, "")
}

func TestAgentCoreCloudFormation(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	role := agentCoreRole(t, s, "agent")
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := func(runtimeName, team, description, endpointVersion string) *string {
		doc := map[string]any{
			"Resources": map[string]any{
				"Runtime": map[string]any{"Type": "AWS::BedrockAgentCore::Runtime", "Properties": map[string]any{
					"AgentRuntimeName": runtimeName,
					"AgentRuntimeArtifact": map[string]any{
						"ContainerConfiguration": map[string]any{"ContainerUri": agentImage},
					},
					"RoleArn":               role,
					"Description":           description,
					"ProtocolConfiguration": "HTTP",
					"EnvironmentVariables":  map[string]string{"MODE": "stack"},
					"Tags":                  map[string]string{"team": team},
				}},
				"Endpoint": map[string]any{
					"Type": "AWS::BedrockAgentCore::RuntimeEndpoint",
					"Properties": map[string]any{
						"AgentRuntimeId":      map[string]any{"Fn::GetAtt": []string{"Runtime", "AgentRuntimeId"}},
						"Name":                "prod",
						"AgentRuntimeVersion": endpointVersion,
						"Tags":                map[string]string{"team": team},
					},
				},
				"Memory": map[string]any{"Type": "AWS::BedrockAgentCore::Memory", "Properties": map[string]any{
					"Name":                "stack_memory",
					"EventExpiryDuration": 30,
					"Description":         description,
					"Tags":                map[string]string{"team": team},
				}},
			},
			"Outputs": map[string]any{
				"RuntimeArn": map[string]any{"Value": map[string]string{"Ref": "Runtime"}},
				"RuntimeId": map[string]any{
					"Value": map[string]any{"Fn::GetAtt": []string{"Runtime", "AgentRuntimeId"}},
				},
				"Version": map[string]any{
					"Value": map[string]any{"Fn::GetAtt": []string{"Runtime", "AgentRuntimeVersion"}},
				},
				"EndpointArn": map[string]any{"Value": map[string]string{"Ref": "Endpoint"}},
				"Live": map[string]any{
					"Value": map[string]any{"Fn::GetAtt": []string{"Endpoint", "LiveVersion"}},
				},
				"MemoryArn": map[string]any{"Value": map[string]string{"Ref": "Memory"}},
				"MemoryId":  map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"Memory", "MemoryId"}}},
			},
		}

		data, _ := json.Marshal(doc)

		return new(string(data))
	}

	if _, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("agents"), TemplateBody: template("stack_agent", "core", "first", "1"),
	}); err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, c, "agents", cfntypes.StackStatusCreateComplete)
	out := outputsOf(new(stack))
	if out["RuntimeId"] == "" || out["Version"] != "1" || out["Live"] != "1" ||
		out["RuntimeArn"] != "arn:aws:bedrock-agentcore:us-east-1:000000000000:runtime/"+out["RuntimeId"] ||
		out["EndpointArn"] != out["RuntimeArn"]+"/runtime-endpoint/prod" {
		t.Fatalf("stack: %v %s", out, aws.ToString(stack.StackStatusReason))
	}

	control := agentCoreControl(s, "us-east-1")
	live := wantAgentRuntime(t, control, new(out["RuntimeId"]), types.AgentRuntimeStatusReady)
	if aws.ToString(live.Description) != "first" || live.EnvironmentVariables["MODE"] != "stack" ||
		live.ProtocolConfiguration.ServerProtocol != types.ServerProtocolHttp {
		t.Fatalf("runtime: %+v", live)
	}

	// A description change updates in place and makes version 2; the endpoint keeps its version until it is
	// told otherwise, and tags move with the stack.
	if _, err := c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: new("agents"), TemplateBody: template("stack_agent", "platform", "second", "1"),
	}); err != nil {
		t.Fatal(err)
	}

	updated := outputsOf(new(wantStatus(ctx, t, c, "agents", cfntypes.StackStatusUpdateComplete)))
	if updated["RuntimeId"] != out["RuntimeId"] || updated["Version"] != "2" || updated["Live"] != "1" ||
		updated["MemoryId"] != out["MemoryId"] {
		t.Fatalf("in-place update: %v", updated)
	}

	tags, err := control.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(out["EndpointArn"])})
	if err != nil || tags.Tags["team"] != "platform" {
		t.Fatalf("endpoint tags: %+v %v", tags, err)
	}

	memory := wantAgentMemory(t, control, new(out["MemoryId"]), types.MemoryStatusActive)
	if aws.ToString(memory.Description) != "second" {
		t.Fatalf("memory: %+v", memory)
	}

	// Changing the immutable name replaces the runtime and with it the endpoint that depends on it.
	if _, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: new("agents"), TemplateBody: template("renamed_agent", "platform", "second", "1"),
	}); err != nil {
		t.Fatal(err)
	}

	renamed := outputsOf(new(wantStatus(ctx, t, c, "agents", cfntypes.StackStatusUpdateComplete)))
	if renamed["RuntimeId"] == out["RuntimeId"] || renamed["Version"] != "1" {
		t.Fatalf("replacement: %v", renamed)
	}

	wantAgentRuntime(t, control, new(out["RuntimeId"]), "")
	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("agents")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack deletion", func() bool {
		_, e := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("agents")})

		return e != nil
	})
	wantAgentRuntime(t, control, new(renamed["RuntimeId"]), "")
	wantAgentMemory(t, control, new(renamed["MemoryId"]), "")
}

func networkConfiguration(mode types.NetworkMode, subnets, groups []string) *types.NetworkConfiguration {
	return &types.NetworkConfiguration{
		NetworkMode:       mode,
		NetworkModeConfig: &types.VpcConfig{Subnets: subnets, SecurityGroups: groups},
	}
}

func codeArtifact() types.AgentRuntimeArtifact {
	return &types.AgentRuntimeArtifactMemberCodeConfiguration{Value: types.CodeConfiguration{
		EntryPoint: []string{"main.py"}, Runtime: types.AgentManagedRuntimeTypePython312,
		Code: &types.CodeMemberS3{Value: types.S3Location{Bucket: new("agents"), Prefix: new("agent.zip")}},
	}}
}
