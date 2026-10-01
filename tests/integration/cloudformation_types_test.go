package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
)

const platformTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Parameters:
  Port: {Type: Number, Default: 80}
  Shards: {Type: Number, Default: 1}
  Threshold: {Type: Number, Default: 5}
  Release: {Type: String, Default: first}
  Greeting: {Type: String, Default: hello}
Resources:
  Vpc:
    Type: AWS::EC2::VPC
    Properties:
      CidrBlock: 10.1.0.0/16
      Tags: [{Key: Name, Value: platform}]
  Subnet:
    Type: AWS::EC2::Subnet
    Properties:
      VpcId: !Ref Vpc
      CidrBlock: 10.1.1.0/24
      AvailabilityZone: !Select [0, !GetAZs ""]
  Web:
    Type: AWS::EC2::SecurityGroup
    Properties:
      GroupDescription: web
      VpcId: !Ref Vpc
      SecurityGroupIngress:
        - {IpProtocol: tcp, FromPort: !Ref Port, ToPort: !Ref Port, CidrIp: 0.0.0.0/0}
      SecurityGroupEgress:
        - {IpProtocol: tcp, FromPort: 443, ToPort: 443, CidrIp: 0.0.0.0/0}
  Repository:
    Type: AWS::ECR::Repository
    Properties:
      ImageTagMutability: IMMUTABLE
      EmptyOnDelete: true
  Cluster:
    Type: AWS::ECS::Cluster
  TaskDefinition:
    Type: AWS::ECS::TaskDefinition
    Properties:
      Cpu: 256
      Memory: 512
      NetworkMode: awsvpc
      RequiresCompatibilities: [FARGATE]
      ContainerDefinitions:
        - Name: web
          Image: !Sub "${Repository.RepositoryUri}:latest"
          Essential: true
          PortMappings: [{ContainerPort: 80}]
  Service:
    Type: AWS::ECS::Service
    Properties:
      Cluster: !Ref Cluster
      TaskDefinition: !Ref TaskDefinition
      DesiredCount: 1
      LaunchType: FARGATE
      NetworkConfiguration:
        AwsvpcConfiguration:
          Subnets: [!Ref Subnet]
          SecurityGroups: [!Ref Web]
  Role:
    Type: AWS::IAM::Role
    Properties:
      AssumeRolePolicyDocument:
        Version: "2012-10-17"
        Statement:
          - Effect: Allow
            Principal: {Service: [states.amazonaws.com, lambda.amazonaws.com]}
            Action: sts:AssumeRole
  Access:
    Type: AWS::IAM::ManagedPolicy
    Properties:
      PolicyDocument:
        Version: "2012-10-17"
        Statement: [{Effect: Allow, Action: "kinesis:PutRecord", Resource: !GetAtt Stream.Arn}]
      Roles: [!Ref Role]
  Inline:
    Type: AWS::IAM::Policy
    Properties:
      PolicyName: logs
      PolicyDocument:
        Version: "2012-10-17"
        Statement: [{Effect: Allow, Action: "logs:*", Resource: "*"}]
      Roles: [!Ref Role]
  Machine:
    Type: AWS::StepFunctions::StateMachine
    Properties:
      RoleArn: !GetAtt Role.Arn
      Definition:
        StartAt: Greet
        States:
          Greet: {Type: Pass, Result: "${Greeting}", End: true}
      DefinitionSubstitutions:
        Greeting: !Ref Greeting
  Activity:
    Type: AWS::StepFunctions::Activity
    Properties:
      Name: !Sub "${AWS::StackName}-review"
  Stream:
    Type: AWS::Kinesis::Stream
    Properties:
      ShardCount: !Ref Shards
      RetentionPeriodHours: 48
  Bus:
    Type: AWS::Events::EventBus
    Properties:
      Name: !Sub "${AWS::StackName}-bus"
      Description: orders
  Alarm:
    Type: AWS::CloudWatch::Alarm
    Properties:
      Namespace: App
      MetricName: Errors
      Statistic: Sum
      Period: 60
      EvaluationPeriods: 1
      Threshold: !Ref Threshold
      ComparisonOperator: GreaterThanThreshold
      Dimensions: [{Name: Stream, Value: !Ref Stream}]
  Function:
    Type: AWS::Lambda::Function
    Properties:
      Runtime: python3.12
      Handler: index.handler
      Role: !GetAtt Role.Arn
      Code:
        ZipFile: !Sub |
          def handler(event, context):
              return "${Release}"
  Version:
    Type: AWS::Lambda::Version
    Properties:
      FunctionName: !Ref Function
      Description: !Ref Release
  Live:
    Type: AWS::Lambda::Alias
    Properties:
      FunctionName: !Ref Function
      FunctionVersion: !GetAtt Version.Version
      Name: live
