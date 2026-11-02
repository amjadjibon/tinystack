// Console names, categories, and icons for the services TinyStack registers.
// Icons are the official AWS Architecture Icons in public/aws-icons/.

export type ServiceInfo = {
	title: string;
	category: string;
	icon?: string;
	href?: string;
};

const machineLearning = "Machine Learning";
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
const developerTools = "Developer Tools";
const mediaServices = "Media Services";
const migration = "Migration & Modernization";
const business = "Business Applications";
const frontend = "Front-End Web & Mobile";

export const categories = [
	machineLearning,
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
	developerTools,
	mediaServices,
	migration,
	frontend,
];

const catalog: Record<string, ServiceInfo> = {
	translate: {
		title: "Amazon Translate",
		category: machineLearning,
		icon: "translate",
	},
	transcribe: {
		title: "Amazon Transcribe",
		category: machineLearning,
		icon: "transcribe",
	},
	mediaconvert: {
		title: "AWS Elemental MediaConvert",
		category: mediaServices,
		icon: "mediaconvert",
	},
	swf: {
		title: "Amazon Simple Workflow Service",
		category: integration,
		icon: "swf",
	},
	mediaconnect: {
		title: "AWS Elemental MediaConnect",
		category: mediaServices,
		icon: "mediaconnect",
	},
	datasync: { title: "AWS DataSync", category: migration, icon: "datasync" },
	resourceexplorer2: {
		title: "AWS Resource Explorer",
		category: management,
		icon: "resourceexplorer2",
	},
	servicecatalog: {
		title: "Service Catalog",
		category: management,
		icon: "servicecatalog",
	},
	globalaccelerator: {
		title: "Global Accelerator",
		category: networking,
		icon: "globalaccelerator",
	},
	shield: { title: "AWS Shield Advanced", category: security, icon: "shield" },
	dsql: { title: "Amazon Aurora DSQL", category: database, icon: "dsql" },
	bedrockagentcorecontrol: {
		title: "Amazon Bedrock AgentCore",
		category: machineLearning,
		icon: "bedrockagentcore",
	},
	bedrockagentcore: {
		title: "Amazon Bedrock AgentCore Data Plane",
		category: machineLearning,
		icon: "bedrockagentcore",
	},
	fis: {
		title: "Fault Injection Service",
		category: management,
		icon: "fis",
	},
	accessanalyzer: {
		title: "IAM Access Analyzer",
		category: security,
		icon: "accessanalyzer",
	},
	inspector2: {
		title: "Amazon Inspector",
		category: security,
		icon: "inspector2",
	},
	securityhub: {
		title: "AWS Security Hub",
		category: security,
		icon: "securityhub",
	},
	amp: {
		title: "Amazon Managed Service for Prometheus",
		category: management,
		icon: "cloudwatch",
	},
	bedrockagent: {
		title: "Bedrock Agents",
		category: machineLearning,
		icon: "bedrockagent",
	},
	bedrockagentruntime: {
		title: "Bedrock Agent Runtime",
		category: machineLearning,
		icon: "bedrockagentruntime",
	},
	bedrock: {
		title: "Amazon Bedrock",
		category: machineLearning,
		icon: "bedrock",
	},
	bedrockruntime: {
		title: "Amazon Bedrock Runtime",
		category: machineLearning,
		icon: "bedrock",
	},
	databasemigrationservice: {
		title: "AWS Database Migration Service",
		category: migration,
		icon: "rds",
	},
	elasticbeanstalk: {
		title: "Elastic Beanstalk",
		category: compute,
		icon: "ec2",
	},
	signer: { title: "AWS Signer", category: security, icon: "acm" },
	kinesisanalyticsv2: {
		title: "Amazon Managed Service for Apache Flink",
		category: analytics,
		icon: "kinesis",
	},
	costexplorer: {
		title: "AWS Cost Explorer",
		category: management,
		icon: "costexplorer",
	},
	budgets: { title: "AWS Budgets", category: management, icon: "budgets" },
	emr: { title: "Amazon EMR", category: analytics, icon: "glue" },
	emrserverless: {
		title: "Amazon EMR Serverless",
		category: analytics,
		icon: "glue",
	},
	identitystore: {
		title: "Identity Center — Identity Store",
		category: security,
		icon: "identitysso",
	},
	ssoadmin: {
		title: "Identity Center — SSO Admin",
		category: security,
		icon: "identitysso",
	},
	memorydb: { title: "Amazon MemoryDB", category: database, icon: "memorydb" },
	redshiftdata: {
		title: "Amazon Redshift Data API",
		category: analytics,
		icon: "redshift",
	},
	redshiftserverless: {
		title: "Amazon Redshift Serverless",
		category: analytics,
		icon: "redshift",
	},
	redshift: { title: "Amazon Redshift", category: analytics, icon: "redshift" },
	s3tables: { title: "Amazon S3 Tables", category: storage, icon: "s3" },
	s3files: { title: "Amazon S3 Files", category: storage, icon: "s3" },
	glacier: { title: "Amazon S3 Glacier", category: storage, icon: "glacier" },
	location: {
		title: "Amazon Location Service",
		category: frontend,
		icon: "location",
	},
	transfer: {
		title: "AWS Transfer Family",
		category: migration,
		icon: "transfer",
	},
	mwaa: { title: "Amazon MWAA", category: integration, icon: "mwaa" },
	mq: { title: "Amazon MQ", category: integration, icon: "mq" },
	kafka: { title: "Amazon MSK", category: analytics, icon: "msk" },
	account: {
		title: "AWS Account",
		category: management,
		icon: "organizations",
	},
	appconfig: {
		title: "AppConfig",
		category: management,
		icon: "appconfig",
		href: "/services/appconfig",
	},
	appconfigdata: {
		title: "AppConfig Data",
		category: management,
		icon: "appconfig",
	},
	waf: { title: "WAF Classic", category: security, icon: "wafv2" },
	wafv2: { title: "WAF v2", category: security, icon: "wafv2" },
	codebuild: {
		title: "CodeBuild",
		category: developerTools,
		icon: "codebuild",
	},
	costandusagereportservice: {
		title: "Cost and Usage Reports",
		category: management,
		icon: "costandusagereportservice",
	},
	servicequotas: {
		title: "Service Quotas",
		category: management,
		icon: "servicequotas",
	},
	lakeformation: {
		title: "Lake Formation",
		category: analytics,
		icon: "lakeformation",
	},
	apprunner: {
		title: "App Runner",
		category: compute,
		icon: "apprunner",
	},
	amplify: {
		title: "Amplify Hosting",
		category: developerTools,
		icon: "amplify",
	},
	codeconnections: {
		title: "CodeConnections",
		category: developerTools,
		icon: "codeconnections",
	},
	timestreamwrite: {
		title: "Amazon Timestream",
		category: database,
		icon: "timestreamwrite",
	},
	timestreamquery: {
		title: "Amazon Timestream Query",
		category: database,
		icon: "timestreamquery",
	},
	codeartifact: {
		title: "CodeArtifact",
		category: developerTools,
		icon: "codeartifact",
	},
	codecommit: {
		title: "CodeCommit",
		category: developerTools,
		icon: "codecommit",
	},
	opensearchserverless: {
		title: "OpenSearch Serverless",
		category: analytics,
		icon: "opensearch",
	},
	opensearch: {
		title: "OpenSearch Service",
		category: analytics,
		icon: "opensearch",
	},
	codepipeline: {
		title: "CodePipeline",
		category: developerTools,
		icon: "codepipeline",
	},
	codedeploy: {
		title: "AWS CodeDeploy",
		category: developerTools,
		icon: "codedeploy",
	},
	xray: {
		title: "AWS X-Ray",
		category: developerTools,
		icon: "xray",
	},
	batch: { title: "AWS Batch", category: compute, icon: "batch" },
	efs: {
		title: "Elastic File System",
		category: storage,
		icon: "efs",
		href: "/services/efs",
	},
	backup: { title: "AWS Backup", category: management, icon: "backup" },
	config: { title: "AWS Config", category: management, icon: "config" },
	cloudtrail: {
		href: "/services/cloudtrail",
		title: "CloudTrail",
		category: management,
		icon: "cloudtrail",
	},
	vpc: { title: "VPC", category: networking, icon: "ec2" },
	route53resolver: {
		title: "Route 53 Resolver",
		category: networking,
		icon: "route53",
	},
	ram: {
		title: "Resource Access Manager",
		category: security,
		icon: "ram",
	},
	acm: { title: "Certificate Manager", category: security, icon: "acm" },
	acmpca: {
		title: "Private CA",
		category: security,
		icon: "acmpca",
	},
	verifiedpermissions: {
		title: "Amazon Verified Permissions",
		category: security,
		icon: "verifiedpermissions",
	},
	networkfirewall: {
		title: "AWS Network Firewall",
		category: security,
		icon: "networkfirewall",
	},
	apigateway: {
		title: "API Gateway",
		category: networking,
		icon: "apigateway",
	},
	apigatewaymanagementapi: {
		title: "API Gateway Management",
		category: networking,
		icon: "apigateway",
	},
	apigatewayv2: {
		title: "API Gateway (HTTP and WebSocket APIs)",
		category: networking,
		icon: "apigateway",
	},
	appsync: {
		title: "AppSync GraphQL & Events",
		category: integration,
		icon: "appsync",
	},
	athena: {
		title: "Athena",
		category: analytics,
		icon: "athena",
		href: "/services/athena",
	},
	applicationautoscaling: {
		title: "Application Auto Scaling",
		category: management,
		icon: "applicationautoscaling",
	},
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
	cloudfrontkeyvaluestore: {
		title: "CloudFront KeyValueStore",
		category: networking,
		icon: "cloudfrontkeyvaluestore",
	},
	cloudwatch: { title: "CloudWatch", category: management, icon: "cloudwatch" },
	cognitoidentity: {
		href: "/services/cognitoidentity",
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
		title: "Elastic Container Registry (ECR)",
		category: containers,
		icon: "ecr",
	},
	ecs: {
		title: "Elastic Container Service (ECS)",
		category: containers,
		icon: "ecs",
	},
	eks: {
		title: "Elastic Kubernetes Service",
		category: containers,
		icon: "eks",
	},
	rdsdata: {
		title: "RDS Data API",
		category: database,
		icon: "rds",
	},
	rds: {
		title: "RDS, DocumentDB and Neptune",
		category: database,
		icon: "rds",
	},
	elasticache: {
		title: "ElastiCache",
		category: database,
		icon: "elasticache",
	},
	elb: {
		title: "Elastic Load Balancing (Classic)",
		category: networking,
		icon: "elb",
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
	firehose: {
		title: "Data Firehose",
		category: analytics,
		icon: "firehose",
		href: "/services/firehose",
	},
	glue: {
		href: "/services/glue",
		title: "AWS Glue",
		category: analytics,
		icon: "glue",
	},
	iam: { title: "IAM", category: security, icon: "iam" },
	iot: { title: "IoT Core", category: iot, icon: "iot" },
	iotdataplane: { title: "IoT Data Plane", category: iot, icon: "iot" },
	iotjobsdataplane: {
		title: "IoT Jobs Data Plane",
		category: iot,
		icon: "iot",
	},
	iotwireless: { title: "IoT Wireless", category: iot, icon: "iotwireless" },
	kinesis: {
		title: "Kinesis Data Streams",
		href: "/services/kinesis",
		category: analytics,
		icon: "kinesis",
	},
	kms: {
		title: "Key Management Service (KMS)",
		category: security,
		icon: "kms",
	},
	lambda: { title: "Lambda", category: compute, icon: "lambda" },
	logs: { title: "CloudWatch Logs", category: management, icon: "logs" },
	pipes: {
		href: "/services/pipes",
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
		href: "/services/servicediscovery",
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
	s3control: { title: "S3 Control", category: storage, icon: "s3" },
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
		title: "Systems Manager — Parameter Store",
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
