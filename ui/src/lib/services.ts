// Console names, categories, and icons for the services TinyStack registers.
// Icons are the official AWS Architecture Icons in public/aws-icons/.

export type ServiceInfo = {
	title: string;
	category: string;
	icon?: string;
};

const compute = "Compute";
const containers = "Containers";
const storage = "Storage";
const database = "Database";
const networking = "Networking & Content Delivery";
const integration = "Application Integration";
const analytics = "Analytics";
const iot = "Internet of Things";
const management = "Management & Governance";
const security = "Security, Identity & Compliance";
const business = "Business Applications";

export const categories = [
	compute,
	containers,
	storage,
	database,
	networking,
	integration,
	analytics,
	iot,
	management,
	security,
	business,
];

const catalog: Record<string, ServiceInfo> = {
	account: {
		title: "AWS Account",
		category: management,
		icon: "organizations",
	},
	appconfig: { title: "AppConfig", category: management, icon: "appconfig" },
	appconfigdata: {
		title: "AppConfig Data",
		category: management,
		icon: "appconfig",
	},
	wafv2: { title: "WAF v2", category: security, icon: "wafv2" },
	backup: { title: "AWS Backup", category: management, icon: "backup" },
	config: { title: "AWS Config", category: management, icon: "config" },
	cloudtrail: { title: "CloudTrail", category: management, icon: "cloudtrail" },
	vpc: { title: "VPC", category: networking, icon: "ec2" },
	acm: { title: "Certificate Manager", category: security, icon: "acm" },
	apigateway: {
		title: "API Gateway",
		category: networking,
		icon: "apigateway",
	},
	apigatewayv2: {
		title: "API Gateway (HTTP APIs)",
		category: networking,
		icon: "apigateway",
	},
	appsync: { title: "AppSync", category: integration, icon: "appsync" },
	athena: { title: "Athena", category: analytics, icon: "athena" },
	autoscaling: {
		title: "EC2 Auto Scaling",
		category: compute,
		icon: "autoscaling",
	},
	cloudcontrol: {
		title: "Cloud Control API",
		category: management,
		icon: "cloudformation",
	},
	cloudformation: {
		title: "CloudFormation",
		category: management,
		icon: "cloudformation",
	},
	cloudfront: { title: "CloudFront", category: networking, icon: "cloudfront" },
	cloudwatch: { title: "CloudWatch", category: management, icon: "cloudwatch" },
	cognitoidentity: {
		title: "Cognito Identity Pools",
		category: security,
		icon: "cognitoidp",
	},
	cognitoidp: { title: "Cognito", category: security, icon: "cognitoidp" },
	dynamodb: { title: "DynamoDB", category: database, icon: "dynamodb" },
	dynamodbstreams: {
		title: "DynamoDB Streams",
		category: database,
		icon: "dynamodb",
	},
	ec2: { title: "EC2", category: compute, icon: "ec2" },
	ecr: {
		title: "Elastic Container Registry",
		category: containers,
		icon: "ecr",
	},
	ecs: {
		title: "Elastic Container Service",
		category: containers,
		icon: "ecs",
	},
	eks: {
		title: "Elastic Kubernetes Service",
		category: containers,
		icon: "eks",
	},
	rds: {
		title: "Relational Database Service",
		category: database,
		icon: "rds",
	},
	elasticache: {
		title: "ElastiCache",
		category: database,
		icon: "elasticache",
	},
	elbv2: {
		title: "Elastic Load Balancing",
		category: networking,
		icon: "elbv2",
	},
	eventbridge: {
		title: "EventBridge",
		category: integration,
		icon: "eventbridge",
	},
	firehose: { title: "Data Firehose", category: analytics, icon: "firehose" },
	glue: { title: "Glue Data Catalog", category: analytics, icon: "glue" },
	iam: { title: "IAM", category: security, icon: "iam" },
	iot: { title: "IoT Core", category: iot, icon: "iot" },
	iotdataplane: { title: "IoT Data Plane", category: iot, icon: "iot" },
	iotjobsdataplane: {
		title: "IoT Jobs Data Plane",
		category: iot,
		icon: "iot",
	},
	kinesis: {
		title: "Kinesis Data Streams",
		category: analytics,
		icon: "kinesis",
	},
	kms: {
		title: "Key Management Service",
		category: security,
		icon: "kms",
	},
	lambda: { title: "Lambda", category: compute, icon: "lambda" },
	logs: { title: "CloudWatch Logs", category: management, icon: "logs" },
	pipes: {
		title: "EventBridge Pipes",
		category: integration,
		icon: "eventbridge",
	},
	organizations: {
		title: "Organizations",
		category: management,
		icon: "organizations",
	},
	servicediscovery: {
		title: "Cloud Map",
		category: networking,
		icon: "servicediscovery",
	},
	resourcegroups: { title: "Resource Groups", category: management },
	resourcegroupstaggingapi: {
		title: "Resource Groups Tagging API",
		category: management,
	},
	route53: { title: "Route 53", category: networking, icon: "route53" },
	s3: { title: "S3", category: storage, icon: "s3" },
	scheduler: {
		title: "EventBridge Scheduler",
		category: integration,
		icon: "eventbridge",
	},
	secretsmanager: {
		title: "Secrets Manager",
		category: security,
		icon: "secretsmanager",
	},
	ses: {
		title: "Simple Email Service (v1)",
		category: business,
		icon: "sesv2",
	},
	sesv2: {
		title: "Simple Email Service",
		category: business,
		icon: "sesv2",
	},
	sfn: { title: "Step Functions", category: integration, icon: "sfn" },
	sns: {
		title: "Simple Notification Service",
		category: integration,
		icon: "sns",
	},
	sqs: { title: "Simple Queue Service", category: integration, icon: "sqs" },
	ssm: {
		title: "Systems Manager",
		category: management,
		icon: "ssm",
	},
	sts: {
		title: "Security Token Service",
		category: security,
		icon: "iam",
	},
};

// service describes name, falling back to the bare name for a service this
// console does not know yet.
export function service(name: string): ServiceInfo {
	return catalog[name] ?? { title: name, category: "Other" };
}

// VPC is a console area backed by EC2, rather than a separate API service.
export function consoleServiceNames(names: string[]): string[] {
	return names.includes("ec2") && !names.includes("vpc")
		? [...names, "vpc"]
		: names;
}
