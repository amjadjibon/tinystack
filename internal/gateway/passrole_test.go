package gateway

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	agentcoreapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcorecontrol"
	cognitoapi "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidentity"
	userpoolapi "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	ecsapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	eksapi "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	emrapi "github.com/amjadjibon/tinystack/internal/aws/generated/emrserverless"
	eventsapi "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	firehoseapi "github.com/amjadjibon/tinystack/internal/aws/generated/firehose"
	iotapi "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	lakeapi "github.com/amjadjibon/tinystack/internal/aws/generated/lakeformation"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	sesapi "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/iam"
)

func TestPassedRoleInputs(t *testing.T) {
	const role = "arn:aws:iam::000000000000:role/passed"
	for _, test := range []struct {
		service, operation, principal string
		input                         any
		roles                         []string
	}{
		{
			"lambda", "UpdateFunctionConfiguration", "lambda.amazonaws.com",
			&lambdaapi.UpdateFunctionConfigurationRequest{Role: new(role)},
			[]string{role},
		},
		{"lambda", "UpdateFunctionConfiguration", "", &lambdaapi.UpdateFunctionConfigurationRequest{}, nil},
		{
			"ecs", "RegisterTaskDefinition", "ecs-tasks.amazonaws.com",
			&ecsapi.RegisterTaskDefinitionRequest{TaskRoleArn: new(role), ExecutionRoleArn: new(role)},
			[]string{role},
		},
		{"ecs", "RunTask", "ecs-tasks.amazonaws.com", &ecsapi.RunTaskRequest{
			Overrides: &ecsapi.TaskOverride{TaskRoleArn: new(role), ExecutionRoleArn: new(role + "-execution")},
		}, []string{role, role + "-execution"}},
		{"eventbridge", "PutTargets", "events.amazonaws.com", &eventsapi.PutTargetsRequest{
			Targets: []eventsapi.Target{{RoleArn: new(role)}, {}, {RoleArn: new(role + "-other")}},
		}, []string{role, role + "-other"}},
		{"firehose", "CreateDeliveryStream", "firehose.amazonaws.com", &firehoseapi.CreateDeliveryStreamInput{
			KinesisStreamSourceConfiguration: &firehoseapi.KinesisStreamSourceConfiguration{RoleARN: new(role)},
			S3DestinationConfiguration:       &firehoseapi.S3DestinationConfiguration{RoleARN: new(role + "-destination")},
		}, []string{role, role + "-destination"}},
		{"cognitoidentity", "SetIdentityPoolRoles", "cognito-identity.amazonaws.com", &cognitoapi.SetIdentityPoolRolesInput{
			Roles: map[string]string{"authenticated": role},
			RoleMappings: map[string]cognitoapi.RoleMapping{"provider": {
				RulesConfiguration: &cognitoapi.RulesConfigurationType{Rules: []cognitoapi.MappingRule{
					{RoleARN: new(role + "-mapped")},
				}},
			}},
		}, []string{role, role + "-mapped"}},

		{
			"lakeformation", "RegisterResource", "lakeformation.amazonaws.com",
			&lakeapi.RegisterResourceRequest{RoleArn: new(role)},
			[]string{role},
		},
		{
			"emrserverless", "StartJobRun", "emr-serverless.amazonaws.com",
			&emrapi.StartJobRunRequest{ExecutionRoleArn: new(role)},
			[]string{role},
		},
		{
			"sesv2", "CreateConfigurationSetEventDestination", "ses.amazonaws.com",
			&sesapi.CreateConfigurationSetEventDestinationRequest{EventDestination: &sesapi.EventDestinationDefinition{
				KinesisFirehoseDestination: &sesapi.KinesisFirehoseDestination{IamRoleArn: new(role)},
			}},
			[]string{role},
		},

		{
			"cognitoidp", "CreateGroup", "cognito-identity.amazonaws.com",
			&userpoolapi.CreateGroupRequest{RoleArn: new(role)},
			[]string{role},
		},
		{
			"bedrockagentcorecontrol", "CreateAgentRuntime", "bedrock-agentcore.amazonaws.com",
			&agentcoreapi.CreateAgentRuntimeRequest{RoleArn: new(role)},
			[]string{role},
		},
		{
			"bedrockagentcorecontrol", "UpdateMemory", "bedrock-agentcore.amazonaws.com",
			&agentcoreapi.UpdateMemoryInput{MemoryExecutionRoleArn: new(role)},
			[]string{role},
		},
		{"eks", "CreateNodegroup", "eks.amazonaws.com", &eksapi.CreateNodegroupRequest{NodeRole: new(role)}, []string{role}},
		{
			"eks", "CreateFargateProfile", "eks.amazonaws.com",
			&eksapi.CreateFargateProfileRequest{PodExecutionRoleArn: new(role)},
			[]string{role},
		},
		{
			"iot", "CreateRoleAlias", "iot.amazonaws.com",
			&iotapi.CreateRoleAliasRequest{RoleArn: new(role)},
			[]string{role},
		},
		{
			"iot", "UpdateRoleAlias", "iot.amazonaws.com",
			&iotapi.UpdateRoleAliasRequest{RoleArn: new(role)},
			[]string{role},
		},
		{"iam", "AddRoleToInstanceProfile", "", nil, nil},
		{"sts", "AssumeRole", "", nil, nil},
	} {
		t.Run(test.service+"/"+test.operation, func(t *testing.T) {
			server := &Server{}
			ctx := request.WithMetadata(t.Context(), request.Metadata{AccountID: "000000000000"})
			targets, err := server.passedRoles(ctx, test.service, test.operation, test.input)
			if err != nil {
				t.Fatal(err)
			}

			var roles []string
			for _, target := range targets {
				if target.Action != "iam:PassRole" || target.OwnerAccount != "" || target.Policy != "" ||
					!reflect.DeepEqual(target.Values["iam:passedtoservice"], []string{test.principal}) {
					t.Fatalf("incorrect target: %+v", target)
				}

				roles = append(roles, target.ARN)
			}

			if !reflect.DeepEqual(roles, test.roles) {
				t.Fatalf("roles = %v, want %v", roles, test.roles)
			}
		})
	}
}

