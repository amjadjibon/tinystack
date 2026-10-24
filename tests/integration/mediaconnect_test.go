package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	sdk "github.com/aws/aws-sdk-go-v2/service/mediaconnect"
	"github.com/aws/aws-sdk-go-v2/service/mediaconnect/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func connectClient(s *httptest.Server, account string) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds(account),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func connectCreate(t *testing.T, c *sdk.Client, name string) *types.Flow {
	t.Helper()
	out, err := c.CreateFlow(context.Background(), &sdk.CreateFlowInput{
		Name: new(name),
		Source: &types.SetSourceRequest{
			Name: new("primary"), Protocol: types.ProtocolRtp, IngestPort: new(int32(5000)),
			WhitelistCidr: new("10.0.0.0/8"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.Flow
}

func connectWait(t *testing.T, c *sdk.Client, arn *string, want types.Status) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := c.DescribeFlow(context.Background(), &sdk.DescribeFlowInput{FlowArn: arn})
		if err != nil {
			t.Fatal(err)
		}

		if out.Flow.Status == want {
			return
		}

		select {
		case <-deadline.C:
			t.Fatalf("flow status %s, want %s", out.Flow.Status, want)
		case <-ticker.C:
		}
	}
}

func TestMediaConnectLifecycle(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := connectClient(s, "test")
	ctx := context.Background()
	flow := connectCreate(t, c, "lifecycle")
	if _, checkErr := c.UpdateFlow(ctx, &sdk.UpdateFlowInput{
		FlowArn: flow.FlowArn,
		SourceFailoverConfig: &types.UpdateFailoverConfig{
			State:        types.StateEnabled,
			FailoverMode: types.FailoverModeFailover,
		},
	}); checkErr != nil {
		t.Fatal(checkErr)
	}

	connectWait(t, c, flow.FlowArn, types.StatusStandby)
	added, err := c.AddFlowSources(
		ctx,
		&sdk.AddFlowSourcesInput{FlowArn: flow.FlowArn, Sources: []types.SetSourceRequest{
			{
				Name:          new("backup"),
				Protocol:      types.ProtocolRtp,
				IngestPort:    new(int32(5002)),
				WhitelistCidr: new("10.0.0.0/8"),
			},
		}},
	)
	if err != nil || len(added.Sources) != 1 {
		t.Fatalf("add sources: %+v %v", added, err)
	}

	source, err := c.UpdateFlowSource(ctx, &sdk.UpdateFlowSourceInput{
		FlowArn:   flow.FlowArn,
		SourceArn: added.Sources[0].SourceArn, Description: new("updated backup"), MaxBitrate: new(int32(1000000)),
	})
	if err != nil || aws.ToString(source.Source.Description) != "updated backup" {
		t.Fatalf("update source: %+v %v", source, err)
	}

	outputs, err := c.AddFlowOutputs(
		ctx,
		&sdk.AddFlowOutputsInput{FlowArn: flow.FlowArn, Outputs: []types.AddOutputRequest{
			{
				Name:        new("destination"),
				Protocol:    types.ProtocolRtp,
				Destination: new("192.0.2.1"),
				Port:        new(int32(6000)),
			},
		}},
	)
	if err != nil || len(outputs.Outputs) != 1 {
		t.Fatalf("add outputs: %+v %v", outputs, err)
	}

	output, err := c.UpdateFlowOutput(ctx, &sdk.UpdateFlowOutputInput{
		FlowArn:   flow.FlowArn,
		OutputArn: outputs.Outputs[0].OutputArn, Port: new(int32(6002)), Description: new("updated output"),
	})
	if err != nil || aws.ToInt32(output.Output.Port) != 6002 {
		t.Fatalf("update output: %+v %v", output, err)
	}

	if _, checkErr := c.StartFlow(ctx, &sdk.StartFlowInput{FlowArn: flow.FlowArn}); checkErr != nil {
		t.Fatal(checkErr)
	}

	connectWait(t, c, flow.FlowArn, types.StatusActive)
	_, err = c.DeleteFlow(ctx, &sdk.DeleteFlowInput{FlowArn: flow.FlowArn})
	dataSyncError(t, err, "BadRequestException")
	if _, checkErr := c.StopFlow(ctx, &sdk.StopFlowInput{FlowArn: flow.FlowArn}); checkErr != nil {
		t.Fatal(checkErr)
	}

	connectWait(t, c, flow.FlowArn, types.StatusStandby)
	if _, checkErr := c.RemoveFlowOutput(
		ctx,
		&sdk.RemoveFlowOutputInput{FlowArn: flow.FlowArn, OutputArn: output.Output.OutputArn},
	); checkErr != nil {
		t.Fatal(checkErr)
	}

	if _, checkErr := c.RemoveFlowSource(
		ctx,
		&sdk.RemoveFlowSourceInput{FlowArn: flow.FlowArn, SourceArn: source.Source.SourceArn},
	); checkErr != nil {
		t.Fatal(checkErr)
	}

	if _, checkErr := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: flow.FlowArn, Tags: map[string]string{"team": "media", "remove": "yes"}},
	); checkErr != nil {
		t.Fatal(checkErr)
	}

	if _, checkErr := c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: flow.FlowArn, TagKeys: []string{"remove"}},
	); checkErr != nil {
		t.Fatal(checkErr)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: flow.FlowArn})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["team"] != "media" {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	tagged, err := c.CreateFlow(ctx, &sdk.CreateFlowInput{
		Name:     new("tagged"),
		FlowTags: map[string]string{"env": "dev"},
		Source: &types.SetSourceRequest{
			Name: new("primary"), Protocol: types.ProtocolRtp, IngestPort: new(int32(5000)),
			WhitelistCidr: new("10.0.0.0/8"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: tagged.Flow.FlowArn})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["env"] != "dev" {
		t.Fatalf("flow tags: %+v %v", tags, err)
	}

	connectCreate(t, c, "page-two")
	page, err := c.ListFlows(ctx, &sdk.ListFlowsInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Flows) != 1 || page.NextToken == nil {
		t.Fatalf("page one: %+v %v", page, err)
	}

	next, err := c.ListFlows(ctx, &sdk.ListFlowsInput{MaxResults: new(int32(1)), NextToken: page.NextToken})
	if err != nil || len(next.Flows) != 1 ||
		aws.ToString(next.Flows[0].FlowArn) == aws.ToString(page.Flows[0].FlowArn) {
		t.Fatalf("page two: %+v %v", next, err)
	}

	_, err = c.ListFlows(ctx, &sdk.ListFlowsInput{NextToken: new("**invalid**")})
	dataSyncError(t, err, "BadRequestException")
	if _, checkErr := c.DeleteFlow(ctx, &sdk.DeleteFlowInput{FlowArn: flow.FlowArn}); checkErr != nil {
		t.Fatal(checkErr)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = c.DescribeFlow(ctx, &sdk.DescribeFlowInput{FlowArn: flow.FlowArn})
		if err != nil {
			dataSyncError(t, err, "NotFoundException")
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("flow was not deleted")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func TestMediaConnectEntitlements(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := connectClient(s, "111111111111")
	subscriber := connectClient(s, "222222222222")
	outsider := connectClient(s, "333333333333")
	ctx := context.Background()
	flow := connectCreate(t, c, "origin")
	ent, err := c.GrantFlowEntitlements(ctx, &sdk.GrantFlowEntitlementsInput{
		FlowArn: flow.FlowArn,
		Entitlements: []types.GrantEntitlementRequest{
			{Name: new("share-one"), Subscribers: []string{"222222222222"}},
			{Name: new("share-two"), Subscribers: []string{"222222222222"}},
		},
	})
	if err != nil || len(ent.Entitlements) != 2 {
		t.Fatalf("grant: %+v %v", ent, err)
	}

	page, err := subscriber.ListEntitlements(ctx, &sdk.ListEntitlementsInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Entitlements) != 1 || page.NextToken == nil {
		t.Fatalf("list: %+v %v", page, err)
	}

	next, err := subscriber.ListEntitlements(
		ctx,
		&sdk.ListEntitlementsInput{MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(next.Entitlements) != 1 ||
		aws.ToString(next.Entitlements[0].EntitlementArn) == aws.ToString(page.Entitlements[0].EntitlementArn) {
		t.Fatalf("next: %+v %v", next, err)
	}

	source := &types.SetSourceRequest{EntitlementArn: ent.Entitlements[0].EntitlementArn}
	received, err := subscriber.CreateFlow(ctx, &sdk.CreateFlowInput{Name: new("receiver"), Source: source})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(received.Flow.Source.EntitlementArn) != aws.ToString(source.EntitlementArn) {
		t.Fatal("entitlement not preserved")
	}

	_, err = outsider.CreateFlow(ctx, &sdk.CreateFlowInput{Name: new("denied"), Source: source})
	dataSyncError(t, err, "BadRequestException")
	_, err = subscriber.DescribeFlow(ctx, &sdk.DescribeFlowInput{FlowArn: flow.FlowArn})
	dataSyncError(t, err, "NotFoundException")
	updated, err := c.UpdateFlowEntitlement(ctx, &sdk.UpdateFlowEntitlementInput{
		FlowArn:           flow.FlowArn,
		EntitlementArn:    source.EntitlementArn,
		Description:       new("updated"),
		EntitlementStatus: types.EntitlementStatusDisabled,
	})
	if err != nil || updated.Entitlement.EntitlementStatus != types.EntitlementStatusDisabled {
		t.Fatalf("update entitlement: %+v %v", updated, err)
	}

	_, err = subscriber.CreateFlow(ctx, &sdk.CreateFlowInput{Name: new("disabled"), Source: source})
	dataSyncError(t, err, "BadRequestException")
	if _, checkErr := c.RevokeFlowEntitlement(
		ctx,
		&sdk.RevokeFlowEntitlementInput{FlowArn: flow.FlowArn, EntitlementArn: source.EntitlementArn},
	); checkErr != nil {
		t.Fatal(checkErr)
	}

	listed, err := subscriber.ListEntitlements(ctx, &sdk.ListEntitlementsInput{})
	if err != nil || len(listed.Entitlements) != 1 {
		t.Fatalf("revoked: %+v %v", listed, err)
	}
}

func TestMediaConnectVpcAndEncryption(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := connectClient(s, "test")
	ctx := context.Background()
	flow := connectCreate(t, c, "vpc")
	role, err := iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 new("media"),
		AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	network := ec2Client(s, "test", "us-east-1")
	vpc, err := network.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.40.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(
		ctx,
		&ec2.CreateSubnetInput{
			VpcId:            vpc.Vpc.VpcId,
			CidrBlock:        new("10.40.1.0/24"),
			AvailabilityZone: new("us-east-1a"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	group, err := network.CreateSecurityGroup(
		ctx,
		&ec2.CreateSecurityGroupInput{VpcId: vpc.Vpc.VpcId, GroupName: new("media"), Description: new("media")},
	)
	if err != nil {
		t.Fatal(err)
	}

	request := types.VpcInterfaceRequest{
		Name:             new("private"),
		RoleArn:          role.Role.Arn,
		SubnetId:         subnet.Subnet.SubnetId,
		SecurityGroupIds: []string{*group.GroupId},
	}

	added, err := c.AddFlowVpcInterfaces(
		ctx,
		&sdk.AddFlowVpcInterfacesInput{FlowArn: flow.FlowArn, VpcInterfaces: []types.VpcInterfaceRequest{request}},
	)
	if err != nil || len(added.VpcInterfaces) != 1 {
		t.Fatalf("vpc: %+v %v", added, err)
	}

	request.Name = new("bad")
	request.SubnetId = new("subnet-missing")
	_, err = c.AddFlowVpcInterfaces(
		ctx,
		&sdk.AddFlowVpcInterfacesInput{FlowArn: flow.FlowArn, VpcInterfaces: []types.VpcInterfaceRequest{request}},
	)
	dataSyncError(t, err, "BadRequestException")
	request.SubnetId = subnet.Subnet.SubnetId
	request.SecurityGroupIds = []string{"sg-missing"}
	_, err = c.AddFlowVpcInterfaces(
		ctx,
		&sdk.AddFlowVpcInterfacesInput{FlowArn: flow.FlowArn, VpcInterfaces: []types.VpcInterfaceRequest{request}},
	)
	dataSyncError(t, err, "BadRequestException")
	request.RoleArn = new("arn:aws:iam::000000000000:role/missing")
	_, err = c.AddFlowVpcInterfaces(
		ctx,
		&sdk.AddFlowVpcInterfacesInput{FlowArn: flow.FlowArn, VpcInterfaces: []types.VpcInterfaceRequest{request}},
	)
	dataSyncError(t, err, "BadRequestException")
	if _, checkErr := c.RemoveFlowVpcInterface(
		ctx,
		&sdk.RemoveFlowVpcInterfaceInput{FlowArn: flow.FlowArn, VpcInterfaceName: new("private")},
	); checkErr != nil {
		t.Fatal(checkErr)
	}

	secretClient := secretsmanager.New(
		secretsmanager.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	secret, err := secretClient.CreateSecret(
		ctx,
		&secretsmanager.CreateSecretInput{Name: new("media-key"), SecretString: new("0123456789abcdef")},
	)
	if err != nil {
		t.Fatal(err)
	}

	encryption := &types.Encryption{
		Algorithm: types.AlgorithmAes128,
		KeyType:   types.KeyTypeStaticKey,
		RoleArn:   role.Role.Arn,
		SecretArn: secret.ARN,
	}

	output := types.AddOutputRequest{
		Name:        new("encrypted"),
		Protocol:    types.ProtocolZixiPush,
		Destination: new("192.0.2.2"),
		Port:        new(int32(7000)),
		Encryption:  encryption,
	}

	if _, checkErr := c.AddFlowOutputs(
		ctx,
		&sdk.AddFlowOutputsInput{FlowArn: flow.FlowArn, Outputs: []types.AddOutputRequest{output}},
	); checkErr != nil {
		t.Fatal(checkErr)
	}

	invalidOutput := output
	invalidOutput.Name = new("rtp-encryption")
	invalidOutput.Protocol = types.ProtocolRtp
	_, err = c.AddFlowOutputs(
		ctx,
		&sdk.AddFlowOutputsInput{FlowArn: flow.FlowArn, Outputs: []types.AddOutputRequest{invalidOutput}},
	)
	dataSyncError(t, err, "BadRequestException")
	srtEncryption := *encryption
	srtEncryption.KeyType = types.KeyTypeSrtPassword
	if _, srtErr := c.CreateFlow(ctx, &sdk.CreateFlowInput{Name: new("srt-encryption"), Source: &types.SetSourceRequest{
		Protocol:   types.ProtocolSrtListener,
		IngestPort: new(int32(5000)),
		MinLatency: new(int32(100)),
		Decryption: new(srtEncryption),
	}}); srtErr != nil {
		t.Fatal(srtErr)
	}

	encryption.SecretArn = new("arn:aws:secretsmanager:us-east-1:000000000000:secret:missing")
	output.Name = new("invalid-encryption")
	_, err = c.AddFlowOutputs(
		ctx,
		&sdk.AddFlowOutputsInput{FlowArn: flow.FlowArn, Outputs: []types.AddOutputRequest{output}},
	)
	dataSyncError(t, err, "BadRequestException")
	_, err = c.CreateFlow(
		ctx,
		&sdk.CreateFlowInput{
			Name:   new("bad-protocol"),
			Source: &types.SetSourceRequest{Protocol: types.Protocol("invalid")},
		},
	)
	dataSyncError(t, err, "BadRequestException")
	_, err = c.RemoveFlowOutput(
		ctx,
		&sdk.RemoveFlowOutputInput{
			FlowArn:   flow.FlowArn,
			OutputArn: new("arn:aws:mediaconnect:us-east-1:000000000000:output:missing:test"),
		},
	)
	dataSyncError(t, err, "NotFoundException")
}

func TestMediaConnectPersistentRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := connectClient(s, "test")
	ctx := context.Background()
	flow := connectCreate(t, c, "persistent")
	if _, checkErr := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: flow.FlowArn, Tags: map[string]string{"persist": "yes"}},
	); checkErr != nil {
		t.Fatal(checkErr)
	}

	if _, checkErr := c.StartFlow(ctx, &sdk.StartFlowInput{FlowArn: flow.FlowArn}); checkErr != nil {
		t.Fatal(checkErr)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = connectClient(s, "test")
	connectWait(t, c, flow.FlowArn, types.StatusActive)
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: flow.FlowArn})
	if err != nil || tags.Tags["persist"] != "yes" {
		t.Fatalf("restart tags: %+v %v", tags, err)
	}

	if _, checkErr := c.StopFlow(ctx, &sdk.StopFlowInput{FlowArn: flow.FlowArn}); checkErr != nil {
		t.Fatal(checkErr)
	}

	connectWait(t, c, flow.FlowArn, types.StatusStandby)
}

func TestMediaConnectCloudFormation(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := cfnClient(s)
	ctx := context.Background()
	template := func(updated bool) string {
		description := "before"
		if updated {
			description = "after"
		}

		body := fmt.Sprintf(`{
 "Resources":{
 "Flow":{"Type":"AWS::MediaConnect::Flow","Properties":{"Name":"stack-flow",
 "Source":{"Name":"primary","Protocol":"rtp","IngestPort":5000,"Description":"%s"},
 "SourceFailoverConfig":{"State":"ENABLED","FailoverMode":"FAILOVER"},"Tags":[{"Key":"phase","Value":"%s"}]}},
 "Source":{"Type":"AWS::MediaConnect::FlowSource","Properties":{"FlowArn":{"Ref":"Flow"},
 "Name":"backup","Description":"%s","Protocol":"rtp","IngestPort":5002}},
 "Output":{"Type":"AWS::MediaConnect::FlowOutput","Properties":{"FlowArn":{"Ref":"Flow"},
 "Name":"output","Protocol":"rtp","Destination":"192.0.2.10","Port":6000,"Description":"%s"}},
 "Entitlement":{"Type":"AWS::MediaConnect::FlowEntitlement","Properties":{"FlowArn":{"Ref":"Flow"},
 "Name":"share","Description":"%s","Subscribers":["222222222222"]}},
 "Vpc":{"Type":"AWS::EC2::VPC","Properties":{"CidrBlock":"10.88.0.0/16"}},
 "Subnet":{"Type":"AWS::EC2::Subnet","Properties":{"VpcId":{"Ref":"Vpc"},"CidrBlock":"10.88.1.0/24",
 "AvailabilityZone":"us-east-1a"}},
 "Group":{"Type":"AWS::EC2::SecurityGroup","Properties":{"VpcId":{"Ref":"Vpc"},"GroupDescription":"media"}},
 "Role":{"Type":"AWS::IAM::Role","Properties":{"AssumeRolePolicyDocument":{"Version":"2012-10-17","Statement":[]}}},
 "Interface":{"Type":"AWS::MediaConnect::FlowVpcInterface","Properties":{"FlowArn":{"Ref":"Flow"},"Name":"private",
 "SubnetId":{"Ref":"Subnet"},"SecurityGroupIds":[{"Ref":"Group"}],"RoleArn":{"Fn::GetAtt":["Role","Arn"]}}}
 },"Outputs":{
 "Flow":{"Value":{"Ref":"Flow"}},"Source":{"Value":{"Ref":"Source"}},
 "Output":{"Value":{"Ref":"Output"}},"Entitlement":{"Value":{"Ref":"Entitlement"}}}}`,
			description, description, description, description, description)
		if !json.Valid([]byte(body)) {
			t.Fatal("invalid fixture")
		}

		return body
	}

	if _, checkErr := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("media-stack"), TemplateBody: new(template(false)),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
	}); checkErr != nil {
		t.Fatal(checkErr)
	}

	initial := wantStatus(ctx, t, c, "media-stack", cfntypes.StackStatusCreateComplete)
	ids := outputsOf(new(initial))
	if _, checkErr := c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: new("media-stack"), TemplateBody: new(template(true)),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
	}); checkErr != nil {
		t.Fatal(checkErr)
	}

	updated := wantStatus(ctx, t, c, "media-stack", cfntypes.StackStatusUpdateComplete)
	for key, id := range outputsOf(new(updated)) {
		if ids[key] != id {
			t.Fatalf("%s replaced during mutable update", key)
		}
	}

	described, err := connectClient(s, "test").DescribeFlow(ctx, &sdk.DescribeFlowInput{FlowArn: new(ids["Flow"])})
	if err != nil || aws.ToString(described.Flow.Source.Description) != "after" ||
		len(described.Flow.VpcInterfaces) != 1 {
		t.Fatalf("updated configuration: %+v %v", described, err)
	}

	if _, checkErr := c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("media-stack")}); checkErr != nil {
		t.Fatal(checkErr)
	}

	waitFor(t, "MediaConnect stack deletion", func() bool {
		_, err := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("media-stack")})
		if err != nil {
			dataSyncError(t, err, "ValidationError")
			return true
		}

		return false
	})
}
