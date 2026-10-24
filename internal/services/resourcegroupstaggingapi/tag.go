package resourcegroupstaggingapi

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroupstaggingapi"
	"github.com/amjadjibon/tinystack/internal/services/listing"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

// maxTaggedARNs is the most resources one TagResources or UntagResources call
// may name.
const (
	maxTaggedARNs    = 20
	resourceIDMember = "ResourceId"
)

// tagger tags and untags one service's resources through its own operations.
type tagger struct {
	tag   func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, tags map[string]string) error
	untag func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, keys []string) error
}

// target returns the members that name a resource in a service's tag
// operations.
type target func(ctx context.Context, c *catalog.Caller, r *catalog.Owned) (map[string]any, error)

// pairs encodes tags as a list of Key and Value members, as most services take them.
func pairs(tags map[string]string) any {
	return tagging.List(tags, func(k, v string) any { return map[string]any{"Key": k, "Value": v} })
}

// keyPairs encodes tags as KMS's list of TagKey and TagValue members.
func keyPairs(tags map[string]string) any {
	return tagging.List(tags, func(k, v string) any { return map[string]any{"TagKey": k, "TagValue": v} })
}

// plain encodes tags as a map.
func plain(tags map[string]string) any {
	out := make(map[string]any, len(tags))
	for k, v := range tags {
		out[k] = v
	}

	return out
}

// names encodes tag keys as a list of strings.
func names(keys []string) any {
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}

	return out
}

// keyOnly encodes tag keys as tags with no values, as ACM's
// RemoveTagsFromCertificate takes them.
func keyOnly(keys []string) any {
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = map[string]any{"Key": k}
	}

	return out
}

// byARN names a resource by its ARN in member.
func byARN(member string) target {
	return func(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
		return map[string]any{member: r.ARN}, nil
	}
}

// byName names a resource by its inventory name in member, with fixed members.
func byName(member string, fixed map[string]any) target {
	return func(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
		params := maps.Clone(fixed)
		if params == nil {
			params = map[string]any{}
		}

		params[member] = r.Name

		return params, nil
	}
}

// ssmResource names a Systems Manager parameter or document by its ResourceType and ResourceId.
func ssmResource(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
	kind := "Parameter"
	if r.Type == "document" {
		kind = "Document"
	}

	return map[string]any{"ResourceType": kind, resourceIDMember: r.Name}, nil
}

// operations is the tagger of a service whose tag operation takes the tags in
// tagsMember and whose untag operation takes the keys in keysMember.
func operations(service, tagOp, untagOp string, name target, tagsMember string, encode func(map[string]string) any,
	keysMember string, encodeKeys func([]string) any,
) tagger {
	call := func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, op, member string, value any) error {
		params, err := name(ctx, c, r)
		if err != nil {
			return err
		}

		params[member] = value
		_, err = c.Call(ctx, service, op, params)

		return err
	}

	return tagger{
		tag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, tags map[string]string) error {
			return call(ctx, c, r, tagOp, tagsMember, encode(tags))
		},
		untag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, keys []string) error {
			return call(ctx, c, r, untagOp, keysMember, encodeKeys(keys))
		},
	}
}

// resourceOperations is the tagger of a service with TagResource and
// UntagResource operations that name the resource by its ARN in arnMember.
func resourceOperations(service, arnMember string, encode func(map[string]string) any) tagger {
	return operations(service, "TagResource", "UntagResource", byARN(arnMember), "Tags", encode, "TagKeys", names)
}

// resourceID names an EC2 resource by the last segment of its ARN.
func resourceID(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
	return map[string]any{"Resources": []any{r.ARN[strings.LastIndex(r.ARN, "/")+1:]}}, nil
}

// queue names an SQS queue by the URL GetQueueUrl reports for it.
func queue(ctx context.Context, c *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
	out, err := c.Call(ctx, "sqs", "GetQueueUrl", map[string]any{"QueueName": r.Name})
	if err != nil {
		return nil, err
	}

	return map[string]any{"QueueUrl": out["QueueUrl"]}, nil
}

// zone names a Route 53 hosted zone or health check by the type and ID its
// ARN ends in ("hostedzone/<ID>").
func zone(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
	kind, id, _ := strings.Cut(r.ARN[strings.LastIndex(r.ARN, ":")+1:], "/")

	return map[string]any{"ResourceType": kind, resourceIDMember: id}, nil
}

