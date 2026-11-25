package iam

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"slices"
	"strings"
	"time"

	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceRolePath = "/service-role/"

// awsManagedEpoch is the standard creation timestamp for built-in AWS-managed policies.
var awsManagedEpoch = time.Date(2015, 2, 6, 18, 39, 46, 0, time.UTC).UnixMilli()

type awsManagedEntry struct {
	Path        string
	Name        string
	Description string
	Document    string
}

func (e *awsManagedEntry) arn() string {
	return arn(awsPartition, "policy"+e.Path+e.Name)
}

func (e *awsManagedEntry) id() string {
	sum := sha256.Sum256([]byte(e.arn()))
	return "ANPA" + base32.StdEncoding.EncodeToString(sum[:10])
}

func (e *awsManagedEntry) asPolicy(attachments, boundaries int64) *policy {
	return &policy{
		Name:           e.Name,
		ID:             e.id(),
		Path:           e.Path,
		Description:    e.Description,
		DefaultVersion: "v1",
		Versions:       []policyVersion{{ID: "v1", Document: e.Document, CreatedAt: awsManagedEpoch}},
		NextVersion:    2,
		Attachments:    attachments,
		BoundaryUses:   boundaries,
		CreatedAt:      awsManagedEpoch,
		UpdatedAt:      awsManagedEpoch,
	}
}

