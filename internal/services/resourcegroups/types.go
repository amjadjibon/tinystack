package resourcegroups

import (
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// namespaces maps a TinyStack service to the CloudFormation namespace of its
// resource types ("sqs" to "AWS::SQS::..."). A service missing here has no
// resource types a group can select.
var namespaces = map[string]string{
	"acm":             "CertificateManager",
	"apigateway":      "ApiGateway",
	"apigatewayv2":    "ApiGatewayV2",
	"appsync":         "AppSync",
	"athena":          "Athena",
	"autoscaling":     "AutoScaling",
	"cloudformation":  "CloudFormation",
	"cloudfront":      "CloudFront",
	"cloudwatch":      "CloudWatch",
	"cognitoidentity": "Cognito",
	"cognitoidp":      "Cognito",
	"dynamodb":        "DynamoDB",
	"ec2":             "EC2",
	"ecr":             "ECR",
	"ecs":             "ECS",
	"eks":             "EKS",
	"elasticache":     "ElastiCache",
	"rds":             "RDS",
	"elbv2":           "ElasticLoadBalancingV2",
	"eventbridge":     "Events",
	"firehose":        "KinesisFirehose",
	"glue":            "Glue",
	"iam":             "IAM",
	"iot":             "IoT",
	"kinesis":         "Kinesis",
	"kms":             "KMS",
	"lambda":          "Lambda",
	"logs":            "Logs",
	"pipes":           "Pipes",
	"resourcegroups":  "ResourceGroups",
	"route53":         "Route53",
	"s3":              "S3",
	"scheduler":       "Scheduler",
	"secretsmanager":  "SecretsManager",
	"sesv2":           "SES",
	"sfn":             "StepFunctions",
	"sns":             "SNS",
	"sqs":             "SQS",
	"ssm":             "SSM",
}

// typeNames are the CloudFormation names of the inventory types whose name is
// not the inventory type in PascalCase, by "service/type".
var typeNames = map[string]string{
	"apigatewayv2/http-api":               "Api",
	"appsync/graphql-api":                 "GraphQLApi",
	"athena/workgroup":                    "WorkGroup",
	"ec2/elastic-ip":                      "EIP",
	"ec2/nat-gateway":                     "NatGateway",
	"ec2/natgateway":                      "NatGateway",
	"ec2/network-acl":                     "NetworkAcl",
	"ec2/vpc":                             "VPC",
	"ec2/vpc-endpoint":                    "VPCEndpoint",
	"rds/db-instance":                     "DBInstance",
	"rds/db-subnet-group":                 "DBSubnetGroup",
	"rds/db-parameter-group":              "DBParameterGroup",
	"elasticache/cache-parameter-group":   "ParameterGroup",
	"elasticache/cache-replication-group": "ReplicationGroup",
	"elasticache/cache-subnet-group":      "SubnetGroup",
	"iam/policy":                          "ManagedPolicy",
	"iot/thingtype":                       "ThingType",
	"sesv2/identity":                      "EmailIdentity",
	"sesv2/template":                      "Template",
}

// pascal writes a kebab-case inventory type as a CloudFormation name.
func pascal(kind string) string {
	var out strings.Builder
	for _, word := range strings.Split(kind, "-") {
		if word != "" {
			out.WriteString(strings.ToUpper(word[:1]) + word[1:])
		}
	}

	return out.String()
}

// resourceType returns the CloudFormation-style type of a resource, such as
// "AWS::SQS::Queue", derived from the service that holds it and its inventory
// type. It reports false for the resources of a service with no namespace.
func resourceType(r *catalog.Owned) (string, bool) {
	namespace, ok := namespaces[r.Service]
	if !ok || r.Type == "" {
		return "", false
	}

	name, ok := typeNames[r.Service+"/"+r.Type]
	if !ok {
		name = pascal(r.Type)
	}

	return "AWS::" + namespace + "::" + name, true
}

// knownNamespace reports whether a service of the "AWS::<namespace>::" family
// is one whose resources a group can select.
func knownNamespace(resourceType string) bool {
	parts := strings.Split(resourceType, "::")
	if len(parts) != 3 {
		return false
	}

	for _, namespace := range namespaces {
		if namespace == parts[1] {
			return true
		}
	}

	return false
}
