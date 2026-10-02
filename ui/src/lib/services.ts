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
const management = "Management & Governance";
const security = "Security, Identity & Compliance";

export const categories = [
	compute,
	containers,
	storage,
	database,
	networking,
	integration,
	analytics,
	management,
	security,
];

const catalog: Record<string, ServiceInfo> = {
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
	autoscaling: {
		title: "EC2 Auto Scaling",
		category: compute,
		icon: "autoscaling",
	},
	cloudformation: {
		title: "CloudFormation",
		category: management,
		icon: "cloudformation",
	},
	cloudwatch: { title: "CloudWatch", category: management, icon: "cloudwatch" },
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
	iam: { title: "IAM", category: security, icon: "iam" },
	kinesis: {
		title: "Kinesis Data Streams",
		category: analytics,
		icon: "kinesis",
	},
	kms: { title: "Key Management Service", category: security, icon: "kms" },
	lambda: { title: "Lambda", category: compute, icon: "lambda" },
	logs: { title: "CloudWatch Logs", category: management, icon: "logs" },
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
	sfn: { title: "Step Functions", category: integration, icon: "sfn" },
	sns: {
		title: "Simple Notification Service",
		category: integration,
		icon: "sns",
	},
	sqs: { title: "Simple Queue Service", category: integration, icon: "sqs" },
	ssm: { title: "Systems Manager", category: management, icon: "ssm" },
	sts: { title: "Security Token Service", category: security, icon: "iam" },
};

// service describes name, falling back to the bare name for a service this
// console does not know yet.
export function service(name: string): ServiceInfo {
	return catalog[name] ?? { title: name, category: "Other" };
}