// putBucketTags replaces a bucket's tags, which S3 only sets whole: tags is
// the inventory's set with the change applied.
func putBucketTags(ctx context.Context, c *catalog.Caller, r *catalog.Owned, tags map[string]string) error {
	if len(tags) == 0 {
		_, err := c.Call(ctx, "s3", "DeleteBucketTagging", map[string]any{"Bucket": r.Name})
		return err
	}

	_, err := c.Call(ctx, "s3", "PutBucketTagging", map[string]any{
		"Bucket": r.Name, "Tagging": map[string]any{"TagSet": pairs(tags)},
	})

	return err
}

// iamTaggers are IAM's taggers by inventory type.
var iamTaggers = map[string]tagger{
	"role":   operations("iam", "TagRole", "UntagRole", byName("RoleName", nil), "Tags", pairs, "TagKeys", names),
	"user":   operations("iam", "TagUser", "UntagUser", byName("UserName", nil), "Tags", pairs, "TagKeys", names),
	"policy": operations("iam", "TagPolicy", "UntagPolicy", byARN("PolicyArn"), "Tags", pairs, "TagKeys", names),
	"instance-profile": operations("iam", "TagInstanceProfile", "UntagInstanceProfile",
		byName("InstanceProfileName", nil), "Tags", pairs, "TagKeys", names),
}

// errUntaggable reports a resource whose service TinyStack cannot tag through this API.
func errUntaggable(r *catalog.Owned) error {
	return invalid("TinyStack cannot tag %s %s resources through this API.", r.Service, r.Type)
}

