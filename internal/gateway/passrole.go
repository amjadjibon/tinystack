package gateway

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/iam"
)

// UseInstanceProfiles connects the IAM lookup used for roles passed through EC2 profiles.
func (s *Server) UseInstanceProfiles(
	lookup func(context.Context, string, string) (iam.InstanceProfile, bool, error),
) {
	s.instanceProfiles = lookup
}

const (
	authorizationBatch       = "batch"
	authorizationTransfer    = "transfer"
	inputRoleARN             = "RoleArn"
	inputServiceRoleARN      = "ServiceRoleArn"
	inputExecutionRoleARN    = "ExecutionRoleArn"
	ec2ServicePrincipal      = "ec2.amazonaws.com"
	ecsTasksServicePrincipal = "ecs-tasks.amazonaws.com"
	eksServicePrincipal      = "eks.amazonaws.com"
	iotServicePrincipal      = "iot.amazonaws.com"
)

type passedRoleInput struct {
	principal         string
	operations, paths []string
	profile           bool
}

// passedRoleInputs is the single registry of implemented inputs that pass roles.
// Paths use generated Go member names; intermediate lists are traversed automatically.
// IAM role/profile membership and STS role assumption are deliberately absent.
var passedRoleInputs = map[string][]passedRoleInput{
	"lambda": {
		{
			operations: []string{"CreateFunction", "UpdateFunctionConfiguration"},
			principal:  "lambda.amazonaws.com",
			paths:      []string{"Role"},
		},
	},
	"ecs": {
		{
			operations: []string{"RegisterTaskDefinition"},
			principal:  ecsTasksServicePrincipal,
			paths:      []string{"TaskRoleArn", inputExecutionRoleARN},
		},
		{
			operations: []string{"RunTask"},
			principal:  ecsTasksServicePrincipal,
			paths:      []string{"Overrides.TaskRoleArn", "Overrides.ExecutionRoleArn"},
		},
		{
			operations: []string{"CreateService", "UpdateService"},
			principal:  "ecs.amazonaws.com",
			paths:      []string{"VpcLatticeConfigurations.RoleArn"},
		},
	},
	"sfn": {
		{
			operations: []string{"CreateStateMachine", "UpdateStateMachine", "TestState"},
			principal:  "states.amazonaws.com",
			paths:      []string{inputRoleARN},
		},
	},
	"eventbridge": {
		{
			operations: []string{"PutTargets"},
			principal:  "events.amazonaws.com",
			paths:      []string{"Targets.RoleArn"},
		},
		{
			operations: []string{"PutRule"},
			principal:  "events.amazonaws.com",
			paths:      []string{inputRoleARN},
		},
	},
	"scheduler": {
		{
			operations: []string{"CreateSchedule", "UpdateSchedule"},
			principal:  "scheduler.amazonaws.com",
			paths:      []string{"Target.RoleArn"},
		},
	},
	"ec2": {
		{
			operations: []string{"RunInstances", "AssociateIamInstanceProfile", "ReplaceIamInstanceProfileAssociation"},
			principal:  ec2ServicePrincipal,
			paths:      []string{"IamInstanceProfile.Arn", "IamInstanceProfile.Name"}, profile: true,
		},
		{
			operations: []string{"CreateFlowLogs"},
			principal:  "vpc-flow-logs.amazonaws.com",
			paths:      []string{"DeliverLogsPermissionArn", "DeliverCrossAccountRole"},
		},
	},
	"cloudformation": {
		{
			operations: []string{"CreateStack", "UpdateStack", "DeleteStack", "CreateChangeSet"},
			principal:  "cloudformation.amazonaws.com",
			paths:      []string{"RoleARN"},
		},
	},
	"eks": {
		{
			operations: []string{"CreateCluster"},
			principal:  eksServicePrincipal,
			paths:      []string{inputRoleARN},
		},
		{
			operations: []string{"CreateNodegroup"},
			principal:  eksServicePrincipal,
			paths:      []string{"NodeRole"},
		},
		{
			operations: []string{"CreateFargateProfile"},
			principal:  eksServicePrincipal,
			paths:      []string{"PodExecutionRoleArn"},
		},
		{
			operations: []string{"CreateAddon", "UpdateAddon"},
			principal:  eksServicePrincipal,
			paths:      []string{"ServiceAccountRoleArn"},
		},
		{
			operations: []string{"CreatePodIdentityAssociation", "UpdatePodIdentityAssociation"},
			principal:  "pods.eks.amazonaws.com",
			paths:      []string{inputRoleARN},
		},
	},
	"codebuild": {
		{
			operations: []string{"CreateProject", "UpdateProject"},
			principal:  "codebuild.amazonaws.com",
			paths:      []string{"ServiceRole"},
		},
	},
	"codepipeline": {
		{
			operations: []string{"CreatePipeline", "UpdatePipeline"},
			principal:  "codepipeline.amazonaws.com",
			paths:      []string{"Pipeline.RoleArn", "Pipeline.Stages.Actions.RoleArn"},
		},
	},
	"codedeploy": {
		{
			operations: []string{"CreateDeploymentGroup", "UpdateDeploymentGroup"},
			principal:  "codedeploy.amazonaws.com",
			paths:      []string{inputServiceRoleARN},
		},
	},
	"firehose": {
		{
			operations: []string{"CreateDeliveryStream"},
			principal:  "firehose.amazonaws.com",
			paths: []string{
				"KinesisStreamSourceConfiguration.RoleARN",
				"S3DestinationConfiguration.RoleARN",
				"ExtendedS3DestinationConfiguration.RoleARN",
			},
		},
		{
			operations: []string{"UpdateDestination"},
			principal:  "firehose.amazonaws.com",
			paths:      []string{"S3DestinationUpdate.RoleARN", "ExtendedS3DestinationUpdate.RoleARN"},
		},
	},
	"pipes": {
		{
			operations: []string{"CreatePipe", "UpdatePipe"},
			principal:  "pipes.amazonaws.com",
			paths:      []string{inputRoleARN},
		},
	},
	"backup": {
		{
			operations: []string{"CreateBackupSelection"},
			principal:  "backup.amazonaws.com",
			paths:      []string{"BackupSelection.IamRoleArn"},
		},
	},
	"mwaa": {
		{
			operations: []string{"CreateEnvironment", "UpdateEnvironment"},
			principal:  "airflow.amazonaws.com",
			paths:      []string{inputExecutionRoleARN},
		},
	},
	"appsync": {
		{
			operations: []string{"CreateGraphqlApi", "UpdateGraphqlApi"},
			principal:  "appsync.amazonaws.com",
			paths:      []string{"LogConfig.CloudWatchLogsRoleArn"},
		},
		{
			operations: []string{"CreateDataSource", "UpdateDataSource"},
			principal:  "appsync.amazonaws.com",
			paths:      []string{inputServiceRoleARN},
		},
	},
	authorizationBatch: {
		{
			operations: []string{"CreateComputeEnvironment", "UpdateComputeEnvironment"},
			principal:  "batch.amazonaws.com",
			paths:      []string{"ServiceRole"},
		},
		{
			operations: []string{"CreateComputeEnvironment"},
			principal:  ec2ServicePrincipal,
			paths:      []string{"ComputeResources.InstanceRole"}, profile: true,
		},
		{
			operations: []string{"RegisterJobDefinition"},
			principal:  ecsTasksServicePrincipal,
			paths:      []string{"ContainerProperties.JobRoleArn", "ContainerProperties.ExecutionRoleArn"},
		},
	},
	"applicationautoscaling": {
		{
			operations: []string{"RegisterScalableTarget"},
			principal:  "application-autoscaling.amazonaws.com",
			paths:      []string{"RoleARN"},
		},
	},
	"autoscaling": {
		{
			operations: []string{"CreateAutoScalingGroup", "UpdateAutoScalingGroup"},
			principal:  "autoscaling.amazonaws.com",
			paths:      []string{"ServiceLinkedRoleARN"},
		},
		{
			operations: []string{"CreateLaunchConfiguration"},
			principal:  ec2ServicePrincipal,
			paths:      []string{"IamInstanceProfile"}, profile: true,
		},
	},
	"config": {
		{
			operations: []string{"PutConfigurationRecorder"},
			principal:  "config.amazonaws.com",
			paths:      []string{"ConfigurationRecorder.RoleARN"},
		},
	},
	"logs": {
		{
			operations: []string{"PutSubscriptionFilter"},
			principal:  "logs.amazonaws.com",
			paths:      []string{inputRoleARN},
		},
	},
	"ssm": {
		{
			operations: []string{"SendCommand"},
			principal:  "ssm.amazonaws.com",
			paths:      []string{inputServiceRoleARN},
		},
	},
	authorizationTransfer: {
		{
			operations: []string{"CreateUser", "UpdateUser"},
			principal:  "transfer.amazonaws.com",
			paths:      []string{"Role"},
		},
	},
	"iot": {
		{
			operations: []string{"CreateRoleAlias", "UpdateRoleAlias"},
			// PassRole uses IoT, while the role trust policy uses credentials.iot.amazonaws.com.
			// https://docs.aws.amazon.com/service-authorization/latest/reference/list_iot.html
			principal: iotServicePrincipal,
			paths:     []string{inputRoleARN},
		},
		{
			operations: []string{"CreateProvisioningTemplate", "UpdateProvisioningTemplate"},
			principal:  iotServicePrincipal,
			paths:      []string{"ProvisioningRoleArn"},
		},
		{
			operations: []string{"CreateTopicRule", "ReplaceTopicRule"},
			principal:  iotServicePrincipal,
			paths: []string{
				"TopicRulePayload.Actions.DynamoDB.RoleArn",
				"TopicRulePayload.Actions.DynamoDBv2.RoleArn",
				"TopicRulePayload.Actions.Sns.RoleArn",
				"TopicRulePayload.Actions.Sqs.RoleArn",
				"TopicRulePayload.Actions.Kinesis.RoleArn",
				"TopicRulePayload.Actions.Republish.RoleArn",
				"TopicRulePayload.Actions.S3.RoleArn",
				"TopicRulePayload.Actions.Firehose.RoleArn",
				"TopicRulePayload.Actions.CloudwatchMetric.RoleArn",
				"TopicRulePayload.Actions.CloudwatchAlarm.RoleArn",
				"TopicRulePayload.Actions.CloudwatchLogs.RoleArn",
				"TopicRulePayload.Actions.StepFunctions.RoleArn",
				"TopicRulePayload.ErrorAction.DynamoDB.RoleArn",
				"TopicRulePayload.ErrorAction.DynamoDBv2.RoleArn",
				"TopicRulePayload.ErrorAction.Sns.RoleArn",
				"TopicRulePayload.ErrorAction.Sqs.RoleArn",
				"TopicRulePayload.ErrorAction.Kinesis.RoleArn",
				"TopicRulePayload.ErrorAction.Republish.RoleArn",
				"TopicRulePayload.ErrorAction.S3.RoleArn",
				"TopicRulePayload.ErrorAction.Firehose.RoleArn",
				"TopicRulePayload.ErrorAction.CloudwatchMetric.RoleArn",
				"TopicRulePayload.ErrorAction.CloudwatchAlarm.RoleArn",
				"TopicRulePayload.ErrorAction.CloudwatchLogs.RoleArn",
				"TopicRulePayload.ErrorAction.StepFunctions.RoleArn",
			},
		},
	},
	"cognitoidentity": {
		{
			operations: []string{"SetIdentityPoolRoles"},
			principal:  "cognito-identity.amazonaws.com",
			paths:      []string{"Roles", "RoleMappings.RulesConfiguration.Rules.RoleARN"},
		},
	},
	"lakeformation": {{
		operations: []string{"RegisterResource"},
		principal:  "lakeformation.amazonaws.com",
		paths:      []string{inputRoleARN},
	}},
	"emrserverless": {{
		operations: []string{"StartJobRun"},
		principal:  "emr-serverless.amazonaws.com",
		paths:      []string{inputExecutionRoleARN},
	}},
	"sesv2": {{
		operations: []string{"CreateConfigurationSetEventDestination", "UpdateConfigurationSetEventDestination"},
		principal:  "ses.amazonaws.com",
		paths:      []string{"EventDestination.KinesisFirehoseDestination.IamRoleArn"},
	}},
	"cognitoidp": {{
		operations: []string{"CreateGroup", "UpdateGroup"},
		principal:  "cognito-identity.amazonaws.com",
		paths:      []string{inputRoleARN},
	}},
}