func TestPassedRoleProfileResolution(t *testing.T) {
	const role = "arn:aws:iam::000000000000:role/passed"
	const account = "000000000000"
	ctx := request.WithMetadata(t.Context(), request.Metadata{AccountID: account})
	in := &ec2api.RunInstancesRequest{IamInstanceProfile: &ec2api.IamInstanceProfileSpecification{Name: new("profile")}}
	server := &Server{}
	if _, err := server.passedRoles(ctx, "ec2", "RunInstances", in); err == nil {
		t.Fatal("missing resolver allowed")
	}

	for _, test := range []struct {
		err        error
		name, role string
		found      bool
	}{
		{name: "found", found: true, role: role},
		{name: "missing"},
		{name: "empty", found: true},
		{name: "error", err: errors.New("storage failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			server.UseInstanceProfiles(
				func(_ context.Context, gotAccount, ref string) (iam.InstanceProfile, bool, error) {
					if gotAccount != account || ref != "profile" {
						t.Fatalf("lookup %s/%s", gotAccount, ref)
					}

					return iam.InstanceProfile{RoleARN: test.role}, test.found, test.err
				},
			)
			targets, err := server.passedRoles(ctx, "ec2", "RunInstances", in)
			if test.role == "" {
				if err == nil {
					t.Fatal("unresolved profile allowed")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			want := []catalog.AuthorizationResource{{
				Action: "iam:PassRole", ARN: role,
				Values: map[string][]string{"iam:passedtoservice": {"ec2.amazonaws.com"}},
			}}
			if !reflect.DeepEqual(targets, want) {
				t.Fatalf("targets = %+v", targets)
			}
		})
	}
}