// taggers are the taggers of the services whose resources this API tags, by
// service name.
var taggers = map[string]tagger{
	"elasticbeanstalk": operations(
		"elasticbeanstalk",
		"UpdateTagsForResource",
		"UpdateTagsForResource",
		byARN("ResourceArn"),
		"TagsToAdd",
		pairs,
		"TagsToRemove",
		names,
	),
	"bedrock": resourceOperations("bedrock", "ResourceARN", pairs),
	"signer":  resourceOperations("signer", "ResourceArn", plain),
	"emr": operations("emr", "AddTags", "RemoveTags",
		func(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
			return map[string]any{resourceIDMember: r.ARN[strings.LastIndex(r.ARN, "/")+1:]}, nil
		}, "Tags", pairs, "TagKeys", names),
	"databasemigrationservice": operations("databasemigrationservice", "AddTagsToResource", "RemoveTagsFromResource",
		byARN("ResourceArn"), "Tags", pairs, "TagKeys", names),
	"costexplorer": operations(
		"costexplorer",
		"TagResource",
		"UntagResource",
		byARN("ResourceArn"),
		"ResourceTags",
		pairs,
		"ResourceTagKeys",
		names,
	),
	"costandusagereportservice": operations("costandusagereportservice", "TagResource", "UntagResource",
		byName("ReportName", nil), "Tags", pairs, "TagKeys", names),
	"budgets": operations("budgets", "TagResource", "UntagResource", byARN("ResourceARN"),
		"ResourceTags", pairs, "ResourceTagKeys", names),
	"codeartifact":         resourceOperations("codeartifact", "ResourceArn", pairs),
	"timestreamwrite":      resourceOperations("timestreamwrite", "ResourceARN", pairs),
	"kinesisanalyticsv2":   resourceOperations("kinesisanalyticsv2", "ResourceARN", pairs),
	"opensearchserverless": resourceOperations("opensearchserverless", "ResourceArn", pairs),
	"emrserverless":        resourceOperations("emrserverless", "ResourceArn", plain),
	"apprunner":            resourceOperations("apprunner", "ResourceArn", pairs),
	"networkfirewall":      resourceOperations("networkfirewall", "ResourceArn", pairs),
	"amplify":              resourceOperations("amplify", "ResourceArn", plain),
	"codeconnections":      resourceOperations("codeconnections", "ResourceArn", pairs),
	"memorydb":             resourceOperations("memorydb", "ResourceArn", pairs),
	"redshift": operations("redshift", "CreateTags", "DeleteTags", byARN("ResourceName"),
		"Tags", pairs, "TagKeys", names),
	"codedeploy":             resourceOperations("codedeploy", "ResourceArn", pairs),
	"applicationautoscaling": resourceOperations("applicationautoscaling", "ResourceARN", plain),
	"opensearch": operations(
		"opensearch",
		"AddTags",
		"RemoveTags",
		byARN("ARN"),
		"TagList",
		pairs,
		"TagKeys",
		names,
	),
	"route53resolver":     resourceOperations("route53resolver", "ResourceArn", pairs),
	"xray":                resourceOperations("xray", "ResourceARN", pairs),
	"codepipeline":        resourceOperations("codepipeline", "ResourceArn", pairs),
	"codecommit":          resourceOperations("codecommit", "ResourceArn", plain),
	"verifiedpermissions": resourceOperations("verifiedpermissions", "ResourceArn", plain),
	"inspector2":          resourceOperations("inspector2", "ResourceArn", plain),
	"securityhub":         resourceOperations("securityhub", "ResourceArn", plain),
	"amp":                 resourceOperations("amp", "ResourceArn", plain),
	"resourceexplorer2":   resourceOperations("resourceexplorer2", "ResourceArn", plain),
	"fis":                 resourceOperations("fis", "ResourceArn", plain),
	"swf":                 resourceOperations("swf", "ResourceArn", pairs),
	"accessanalyzer":      resourceOperations("accessanalyzer", "ResourceArn", plain),
	"shield":              resourceOperations("shield", "ResourceARN", pairs),
	"globalaccelerator":   resourceOperations("globalaccelerator", "ResourceArn", pairs),
	"ram":                 resourceOperations("ram", "ResourceShareArn", pairs),
	"datasync": operations(
		"datasync",
		"TagResource",
		"UntagResource",
		byARN("ResourceArn"),
		"Tags",
		pairs,
		"Keys",
		names,
	),
	"servicecatalog": {
		tag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, tags map[string]string) error {
			return serviceCatalogTags(ctx, c, r, "AddTags", pairs(tags))
		},
		untag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, keys []string) error {
			return serviceCatalogTags(ctx, c, r, "RemoveTags", names(keys))
		},
	},
	"ssoadmin": operations("ssoadmin", "TagResource", "UntagResource",
		func(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
			details, ok := r.Details.(map[string]any)
			if !ok {
				return nil, awserror.Validation("Missing Identity Center instance metadata")
			}

			return map[string]any{"ResourceArn": r.ARN, "InstanceArn": details["InstanceArn"]}, nil
		}, "Tags", pairs, "TagKeys", names),
	"acmpca": operations("acmpca", "TagCertificateAuthority", "UntagCertificateAuthority",
		byARN("CertificateAuthorityArn"), "Tags", pairs, "Tags", keyOnly),
	"s3tables": operations(
		"s3tables",
		"TagResource",
		"UntagResource",
		byARN("ResourceArn"),
		"Tags",
		plain,
		"TagKeys",
		names,
	),
	"location": operations(
		"location",
		"TagResource",
		"UntagResource",
		byARN("ResourceArn"),
		"Tags",
		plain,
		"TagKeys",
		names,
	),
	"glacier": operations(
		"glacier",
		"AddTagsToVault",
		"RemoveTagsFromVault",
		byName("VaultName", map[string]any{"AccountId": "-"}),
		"Tags",
		plain,
		"TagKeys",
		names,
	),

	"kafka": operations("kafka", "TagResource", "UntagResource", byARN("ResourceArn"), "Tags", plain, "TagKeys", names),
	"mwaa":  operations("mwaa", "TagResource", "UntagResource", byARN("ResourceArn"), "Tags", plain, "tagKeys", names),
	"mq":    operations("mq", "CreateTags", "DeleteTags", byARN("ResourceArn"), "Tags", plain, "TagKeys", names),
	"codebuild": {
		tag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, tags map[string]string) error {
			merged := maps.Clone(r.Tags)
			if merged == nil {
				merged = map[string]string{}
			}
			maps.Copy(merged, tags)
			return putCodeBuildTags(ctx, c, r, merged)
		},
		untag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, keys []string) error {
			return putCodeBuildTags(ctx, c, r, tagging.Remove(maps.Clone(r.Tags), keys))
		},
	},
	"transfer": operations(
		"transfer", "TagResource", "UntagResource", byARN("Arn"), "Tags", pairs, "TagKeys", names,
	),
	"organizations": operations(
		"organizations",
		"TagResource",
		"UntagResource",
		byName(resourceIDMember, nil),
		"Tags",
		pairs,
		"TagKeys",
		names,
	),
	"servicediscovery": resourceOperations("servicediscovery", "ResourceARN", pairs),
	"batch":            resourceOperations("batch", "ResourceArn", plain),
	"efs":              resourceOperations("efs", resourceIDMember, pairs),
	"waf":              resourceOperations("waf", "ResourceARN", pairs),
	"wafv2":            resourceOperations("wafv2", "ResourceARN", pairs),
	"backup": operations("backup", "TagResource", "UntagResource", byARN("ResourceArn"),
		"Tags", plain, "TagKeyList", names),
	"config":    resourceOperations("config", "ResourceArn", pairs),
	"appconfig": resourceOperations("appconfig", "ResourceArn", plain),
	"cloudtrail": operations("cloudtrail", "AddTags", "RemoveTags", byARN(resourceIDMember),
		"TagsList", pairs, "TagsList", keyOnly),
	"rds": operations("rds", "AddTagsToResource", "RemoveTagsFromResource", byARN("ResourceName"),
		"Tags", pairs, "TagKeys", names),
	"acm": operations("acm", "AddTagsToCertificate", "RemoveTagsFromCertificate", byARN("CertificateArn"),
		"Tags", pairs, "Tags", keyOnly),
	"apigateway":      resourceOperations("apigateway", "ResourceArn", plain),
	"apigatewayv2":    resourceOperations("apigatewayv2", "ResourceArn", plain),
	"appsync":         resourceOperations("appsync", "ResourceArn", plain),
	"athena":          resourceOperations("athena", "ResourceARN", pairs),
	"cloudwatch":      resourceOperations("cloudwatch", "ResourceARN", pairs),
	"cognitoidentity": resourceOperations("cognitoidentity", "ResourceArn", plain),
	"cognitoidp":      resourceOperations("cognitoidp", "ResourceArn", plain),
	"dynamodb":        resourceOperations("dynamodb", "ResourceArn", pairs),
	"ecr":             resourceOperations("ecr", "ResourceArn", pairs),
	"ecs":             resourceOperations("ecs", "ResourceArn", pairs),
	"eks":             resourceOperations("eks", "ResourceArn", plain),
	"elb": operations("elb", "AddTags", "RemoveTags",
		func(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
			return map[string]any{"LoadBalancerNames": []any{r.Name}}, nil
		}, "Tags", pairs, "Tags", keyOnly),
	"elbv2": operations("elbv2", "AddTags", "RemoveTags",
		func(_ context.Context, _ *catalog.Caller, r *catalog.Owned) (map[string]any, error) {
			return map[string]any{"ResourceArns": []any{r.ARN}}, nil
		}, "Tags", pairs, "TagKeys", names),
	"ec2":         operations("ec2", "CreateTags", "DeleteTags", resourceID, "Tags", pairs, "Tags", keyOnly),
	"eventbridge": resourceOperations("eventbridge", "ResourceARN", pairs),
	"firehose": operations("firehose", "TagDeliveryStream", "UntagDeliveryStream",
		byName("DeliveryStreamName", nil), "Tags", pairs, "TagKeys", names),
	"glue": operations("glue", "TagResource", "UntagResource", byARN("ResourceArn"),
		"TagsToAdd", plain, "TagsToRemove", names),
	"iam": {
		tag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, tags map[string]string) error {
			t, ok := iamTaggers[r.Type]
			if !ok {
				return errUntaggable(r)
			}

			return t.tag(ctx, c, r, tags)
		},
		untag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, keys []string) error {
			t, ok := iamTaggers[r.Type]
			if !ok {
				return errUntaggable(r)
			}

			return t.untag(ctx, c, r, keys)
		},
	},
	"iot":     resourceOperations("iot", "ResourceArn", pairs),
	"kinesis": resourceOperations("kinesis", "ResourceARN", plain),
	"kms":     resourceOperations("kms", "KeyId", keyPairs),
	"lambda":  resourceOperations("lambda", "Resource", plain),
	"logs":    resourceOperations("logs", "ResourceArn", plain),
	"pipes":   resourceOperations("pipes", "ResourceArn", plain),
	"route53": operations(
		"route53",
		"ChangeTagsForResource",
		"ChangeTagsForResource",
		zone,
		"AddTags",
		pairs,
		"RemoveTagKeys",
		names,
	),
	"s3": {
		tag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, tags map[string]string) error {
			merged := maps.Clone(r.Tags)
			if merged == nil {
				merged = map[string]string{}
			}

			maps.Copy(merged, tags)

			return putBucketTags(ctx, c, r, merged)
		},
		untag: func(ctx context.Context, c *catalog.Caller, r *catalog.Owned, keys []string) error {
			return putBucketTags(ctx, c, r, tagging.Remove(maps.Clone(r.Tags), keys))
		},
	},
	"resourcegroups": operations("resourcegroups", "Tag", "Untag", byARN("Arn"), "Tags", plain, "Keys", names),
	"scheduler":      resourceOperations("scheduler", "ResourceArn", pairs),
	"secretsmanager": resourceOperations("secretsmanager", "SecretId", pairs),
	"sesv2":          resourceOperations("sesv2", "ResourceArn", pairs),
	"sfn":            resourceOperations("sfn", "ResourceArn", pairs),
	"sns":            resourceOperations("sns", "ResourceArn", pairs),
	"sqs":            operations("sqs", "TagQueue", "UntagQueue", queue, "Tags", plain, "TagKeys", names),
	"ssm": operations("ssm", "AddTagsToResource", "RemoveTagsFromResource", ssmResource, "Tags", pairs, "TagKeys",
		names),
}

