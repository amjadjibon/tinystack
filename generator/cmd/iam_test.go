package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestIAMModelMappings(t *testing.T) {
	tests := []struct{ service, operation, want string }{
		{"s3", "ListObjectsV2", "s3:ListBucket"},
		{"bedrockruntime", "Converse", "bedrock:InvokeModel"},
		{"bedrockruntime", "ConverseStream", "bedrock:InvokeModelWithResponseStream"},
		{"bedrockruntime", "CountTokens", "bedrock:CountTokens"},
		{"s3", "HeadObject", "s3:GetObject"},
		{"s3", "UploadPart", "s3:PutObject"},
		{"s3", "GetBucketEncryption", "s3:GetEncryptionConfiguration"},
		{"s3", "ListObjectVersions", "s3:ListBucketVersions"},
		{"s3", "ListMultipartUploads", "s3:ListBucketMultipartUploads"},
		{"s3", "DeleteBucketOwnershipControls", "s3:PutBucketOwnershipControls"},
		{"sns", "PublishBatch", "sns:Publish"},
		{"iotdataplane", "GetThingShadow", "iot:GetThingShadow"},
		{"iotjobsdataplane", "DescribeJobExecution", "iotjobsdata:DescribeJobExecution"},
		{"appconfigdata", "StartConfigurationSession", "appconfig:StartConfigurationSession"},
		{"cloudcontrol", "CreateResource", "cloudformation:CreateResource"},
		{"cloudwatch", "PutMetricData", "cloudwatch:PutMetricData"},
		{"sfn", "StartExecution", "states:StartExecution"},
		{"opensearch", "CreateDomain", "es:CreateDomain"},
		{"lambda", "Invoke", "lambda:InvokeFunction"},
		{"verifiedpermissions", "BatchIsAuthorized", "verifiedpermissions:IsAuthorized"},
		{"sqs", "SendMessageBatch", "sqs:SendMessage"},
		{"apigateway", "CreateRestApi", "apigateway:POST"},
	}

	for _, test := range tests {
		t.Run(test.service+"/"+test.operation, func(t *testing.T) {
			data, err := os.ReadFile("../model/" + test.service + ".json")
			if err != nil {
				t.Fatal(err)
			}

			var parsed model
			if err := json.Unmarshal(data, &parsed); err != nil {
				t.Fatal(err)
			}

			for id, service := range parsed.Shapes {
				if service.Type != "service" {
					continue
				}

				var signing struct{ Name string }
				if err := json.Unmarshal(service.Traits["aws.auth#sigv4"], &signing); err != nil {
					t.Fatal(err)
				}

				namespace, _, _ := strings.Cut(id, "#")
				op := parsed.Shapes[namespace+"#"+test.operation]
				got := iamAction(test.service, test.operation, iamPrefix(test.service, &service, signing.Name), &op)
				if got != test.want {
					t.Fatalf("got %s, want %s", got, test.want)
				}
			}
		})
	}
}