// checkPassedRoles fails when a registered service's registry entry names an
// operation it lacks or a path that does not reach a string member, which would
// otherwise skip the iam:PassRole check without a trace.
func checkPassedRoles(services map[string]*catalog.Service) error {
	for name, entries := range passedRoleInputs {
		service, ok := services[name]
		if !ok {
			continue
		}

		for _, entry := range entries {
			for _, operation := range entry.operations {
				op, found := service.Operations[operation]
				if !found {
					return fmt.Errorf("iam:PassRole registry: %s has no operation %s", name, operation)
				}

				for _, path := range entry.paths {
					if !stringPath(reflect.TypeOf(op.NewInput()), strings.Split(path, ".")) {
						return fmt.Errorf("iam:PassRole registry: %s %s has no string member %s", name, operation, path)
					}
				}
			}
		}
	}

	return nil
}

// stringPath reports whether a member path, read as catalog.InputPathStrings
// reads it, ends at a string.
func stringPath(t reflect.Type, parts []string) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice ||
		(t.Kind() == reflect.Map && t.Key().Kind() == reflect.String) {
		t = t.Elem()
	}

	if len(parts) == 0 {
		return t.Kind() == reflect.String
	}

	if t.Kind() != reflect.Struct {
		return false
	}

	field, ok := t.FieldByName(parts[0])
	return ok && stringPath(field.Type, parts[1:])
}