// failure is how TagResources and UntagResources report one resource's error:
// a client error as InvalidParameterException and anything else as
// InternalServiceException.
func failure(err error) api.FailureInfo {
	e := awserror.From(err)
	code := "InvalidParameterException"
	if e.StatusCode >= http.StatusInternalServerError {
		code = "InternalServiceException"
	}

	return api.FailureInfo{StatusCode: new(int64(e.StatusCode)), ErrorCode: new(code), ErrorMessage: new(e.Error())}
}

// apply runs change on each resource the scope holds in arns, reporting the
// ones it fails for.
func (s *Service) apply(
	ctx context.Context, arns []string, change func(t tagger, r *catalog.Owned) error,
) (map[string]api.FailureInfo, error) {
	if len(arns) < 1 || len(arns) > maxTaggedARNs {
		return nil, invalid("ResourceARNList must name 1 to %d resources.", maxTaggedARNs)
	}

	resources, err := listing.Scoped(ctx, s.caller)
	if err != nil {
		return nil, err
	}

	byARN := make(map[string]*catalog.Owned, len(resources))
	for i := range resources {
		byARN[resources[i].ARN] = &resources[i]
	}

	failed := map[string]api.FailureInfo{}
	for _, arn := range arns {
		r, ok := byARN[arn]
		if !ok {
			failed[arn] = failure(invalid("The resource %s was not found.", arn))
			continue
		}

		t, ok := taggers[r.Service]
		if !ok {
			failed[arn] = failure(errUntaggable(r))
			continue
		}

		if changeErr := change(t, r); changeErr != nil {
			failed[arn] = failure(changeErr)
		}
	}

	return failed, nil
}