// These exact AWS-managed policy documents form TinyStack's local catalog.
// Unknown AWS-managed policies remain unavailable rather than being approximated
// with broad allows.
var awsManagedCatalog = []awsManagedEntry{
	// Core
	{
		Path:        "/",
		Name:        "AdministratorAccess",
		Description: "Provides full access to AWS services and resources.",
		Document:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "PowerUserAccess",
		Description: "Provides full access to AWS services and resources, but does not allow management of Users and groups.",
		Document: `{"Version":"2012-10-17","Statement":[` +
			`{"Effect":"Allow","NotAction":["iam:*","organizations:*","account:*"],"Resource":"*"},` +
			`{"Effect":"Allow","Action":["iam:CreateServiceLinkedRole","iam:DeleteServiceLinkedRole",` +
			`"iam:ListRoles","organizations:DescribeOrganization","account:ListRegions",` +
			`"account:GetAccountInformation","account:GetPrimaryEmail"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "ReadOnlyAccess",
		Description: "Provides read-only access to AWS services and resources.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[` +
			`"s3:Get*","s3:List*","s3:Describe*","ec2:Describe*","iam:Get*","iam:List*","iam:Generate*",` +
			`"dynamodb:Get*","dynamodb:List*","dynamodb:Describe*","dynamodb:BatchGetItem","dynamodb:Query","dynamodb:Scan",` +
			`"sqs:GetQueueAttributes","sqs:GetQueueUrl","sqs:List*","sns:Get*","sns:List*",` +
			`"lambda:Get*","lambda:List*","logs:Describe*","logs:Get*","logs:FilterLogEvents",` +
			`"cloudwatch:Describe*","cloudwatch:Get*","cloudwatch:List*","events:Describe*","events:List*",` +
			`"states:Describe*","states:List*","states:GetExecutionHistory","rds:Describe*","rds:ListTagsForResource",` +
			`"secretsmanager:DescribeSecret","secretsmanager:ListSecrets","ssm:Describe*","ssm:Get*","ssm:List*",` +
			`"kms:Describe*","kms:Get*","kms:List*","ecr:Describe*","ecr:Get*","ecr:List*","ecr:BatchGetImage",` +
			`"ecs:Describe*","ecs:List*","eks:Describe*","eks:List*","kinesis:Describe*","kinesis:Get*","kinesis:List*",` +
			`"firehose:Describe*","firehose:List*","cloudformation:Describe*","cloudformation:Get*","cloudformation:List*",` +
			`"route53:Get*","route53:List*","sts:GetCallerIdentity","sts:GetAccessKeyInfo","tag:Get*"` +
			`],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "ViewOnlyAccess",
		Description: "Grants permissions to view resources and basic metadata across all AWS services.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[` +
			`"s3:ListAllMyBuckets","s3:GetBucketLocation","s3:ListBucket","ec2:Describe*","iam:Get*","iam:List*",` +
			`"dynamodb:ListTables","dynamodb:DescribeTable","sqs:ListQueues","sqs:GetQueueAttributes",` +
			`"sns:ListTopics","sns:ListSubscriptions","sns:GetTopicAttributes",` +
			`"lambda:ListFunctions","lambda:GetFunctionConfiguration",` +
			`"logs:DescribeLogGroups","logs:DescribeLogStreams","cloudwatch:Describe*","cloudwatch:ListMetrics",` +
			`"events:ListRules","events:DescribeRule","states:ListStateMachines","states:DescribeStateMachine",` +
			`"rds:Describe*","secretsmanager:ListSecrets","ssm:DescribeParameters","kms:ListKeys","kms:ListAliases",` +
			`"ecs:List*","ecs:Describe*","eks:List*","eks:Describe*","cloudformation:Describe*","cloudformation:List*"` +
			`],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "IAMFullAccess",
		Description: "Provides full access to IAM via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["iam:*","organizations:DescribeAccount","organizations:DescribeOrganization",` +
			`"organizations:DescribeOrganizationalUnit","organizations:DescribePolicy","organizations:ListChildren",` +
			`"organizations:ListParents","organizations:ListPoliciesForTarget","organizations:ListRoots",` +
			`"organizations:ListPolicies","organizations:ListTargetsForPolicy"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "IAMReadOnlyAccess",
		Description: "Provides read-only access to IAM via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["iam:GenerateCredentialReport","iam:GenerateServiceLastAccessedDetails","iam:Get*",` +
			`"iam:List*","iam:SimulateCustomPolicy","iam:SimulatePrincipalPolicy"],"Resource":"*"}]}`,
	},

	// Storage & Data
	{
		Path:        "/",
		Name:        "AmazonS3FullAccess",
		Description: "Provides full access to all buckets via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["s3:*","s3-object-lambda:*"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonS3ReadOnlyAccess",
		Description: "Provides read only access to all buckets via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["s3:Get*","s3:List*","s3:Describe*","s3-object-lambda:Get*","s3-object-lambda:List*"],` +
			`"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonDynamoDBFullAccess",
		Description: "Provides full access to Amazon DynamoDB via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["dynamodb:*","dax:*","application-autoscaling:DeleteScalingPolicy",` +
			`"application-autoscaling:DeregisterScalableTarget","application-autoscaling:DescribeScalableTargets",` +
			`"application-autoscaling:DescribeScalingActivities","application-autoscaling:DescribeScalingPolicies",` +
			`"application-autoscaling:PutScalingPolicy","application-autoscaling:RegisterScalableTarget",` +
			`"cloudwatch:DeleteAlarms","cloudwatch:DescribeAlarmHistory","cloudwatch:DescribeAlarms",` +
			`"cloudwatch:DescribeAlarmsForMetric","cloudwatch:GetMetricStatistics","cloudwatch:ListMetrics",` +
			`"cloudwatch:PutMetricAlarm","datapipeline:ActivatePipeline","datapipeline:CreatePipeline",` +
			`"datapipeline:DeletePipeline","datapipeline:DescribeObjects","datapipeline:DescribePipelines",` +
			`"datapipeline:GetPipelineDefinition","datapipeline:ListPipelines","datapipeline:PutPipelineDefinition",` +
			`"datapipeline:QueryObjects","ec2:DescribeVpcs","ec2:DescribeSubnets","ec2:DescribeSecurityGroups",` +
			`"iam:GetRole","iam:ListRoles","kms:DescribeKey","kms:ListAliases","sns:CreateTopic","sns:DeleteTopic",` +
			`"sns:ListSubscriptions","sns:ListSubscriptionsByTopic","sns:ListTopics","sns:Subscribe","sns:Unsubscribe",` +
			`"sns:SetTopicAttributes","lambda:CreateFunction","lambda:ListFunctions","lambda:ListEventSourceMappings",` +
			`"lambda:CreateEventSourceMapping","lambda:DeleteEventSourceMapping","lambda:GetFunctionConfiguration",` +
			`"lambda:DeleteFunction","resource-groups:ListGroups","resource-groups:ListGroupResources",` +
			`"resource-groups:GetGroup","resource-groups:GetGroupQuery","resource-groups:DeleteGroup",` +
			`"resource-groups:CreateGroup","tag:GetResources"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonDynamoDBReadOnlyAccess",
		Description: "Provides read only access to Amazon DynamoDB via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["application-autoscaling:DescribeScalableTargets","application-autoscaling:DescribeScalingActivities",` +
			`"application-autoscaling:DescribeScalingPolicies","cloudwatch:DescribeAlarmHistory","cloudwatch:DescribeAlarms",` +
			`"cloudwatch:DescribeAlarmsForMetric","cloudwatch:GetMetricStatistics","cloudwatch:ListMetrics",` +
			`"cloudwatch:GetMetricData","datapipeline:DescribeObjects","datapipeline:DescribePipelines",` +
			`"datapipeline:GetPipelineDefinition","datapipeline:ListPipelines","datapipeline:QueryObjects",` +
			`"dynamodb:BatchGetItem","dynamodb:Describe*","dynamodb:List*","dynamodb:GetItem","dynamodb:GetResourcePolicy",` +
			`"dynamodb:Query","dynamodb:Scan","dynamodb:PartiQLSelect","dax:Describe*","dax:List*","dax:GetItem",` +
			`"dax:BatchGetItem","dax:Query","dax:Scan","ec2:DescribeVpcs","ec2:DescribeSubnets","ec2:DescribeSecurityGroups",` +
			`"iam:GetRole","iam:ListRoles","kms:DescribeKey","kms:ListAliases","sns:ListSubscriptionsByTopic",` +
			`"sns:ListTopics","lambda:ListFunctions","lambda:ListEventSourceMappings","lambda:GetFunctionConfiguration",` +
			`"resource-groups:ListGroups","resource-groups:ListGroupResources","resource-groups:GetGroup",` +
			`"resource-groups:GetGroupQuery","tag:GetResources"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonRDSFullAccess",
		Description: "Provides full access to Amazon RDS via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["rds:*","application-autoscaling:DeleteScalingPolicy",` +
			`"application-autoscaling:DeregisterScalableTarget","application-autoscaling:DescribeScalableTargets",` +
			`"application-autoscaling:DescribeScalingActivities","application-autoscaling:DescribeScalingPolicies",` +
			`"application-autoscaling:PutScalingPolicy","application-autoscaling:RegisterScalableTarget",` +
			`"cloudwatch:DescribeAlarms","cloudwatch:GetMetricStatistics","cloudwatch:PutMetricAlarm",` +
			`"cloudwatch:DeleteAlarms","ec2:DescribeAccountAttributes","ec2:DescribeAvailabilityZones",` +
			`"ec2:DescribeCoipPools","ec2:DescribeInternetGateways","ec2:DescribeLocalGatewayRouteTables",` +
			`"ec2:DescribeLocalGatewayRouteTableVpcAssociations","ec2:DescribeLocalGateways","ec2:DescribeSecurityGroups",` +
			`"ec2:DescribeSubnets","ec2:DescribeVpcAttribute","ec2:DescribeVpcs","ec2:GetCoipPoolUsage",` +
			`"sns:ListSubscriptions","sns:ListTopics","sns:Publish","logs:DescribeLogStreams","logs:GetLogEvents",` +
			`"outposts:GetOutpostInstanceTypes"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "SecretsManagerReadWrite",
		Description: "Provides full access to AWS Secrets Manager.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["secretsmanager:*","kms:ListAliases","kms:ListKeys","lambda:ListFunctions","rds:DescribeDBClusters",` +
			`"rds:DescribeDBInstances","redshift:DescribeClusters","tag:GetResources"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonSSMReadOnlyAccess",
		Description: "Provides read-only access to Amazon SSM.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["ssm:Describe*","ssm:Get*","ssm:List*"],"Resource":"*"}]}`,
	},

	// Messaging & Events
	{
		Path:        "/",
		Name:        "AmazonSQSFullAccess",
		Description: "Provides full access to Amazon SQS via the AWS Management Console.",
		Document:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sqs:*","Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonSQSReadOnlyAccess",
		Description: "Provides read only access to Amazon SQS via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["sqs:GetQueueAttributes","sqs:GetQueueUrl","sqs:ListDeadLetterSourceQueues",` +
			`"sqs:ListQueues","sqs:ListQueueTags","sqs:ListMessageMoveTasks"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonSNSFullAccess",
		Description: "Provides full access to Amazon SNS via the AWS Management Console.",
		Document:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sns:*","Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonSNSReadOnlyAccess",
		Description: "Provides read only access to Amazon SNS via the AWS Management Console.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["sns:GetTopicAttributes","sns:List*"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonEventBridgeFullAccess",
		Description: "Provides full access to Amazon EventBridge.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["events:*","schemas:*","scheduler:*","pipes:*","iam:ListRoles","iam:PassRole",` +
			`"kms:DescribeKey","kms:ListKeys","kms:ListAliases","logs:DescribeLogGroups","logs:DescribeLogStreams",` +
			`"sns:ListTopics","sqs:ListQueues","lambda:ListFunctions","states:ListStateMachines"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonKinesisFullAccess",
		Description: "Provides full access to Amazon Kinesis via the AWS Management Console.",
		Document:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"kinesis:*","Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonKinesisFirehoseFullAccess",
		Description: "Provides full access to Amazon Kinesis Firehose via the AWS Management Console.",
		Document:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"firehose:*","Resource":"*"}]}`,
	},

	// Compute & Workflows
	{
		Path:        "/",
		Name:        "AWSLambda_FullAccess",
		Description: "Grants full access to AWS Lambda service, AWS Lambda console features, and other related AWS services.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["cloudformation:DescribeStacks","cloudformation:ListStackResources","cloudwatch:ListMetrics",` +
			`"cloudwatch:GetMetricData","ec2:DescribeSecurityGroups","ec2:DescribeSubnets","ec2:DescribeVpcs",` +
			`"kms:ListAliases","iam:GetPolicy","iam:GetPolicyVersion","iam:GetRole","iam:GetRolePolicy",` +
			`"iam:ListAttachedRolePolicies","iam:ListRolePolicies","iam:ListRoles","lambda:*","logs:DescribeLogGroups",` +
			`"states:DescribeStateMachine","states:ListStateMachines","tag:GetResources","xray:GetTraceSummaries",` +
			`"xray:BatchGetTraces"],"Resource":"*"},` +
			`{"Effect":"Allow","Action":"iam:PassRole","Resource":"*","Condition":` +
			`{"StringEquals":{"iam:PassedToService":"lambda.amazonaws.com"}}},` +
			`{"Effect":"Allow","Action":["logs:DescribeLogStreams","logs:GetLogEvents","logs:FilterLogEvents",` +
			`"logs:StartLiveTail"],"Resource":"arn:aws:logs:*:*:log-group:/aws/lambda/*"}]}`,
	},
	{
		Path:        serviceRolePath,
		Name:        "AWSLambdaBasicExecutionRole",
		Description: "Provides write permissions to CloudWatch Logs.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`,
	},
	{
		Path:        serviceRolePath,
		Name:        "AWSLambdaVPCAccessExecutionRole",
		Description: "Provides minimum permissions for a Lambda function to execute while accessing a resource within a VPC.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents",` +
			`"ec2:CreateNetworkInterface","ec2:DescribeNetworkInterfaces","ec2:DescribeSubnets",` +
			`"ec2:DeleteNetworkInterface","ec2:AssignPrivateIpAddresses","ec2:UnassignPrivateIpAddresses"],` +
			`"Resource":"*"}]}`,
	},
	{
		Path: serviceRolePath,
		Name: "AWSLambdaSQSQueueExecutionRole",
		Description: "Provides receive message, delete message, and read attribute access to SQS queues, " +
			"and write permissions to CloudWatch logs.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["sqs:ReceiveMessage","sqs:DeleteMessage","sqs:GetQueueAttributes",` +
			`"logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`,
	},
	{
		Path:        serviceRolePath,
		Name:        "AWSLambdaDynamoDBExecutionRole",
		Description: "Provides list and read access to DynamoDB streams and write permissions to CloudWatch logs.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["dynamodb:DescribeStream","dynamodb:GetRecords","dynamodb:GetShardIterator",` +
			`"dynamodb:ListStreams","logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`,
	},
	{
		Path:        serviceRolePath,
		Name:        "AWSLambdaKinesisExecutionRole",
		Description: "Provides list and read access to Kinesis streams and write permissions to CloudWatch logs.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["kinesis:DescribeStream","kinesis:DescribeStreamSummary","kinesis:GetRecords",` +
			`"kinesis:GetShardIterator","kinesis:ListShards","kinesis:ListStreams","kinesis:SubscribeToShard",` +
			`"logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`,
	},
	{
		Path:        serviceRolePath,
		Name:        "AmazonECSTaskExecutionRolePolicy",
		Description: "Provides access to other AWS service resources that are required to run Amazon ECS tasks.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["ecr:GetAuthorizationToken","ecr:BatchCheckLayerAvailability","ecr:GetDownloadUrlForLayer",` +
			`"ecr:BatchGetImage","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonECS_FullAccess",
		Description: "Provides full access to Amazon ECS and related services.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["ecs:*","ecr:DescribeRepositories","ecr:ListImages","ec2:Describe*","elasticloadbalancing:Describe*",` +
			`"autoscaling:Describe*","cloudwatch:DescribeAlarms","cloudwatch:GetMetricStatistics","logs:Describe*",` +
			`"logs:GetLogEvents","iam:ListRoles","iam:PassRole",` +
			`"application-autoscaling:*","servicediscovery:*"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonEC2ContainerRegistryReadOnly",
		Description: "Provides read-only access to Amazon EC2 Container Registry repositories.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["ecr:GetAuthorizationToken","ecr:BatchCheckLayerAvailability","ecr:GetDownloadUrlForLayer",` +
			`"ecr:GetRepositoryPolicy","ecr:DescribeRepositories","ecr:ListImages","ecr:DescribeImages",` +
			`"ecr:BatchGetImage","ecr:GetLifecyclePolicy","ecr:GetLifecyclePolicyPreview","ecr:ListTagsForResource",` +
			`"ecr:DescribeImageScanFindings"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonEKSClusterPolicy",
		Description: "Provides Kubernetes the permissions it requires to manage resources on your behalf.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["ec2:DescribeAccountAttributes","ec2:DescribeAddresses","ec2:DescribeInternetGateways",` +
			`"ec2:DescribeSecurityGroups","ec2:DescribeSubnets","ec2:DescribeVpcs","ec2:DescribeInstances",` +
			`"ec2:DescribeNetworkInterfaces","ec2:CreateNetworkInterface","ec2:DeleteNetworkInterface",` +
			`"ec2:AttachNetworkInterface","ec2:DetachNetworkInterface","ec2:ModifyNetworkInterfaceAttribute",` +
			`"elasticloadbalancing:*","autoscaling:DescribeAutoScalingGroups","cloudwatch:PutMetricData"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AmazonEKSWorkerNodePolicy",
		Description: "Allows Amazon EKS worker nodes to connect to Amazon EKS Clusters.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["ec2:DescribeInstances","ec2:DescribeInstanceTypes","ec2:DescribeRouteTables",` +
			`"ec2:DescribeSecurityGroups","ec2:DescribeSubnets","ec2:DescribeVolumes","ec2:DescribeVolumesModifications",` +
			`"ec2:DescribeVpcs","eks:DescribeCluster","eks-auth:AssumeRoleForPodIdentity"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "AWSStepFunctionsFullAccess",
		Description: "Provides full access to AWS Step Functions.",
		Document:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"states:*","Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "CloudWatchFullAccess",
		Description: "Provides full access to CloudWatch.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["autoscaling:Describe*","cloudwatch:*","logs:*","sns:*","iam:GetPolicy","iam:GetPolicyVersion",` +
			`"iam:GetRole","oam:ListSinks"],"Resource":"*"}]}`,
	},
	{
		Path:        "/",
		Name:        "CloudWatchLogsFullAccess",
		Description: "Provides full access to CloudWatch Logs.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["logs:*","cloudwatch:GenerateQuery"],"Resource":"*"}]}`,
	},
	{
		Path: serviceRolePath,
		Name: "AWSGlueServiceRole",
		Description: "Policy for AWS Glue service role which allows access to related services " +
			"including EC2, S3, and Cloudwatch Logs.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["glue:*","s3:GetBucketLocation","s3:ListBucket","s3:ListAllMyBuckets","s3:GetBucketAcl",` +
			`"ec2:DescribeVpcEndpoints","ec2:DescribeRouteTables","ec2:CreateNetworkInterface","ec2:DeleteNetworkInterface",` +
			`"ec2:DescribeNetworkInterfaces","ec2:DescribeSecurityGroups","ec2:DescribeSubnets",` +
			`"ec2:DescribeVpcAttribute","iam:ListRolePolicies","iam:GetRole","iam:GetRolePolicy",` +
			`"cloudwatch:PutMetricData","logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`,
	},
	{
		Path:        serviceRolePath,
		Name:        "AWSBatchServiceRole",
		Description: "Allows AWS Batch to create and manage AWS resources on your behalf.",
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["ec2:DescribeAccountAttributes","ec2:DescribeInstances","ec2:DescribeInstanceAttribute",` +
			`"ec2:DescribeSubnets","ec2:DescribeSecurityGroups","ec2:DescribeKeyPairs","ec2:DescribeImages",` +
			`"ec2:DescribeImageAttribute","ec2:DescribeSpotInstanceRequests","ec2:DescribeSpotFleetInstances",` +
			`"ec2:DescribeSpotFleetRequests","ec2:DescribeSpotPriceHistory","ec2:DescribeVpcClassicLink",` +
			`"ec2:DescribeLaunchTemplateVersions","ec2:CreateLaunchTemplate","ec2:DeleteLaunchTemplate",` +
			`"ec2:RequestSpotFleet","ec2:CancelSpotFleetRequests","ec2:ModifySpotFleetRequest",` +
			`"ec2:TerminateInstances","ec2:RunInstances","autoscaling:DescribeAccountLimits",` +
			`"autoscaling:DescribeAutoScalingGroups","autoscaling:DescribeLaunchConfigurations",` +
			`"autoscaling:DescribeAutoScalingInstances","autoscaling:CreateLaunchConfiguration",` +
			`"autoscaling:CreateAutoScalingGroup","autoscaling:UpdateAutoScalingGroup","autoscaling:SetDesiredCapacity",` +
			`"autoscaling:DeleteLaunchConfiguration","autoscaling:DeleteAutoScalingGroup","autoscaling:CreateOrUpdateTags",` +
			`"autoscaling:SuspendProcesses","autoscaling:PutNotificationConfiguration",` +
			`"autoscaling:TerminateInstanceInAutoScalingGroup",` +
			`"ecs:DescribeClusters","ecs:DescribeContainerInstances","ecs:DescribeTaskDefinition","ecs:DescribeTasks",` +
			`"ecs:ListAccountSettings","ecs:ListClusters","ecs:ListContainerInstances","ecs:ListTaskDefinitionFamilies",` +
			`"ecs:ListTaskDefinitions","ecs:ListTasks","ecs:CreateCluster","ecs:DeleteCluster",` +
			`"ecs:RegisterTaskDefinition","ecs:DeregisterTaskDefinition","ecs:RunTask","ecs:StartTask","ecs:StopTask",` +
			`"ecs:UpdateContainerAgent","ecs:DeregisterContainerInstance","logs:CreateLogGroup","logs:CreateLogStream",` +
			`"logs:PutLogEvents","logs:DescribeLogGroups","iam:GetInstanceProfile","iam:GetRole"],"Resource":"*"}]}`,
	},
}

var awsManagedIndex = func() map[string]*awsManagedEntry {
	idx := make(map[string]*awsManagedEntry, len(awsManagedCatalog))
	for i := range awsManagedCatalog {
		e := &awsManagedCatalog[i]
		idx[strings.ToLower(e.Path+e.Name)] = e
	}

	return idx
}()

func lookupAWSManaged(ref policyRef) (*awsManagedEntry, bool) {
	if !ref.awsManaged() {
		return nil, false
	}

	e, ok := awsManagedIndex[strings.ToLower(ref.path+ref.name)]
	return e, ok
}

// policyUsageCounts scans users, groups, and roles in account to compute live
// attachment and permissions-boundary usage counts keyed by lowercased policy ARN.
func policyUsageCounts(tx storage.ReadTx, account string) (attachments, boundaries map[string]int64, err error) {
	attachments = map[string]int64{}
	boundaries = map[string]int64{}

	users, err := list[user](tx, account, kindUser)
	if err != nil {
		return nil, nil, err
	}

	for _, u := range users {
		for _, a := range u.Attached {
			attachments[strings.ToLower(a)]++
		}

		if u.Boundary != "" {
			boundaries[strings.ToLower(u.Boundary)]++
		}
	}

	groups, err := list[group](tx, account, kindGroup)
	if err != nil {
		return nil, nil, err
	}

	for _, g := range groups {
		for _, a := range g.Attached {
			attachments[strings.ToLower(a)]++
		}
	}

	roles, err := list[role](tx, account, kindRole)
	if err != nil {
		return nil, nil, err
	}

	for _, r := range roles {
		for _, a := range r.Attached {
			attachments[strings.ToLower(a)]++
		}

		if r.Boundary != "" {
			boundaries[strings.ToLower(r.Boundary)]++
		}
	}

	return attachments, boundaries, nil
}

func awsManagedPolicies(tx storage.ReadTx, account string) ([]*policy, error) {
	attachments, boundaries, err := policyUsageCounts(tx, account)
	if err != nil {
		return nil, err
	}

	out := make([]*policy, 0, len(awsManagedCatalog))
	for i := range awsManagedCatalog {
		e := &awsManagedCatalog[i]
		k := strings.ToLower(e.arn())
		out = append(out, e.asPolicy(attachments[k], boundaries[k]))
	}

	slices.SortFunc(out, func(a, b *policy) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}

		return strings.Compare(a.Path, b.Path)
	})
	return out, nil
}

func managedDocument(tx storage.ReadTx, account, policyARN string) (iampolicy.Document, error) {
	ref, err := parsePolicyARN(policyARN)
	if err != nil {
		return iampolicy.Document{}, err
	}

	if ref.awsManaged() {
		entry, exists := lookupAWSManaged(ref)
		if !exists {
			return iampolicy.Document{}, noSuchEntity(
				"Policy %s does not exist in TinyStack's AWS-managed catalog.",
				policyARN,
			)
		}

		return iampolicy.Document{Name: entry.arn(), JSON: entry.Document, SourceType: "aws-managed"}, nil
	}

	managed, _, err := loadPolicy(tx, account, ref)
	if err != nil {
		return iampolicy.Document{}, err
	}

	for _, version := range managed.Versions {
		if version.ID == managed.DefaultVersion {
			return iampolicy.Document{
				Name:       policyARN + "/" + version.ID,
				JSON:       version.Document,
				SourceType: "user-managed",
			}, nil
		}
	}

	return iampolicy.Document{}, fmt.Errorf("managed policy %s has no default document", policyARN)
}

// ValidateSessionPolicies shares IAM's identity-document validator and requires
// every managed session policy to exist in the role account (or local AWS catalog).
func (s *Service) ValidateSessionPolicies(ctx context.Context, account, inline string, arns []string) error {
	if inline != "" {
		if err := checkDocument("policyDocument", &inline); err != nil {
			return err
		}
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, arn := range arns {
			if _, err := managedDocument(tx, account, arn); err != nil {
				return err
			}
		}

		return nil
	})
}

func collectLimits(tx storage.ReadTx, account, boundary string, out *Authorization) error {
	if boundary != "" {
		document, err := managedDocument(tx, account, boundary)
		if err != nil {
			return fmt.Errorf("permissions boundary: %w", err)
		}

		out.Boundary = []iampolicy.Document{document}
	}

	p := &out.Principal
	if p.SessionPolicy != "" {
		out.Session = append(out.Session, iampolicy.Document{Name: "session-policy:inline", JSON: p.SessionPolicy})
	}

	for _, arn := range p.SessionPolicyARNs {
		document, err := managedDocument(tx, account, arn)
		if err != nil {
			return fmt.Errorf("session policy: %w", err)
		}

		out.Session = append(out.Session, document)
	}

	return nil
}
