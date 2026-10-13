package main

import (
	"encoding/json"
	"strings"
)

// iamPrefix uses modeled service data, independent of the signing name.
func iamPrefix(name string, service *shape, signing string) string {
	// Some model ARN namespaces differ from their IAM prefixes.
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_cloudwatch.html
	switch name {
	case "cloudwatch":
		return "cloudwatch"
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_iot.html
	case "iotdataplane":
		return "iot"
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_iot-jobs-data.html
	case "iotjobsdataplane":
		return "iotjobsdata"
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_appconfig.html
	case "appconfigdata":
		return "appconfig"
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_cloudcontrol.html
	case "cloudcontrol":
		return "cloudformation"
	}

	var data struct{ ArnNamespace, EndpointPrefix string }
	if json.Unmarshal(service.Traits["aws.api#service"], &data) == nil {
		if data.ArnNamespace != "" {
			return data.ArnNamespace
		}

		if data.EndpointPrefix != "" {
			return data.EndpointPrefix
		}
	}

	return signing
}

// iamOverrides fills action names absent from the pinned model's IAM traits.
// Every entry in each service group is documented by its SAR action/API table.
const (
	iamListBucket = "ListBucket"
	iamPutObject  = "PutObject"
)

var iamOverrides = map[string]map[string]string{
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_s3.html
	"s3": {
		"ListObjects":                        iamListBucket,
		"ListObjectsV2":                      iamListBucket,
		"HeadBucket":                         iamListBucket,
		"HeadObject":                         "GetObject",
		"ListBuckets":                        "ListAllMyBuckets",
		"ListObjectVersions":                 "ListBucketVersions",
		"ListMultipartUploads":               "ListBucketMultipartUploads",
		"DeleteObjects":                      "DeleteObject",
		"CreateMultipartUpload":              iamPutObject,
		"UploadPart":                         iamPutObject,
		"CompleteMultipartUpload":            iamPutObject,
		"UploadPartCopy":                     iamPutObject,
		"CopyObject":                         iamPutObject,
		"ListParts":                          "ListMultipartUploadParts",
		"GetBucketNotificationConfiguration": "GetBucketNotification",
		"PutBucketNotificationConfiguration": "PutBucketNotification",
		"GetBucketLifecycleConfiguration":    "GetLifecycleConfiguration",
		"PutBucketLifecycleConfiguration":    "PutLifecycleConfiguration",
		"DeleteBucketLifecycle":              "PutLifecycleConfiguration",
		"DeleteBucketCors":                   "PutBucketCORS",
		"GetBucketCors":                      "GetBucketCORS",
		"PutBucketCors":                      "PutBucketCORS",
		"DeleteBucketTagging":                "PutBucketTagging",
		"DeleteBucketOwnershipControls":      "PutBucketOwnershipControls",
		"GetBucketEncryption":                "GetEncryptionConfiguration",
		"PutBucketEncryption":                "PutEncryptionConfiguration",
		"DeleteBucketEncryption":             "PutEncryptionConfiguration",
		"GetPublicAccessBlock":               "GetBucketPublicAccessBlock",
		"PutPublicAccessBlock":               "PutBucketPublicAccessBlock",
		"DeletePublicAccessBlock":            "PutBucketPublicAccessBlock",
	},
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_sqs.html
	"sqs": {
		"SendMessageBatch": "SendMessage", "DeleteMessageBatch": "DeleteMessage",
		"ChangeMessageVisibilityBatch": "ChangeMessageVisibility",
	},
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_sns.html
	"sns": {"PublishBatch": "Publish"},
}

func iamAction(service, operation, prefix string, op *shape) string {
	var trait struct{ Name string }
	if json.Unmarshal(op.Traits["aws.iam#iamAction"], &trait) == nil && trait.Name != "" {
		if strings.Contains(trait.Name, ":") {
			return trait.Name
		}

		return prefix + ":" + trait.Name
	}

	// API Gateway management authorizes HTTP verbs, not SDK method names:
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_apigateway.html
	if service == "apigateway" || service == "apigatewayv2" {
		var http struct{ Method string }
		if json.Unmarshal(op.Traits["smithy.api#http"], &http) == nil && http.Method != "" {
			return prefix + ":" + http.Method
		}
	}

	if action, ok := iamOverrides[service][operation]; ok {
		return prefix + ":" + action
	}

	return prefix + ":" + operation
}