// TagResources adds tags to each resource it names, through the resource's own
// service.
func (s *Service) TagResources(ctx context.Context, in *api.TagResourcesInput) (*api.TagResourcesOutput, error) {
	if len(in.Tags) == 0 {
		return nil, invalid("Tags must hold at least one tag.")
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid("%s", err.Error())
	}

	failed, err := s.apply(ctx, in.ResourceARNList, func(t tagger, r *catalog.Owned) error {
		return t.tag(ctx, s.caller, r, in.Tags)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourcesOutput{FailedResourcesMap: failed}, nil
}

// UntagResources removes tag keys from each resource it names, through the
// resource's own service.
func (s *Service) UntagResources(
	ctx context.Context, in *api.UntagResourcesInput,
) (*api.UntagResourcesOutput, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > tagging.Max {
		return nil, invalid("TagKeys must hold 1 to %d keys.", tagging.Max)
	}

	for _, k := range in.TagKeys {
		if n := utf8.RuneCountInString(k); n < 1 || n > 128 {
			return nil, invalid("Tag keys must be 1 to 128 characters long.")
		}
	}

	failed, err := s.apply(ctx, in.ResourceARNList, func(t tagger, r *catalog.Owned) error {
		return t.untag(ctx, s.caller, r, in.TagKeys)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourcesOutput{FailedResourcesMap: failed}, nil
}

// CodeBuild tags are replaced through UpdateProject; it has no tag endpoints.
func putCodeBuildTags(ctx context.Context, c *catalog.Caller, r *catalog.Owned, tags map[string]string) error {
	_, err := c.Call(ctx, "codebuild", "UpdateProject", map[string]any{"Name": r.Name, "Tags": pairs(tags)})
	return err
}

func serviceCatalogTags(ctx context.Context, c *catalog.Caller, r *catalog.Owned, member string, value any) error {
	op := "UpdateProduct"
	if r.Type == "portfolio" {
		op = "UpdatePortfolio"
	}

	_, id, _ := strings.Cut(r.ARN, "/")
	_, err := c.Call(ctx, "servicecatalog", op, map[string]any{"Id": id, member: value})
	return err
}