func (s *Server) passedRoles(ctx context.Context, service, operation string, input any) (
	[]catalog.AuthorizationResource, error,
) {
	meta, _ := request.FromContext(ctx)
	var out []catalog.AuthorizationResource
	seen := map[string]bool{}
	for _, entry := range passedRoleInputs[service] {
		if !slices.Contains(entry.operations, operation) {
			continue
		}

		for _, path := range entry.paths {
			for _, role := range catalog.InputPathStrings(input, path) {
				if entry.profile {
					if s.instanceProfiles == nil {
						return nil, fmt.Errorf("instance profile %s lookup unavailable", role)
					}

					profile, found, err := s.instanceProfiles(ctx, meta.AccountID, role)
					if err != nil {
						return nil, err
					}

					if !found || profile.RoleARN == "" {
						return nil, fmt.Errorf("instance profile %s has no resolvable role", role)
					}

					role = profile.RoleARN
				}

				// A role can only be passed within its own account.
				if parts := strings.SplitN(role, ":", 6); len(parts) == 6 && parts[4] != meta.AccountID {
					return nil, fmt.Errorf("role %s belongs to another account", role)
				}

				key := entry.principal + ":" + role
				if seen[key] {
					continue
				}

				seen[key] = true
				out = append(out, catalog.AuthorizationResource{
					Action: "iam:PassRole", ARN: role,
					Values: map[string][]string{"iam:passedtoservice": {entry.principal}},
				})
			}
		}
	}

	return out, nil
}