Outputs:
  Vpc: {Value: !Ref Vpc}
  DefaultGroup: {Value: !GetAtt Vpc.DefaultSecurityGroup}
  Group: {Value: !GetAtt Web.GroupId}
  Repository: {Value: !Ref Repository}
  Cluster: {Value: !Ref Cluster}
  Service: {Value: !GetAtt Service.Name}
  Machine: {Value: !Ref Machine}
  Activity: {Value: !Ref Activity}
  Stream: {Value: !Ref Stream}
  Policy: {Value: !Ref Access}
  Alarm: {Value: !Ref Alarm}
  Version: {Value: !GetAtt Version.Version}
  Alias: {Value: !Ref Live}
`

func TestCloudFormationResourceTypes(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)

	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("platform"), TemplateBody: aws.String(platformTemplate),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
	})
	if err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, c, "platform", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)

	ec := ec2Client(s, "test", "us-east-1")
	groups, err := ec.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{outputs["Group"], outputs["DefaultGroup"]},
	})
	if err != nil || len(groups.SecurityGroups) != 2 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}

	for _, g := range groups.SecurityGroups {
		if aws.ToString(g.GroupId) != outputs["Group"] {
			continue
		}

		if aws.ToString(g.VpcId) != outputs["Vpc"] || len(g.IpPermissions) != 1 ||
			aws.ToInt32(g.IpPermissions[0].FromPort) != 80 || len(g.IpPermissionsEgress) != 1 ||
			aws.ToInt32(g.IpPermissionsEgress[0].FromPort) != 443 {
			t.Fatalf("group = %+v", g)
		}
	}

	repos, err := ecrClient(s, "test", "us-east-1").DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{outputs["Repository"]},
	})
	if err != nil || repos.Repositories[0].ImageTagMutability != "IMMUTABLE" {
		t.Fatalf("repositories = %+v, %v", repos, err)
	}

	containers := ecsClient(s, "us-east-1")
	services, err := containers.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster: aws.String(outputs["Cluster"]), Services: []string{outputs["Service"]},
	})
	if err != nil || len(services.Services) != 1 || services.Services[0].DesiredCount != 1 ||
		!strings.Contains(
			aws.ToString(services.Services[0].TaskDefinition),
			":task-definition/platform-TaskDefinition-",
		) {
		t.Fatalf("services = %+v, %v", services, err)
	}

	machine, err := sfnClient(s).DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{
		StateMachineArn: aws.String(outputs["Machine"]),
	})
	if err != nil || !strings.Contains(aws.ToString(machine.Definition), `"hello"`) {
		t.Fatalf("state machine = %+v, %v", machine, err)
	}

	if !strings.HasSuffix(outputs["Activity"], ":activity:platform-review") {
		t.Fatalf("activity = %q", outputs["Activity"])
	}

	streams := kinesisClient(s)
	stream, err := streams.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{
		StreamName: aws.String(outputs["Stream"]),
	})
	if err != nil || aws.ToInt32(stream.StreamDescriptionSummary.RetentionPeriodHours) != 48 ||
		aws.ToInt32(stream.StreamDescriptionSummary.OpenShardCount) != 1 {
		t.Fatalf("stream = %+v, %v", stream, err)
	}

	bus, err := eventsClient(s, "test").DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{
		Name: aws.String("platform-bus"),
	})
	if err != nil || aws.ToString(bus.Description) != "orders" {
		t.Fatalf("bus = %+v, %v", bus, err)
	}

	alarms := cloudwatchClient(s)
	alarm, err := alarms.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{AlarmNames: []string{outputs["Alarm"]}})
	if err != nil || len(alarm.MetricAlarms) != 1 || aws.ToFloat64(alarm.MetricAlarms[0].Threshold) != 5 {
		t.Fatalf("alarm = %+v, %v", alarm, err)
	}

	identities := iamClient(s, "test", "us-east-1")
	resources, err := c.DescribeStackResource(ctx, &cfn.DescribeStackResourceInput{
		StackName: aws.String("platform"), LogicalResourceId: aws.String("Role"),
	})
	if err != nil {
		t.Fatal(err)
	}

	role := resources.StackResourceDetail.PhysicalResourceId
	attached, err := identities.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: role})
	if err != nil || len(attached.AttachedPolicies) != 1 ||
		aws.ToString(attached.AttachedPolicies[0].PolicyArn) != outputs["Policy"] {
		t.Fatalf("attached = %+v, %v", attached, err)
	}

	inline, err := identities.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: role})
	if err != nil || len(inline.PolicyNames) != 1 || inline.PolicyNames[0] != "logs" {
		t.Fatalf("inline = %+v, %v", inline, err)
	}

	functions := lambdaClient(s, "test")
	function := aws.String(outputs["Alias"][:strings.LastIndex(outputs["Alias"], ":")])
	alias, err := functions.GetAlias(ctx, &sdklambda.GetAliasInput{FunctionName: function, Name: aws.String("live")})
	if err != nil || aws.ToString(alias.FunctionVersion) != outputs["Version"] || outputs["Version"] != "1" {
		t.Fatalf("alias = %+v, %v (outputs %v)", alias, err, outputs)
	}

	// An update changes resources in place and replaces the version: the
	// function's new code is published as a new version, the alias moves to it, and the old one is
	// deleted during cleanup.
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("platform"), UsePreviousTemplate: aws.Bool(true),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		Parameters: []cfntypes.Parameter{
			{ParameterKey: aws.String("Port"), ParameterValue: aws.String("8080")},
			{ParameterKey: aws.String("Shards"), ParameterValue: aws.String("2")},
			{ParameterKey: aws.String("Threshold"), ParameterValue: aws.String("10")},
			{ParameterKey: aws.String("Release"), ParameterValue: aws.String("second")},
			{ParameterKey: aws.String("Greeting"), ParameterValue: aws.String("bonjour")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, c, "platform", cfntypes.StackStatusUpdateComplete)
	updated := outputsOf(&st)
	if updated["Version"] != "2" || updated["Group"] != outputs["Group"] || updated["Stream"] != outputs["Stream"] {
		t.Fatalf("outputs = %v", updated)
	}

	alias, err = functions.GetAlias(ctx, &sdklambda.GetAliasInput{FunctionName: function, Name: aws.String("live")})
	if err != nil || aws.ToString(alias.FunctionVersion) != "2" {
		t.Fatalf("alias = %+v, %v", alias, err)
	}

	versions, err := functions.ListVersionsByFunction(ctx, &sdklambda.ListVersionsByFunctionInput{
		FunctionName: function,
	})
	if err != nil || len(versions.Versions) != 2 || aws.ToString(versions.Versions[1].Version) != "2" {
		t.Fatalf("versions = %+v, %v", versions, err)
	}

	groups, err = ec.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{outputs["Group"]}})
	if err != nil || len(groups.SecurityGroups[0].IpPermissions) != 1 ||
		aws.ToInt32(groups.SecurityGroups[0].IpPermissions[0].FromPort) != 8080 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}

	stream, err = streams.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{
		StreamName: aws.String(outputs["Stream"]),
	})
	if err != nil || aws.ToInt32(stream.StreamDescriptionSummary.OpenShardCount) != 2 {
		t.Fatalf("stream = %+v, %v", stream, err)
	}

	alarm, err = alarms.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{AlarmNames: []string{outputs["Alarm"]}})
	if err != nil || aws.ToFloat64(alarm.MetricAlarms[0].Threshold) != 10 {
		t.Fatalf("alarm = %+v, %v", alarm, err)
	}

	machine, err = sfnClient(s).DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{
		StateMachineArn: aws.String(outputs["Machine"]),
	})
	if err != nil || !strings.Contains(aws.ToString(machine.Definition), `"bonjour"`) {
		t.Fatalf("state machine = %+v, %v", machine, err)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("platform")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack platform to be deleted", func() bool {
		_, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("platform")})
		return describeErr != nil
	})

	if _, err = ec.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{outputs["Vpc"]}}); err == nil {
		t.Fatal("the VPC outlived its stack")
	}

	if _, err = identities.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(outputs["Policy"])}); err == nil {
		t.Fatal("the managed policy outlived its stack")
	}

	clusters, err := containers.DescribeClusters(
		ctx,
		&ecs.DescribeClustersInput{Clusters: []string{outputs["Cluster"]}},
	)
	if err != nil || len(clusters.Clusters) != 1 || aws.ToString(clusters.Clusters[0].Status) != "INACTIVE" {
		t.Fatalf("clusters = %+v, %v", clusters, err)
	}
}
