package integration_test

import (
	"context"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/globalaccelerator"
	"github.com/aws/aws-sdk-go-v2/service/globalaccelerator/types"
)

func TestCloudFormationGlobalAccelerator(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := gaClient(s, "test", "us-west-2")
	template := `{
 "Resources":{
  "FlowBucket":{"Type":"AWS::S3::Bucket","Properties":{"BucketName":"cfn-ga-flow-logs"}},
  "Accelerator":{"Type":"AWS::GlobalAccelerator::Accelerator","Properties":{
   "Name":"cfn-accelerator","Enabled":true,"IpAddressType":"IPV4",
   "FlowLogsEnabled":true,"FlowLogsS3Bucket":{"Ref":"FlowBucket"},"FlowLogsS3Prefix":"logs/",
   "Tags":[{"Key":"team","Value":"core"}]}},
  "Listener":{"Type":"AWS::GlobalAccelerator::Listener","Properties":{
   "AcceleratorArn":{"Ref":"Accelerator"},"Protocol":"TCP",
   "PortRanges":[{"FromPort":443,"ToPort":443}]}},
  "Group":{"Type":"AWS::GlobalAccelerator::EndpointGroup","Properties":{
   "ListenerArn":{"Ref":"Listener"},"EndpointGroupRegion":"us-east-1","TrafficDialPercentage":100}}
 },
 "Outputs":{
  "Accelerator":{"Value":{"Ref":"Accelerator"}},
  "Listener":{"Value":{"Fn::GetAtt":["Listener","ListenerArn"]}},
  "Group":{"Value":{"Fn::GetAtt":["Group","EndpointGroupArn"]}},
  "DNS":{"Value":{"Fn::GetAtt":["Accelerator","DnsName"]}},
  "IPs":{"Value":{"Fn::Join":[",",{"Fn::GetAtt":["Accelerator","Ipv4Addresses"]}]}}
 }} `
	if _, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("ga-stack"), TemplateBody: new(template)},
	); err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, cf, "ga-stack", cfntypes.StackStatusCreateComplete)
	before := outputsOf(&stack)
	if before["DNS"] == "" || !strings.Contains(before["IPs"], "192.0.2.") || before["Group"] == "" {
		t.Fatalf("outputs: %+v", before)
	}

	changed := strings.ReplaceAll(template, `"Value":"core"`, `"Value":"platform"`)
	changed = strings.ReplaceAll(changed, `"IpAddressType":"IPV4"`, `"IpAddressType":"DUAL_STACK"`)
	changed = strings.ReplaceAll(changed, `"Protocol":"TCP"`, `"Protocol":"UDP","ClientAffinity":"SOURCE_IP"`)
	changed = strings.ReplaceAll(
		changed,
		`"TrafficDialPercentage":100`,
		`"TrafficDialPercentage":25,"HealthCheckPort":8443,"HealthCheckIntervalSeconds":10,"ThresholdCount":5`,
	)
	if _, err := cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("ga-stack"), TemplateBody: new(changed)},
	); err != nil {
		t.Fatal(err)
	}

	stack = wantStatus(ctx, t, cf, "ga-stack", cfntypes.StackStatusUpdateComplete)
	after := outputsOf(&stack)
	for _, key := range []string{"Accelerator", "Listener", "Group"} {
		if before[key] != after[key] {
			t.Fatalf("mutable %s was replaced", key)
		}
	}

	a, err := c.DescribeAccelerator(ctx, &sdk.DescribeAcceleratorInput{AcceleratorArn: new(after["Accelerator"])})
	if err != nil || a.Accelerator.IpAddressType != types.IpAddressTypeDualStack {
		t.Fatalf("CFN accelerator update: %+v %v", a, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(after["Accelerator"])})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Value != "platform" {
		t.Fatalf("CFN tags: %+v %v", tags, err)
	}

	l, err := c.DescribeListener(ctx, &sdk.DescribeListenerInput{ListenerArn: new(after["Listener"])})
	if err != nil || l.Listener.Protocol != types.ProtocolUdp ||
		l.Listener.ClientAffinity != types.ClientAffinitySourceIp {
		t.Fatalf("CFN listener update: %+v %v", l, err)
	}

	g, err := c.DescribeEndpointGroup(ctx, &sdk.DescribeEndpointGroupInput{EndpointGroupArn: new(after["Group"])})
	if err != nil || *g.EndpointGroup.TrafficDialPercentage != 25 {
		t.Fatalf("CFN group update: %+v %v", g, err)
	}

	reset := strings.ReplaceAll(
		changed,
		`,"HealthCheckPort":8443,"HealthCheckIntervalSeconds":10,"ThresholdCount":5`,
		``,
	)
	reset = strings.ReplaceAll(
		reset,
		`"FlowLogsEnabled":true,"FlowLogsS3Bucket":{"Ref":"FlowBucket"},"FlowLogsS3Prefix":"logs/",`,
		``,
	)
	if _, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("ga-stack"), TemplateBody: new(reset)},
	); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, "ga-stack", cfntypes.StackStatusUpdateComplete)
	resetGroup, err := c.DescribeEndpointGroup(
		ctx,
		&sdk.DescribeEndpointGroupInput{EndpointGroupArn: new(after["Group"])},
	)
	if err != nil || *resetGroup.EndpointGroup.HealthCheckPort != 443 ||
		*resetGroup.EndpointGroup.HealthCheckIntervalSeconds != 30 ||
		*resetGroup.EndpointGroup.ThresholdCount != 3 {
		t.Fatalf("CFN default restoration: %+v %v", resetGroup, err)
	}

	flow, err := c.DescribeAcceleratorAttributes(
		ctx,
		&sdk.DescribeAcceleratorAttributesInput{AcceleratorArn: new(after["Accelerator"])},
	)
	if err != nil || *flow.AcceleratorAttributes.FlowLogsEnabled || *flow.AcceleratorAttributes.FlowLogsS3Bucket != "" {
		t.Fatalf("CFN flow log removal: %+v %v", flow, err)
	}

	changed = reset
	changed = strings.ReplaceAll(changed, `"EndpointGroupRegion":"us-east-1"`, `"EndpointGroupRegion":"eu-west-1"`)
	if _, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("ga-stack"), TemplateBody: new(changed)},
	); err != nil {
		t.Fatal(err)
	}

	stack = wantStatus(ctx, t, cf, "ga-stack", cfntypes.StackStatusUpdateComplete)
	replaced := outputsOf(&stack)
	if replaced["Group"] == after["Group"] || replaced["Accelerator"] != after["Accelerator"] {
		t.Fatal("incorrect CFN replacement")
	}

	_, err = c.DescribeEndpointGroup(ctx, &sdk.DescribeEndpointGroupInput{EndpointGroupArn: new(after["Group"])})
	gaError(t, err, "EndpointGroupNotFoundException")
	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("ga-stack")}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *stack.StackId, cfntypes.StackStatusDeleteComplete)

	_, err = c.DescribeAccelerator(ctx, &sdk.DescribeAcceleratorInput{AcceleratorArn: new(after["Accelerator"])})
	gaError(t, err, "AcceleratorNotFoundException")
}
