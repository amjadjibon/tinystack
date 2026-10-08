package integration_test

import (
	"context"
	"maps"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	tagging "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	taggingtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func taggingClient(s *httptest.Server, region string) *tagging.Client {
	return tagging.New(tagging.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// taggedResources holds the ARNs of the resources TestResourceGroupsTagging creates.
type taggedResources struct {
	bucket, queue, queueURL, table, role, key string
}

// tableWithTags is a CreateTable request for a table tagged with key, value pairs.
func tableWithTags(name string, pairs ...string) *sdk.CreateTableInput {
	in := &sdk.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash}},
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		in.Tags = append(in.Tags, types.Tag{Key: aws.String(pairs[i]), Value: aws.String(pairs[i+1])})
	}

	return in
}

func createTaggedResources(t *testing.T, s *httptest.Server) taggedResources {
	t.Helper()
	ctx := context.Background()
	var r taggedResources

	b := s3Client(s, "test", "us-east-1")
	if _, err := b.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("tagged-bucket")}); err != nil {
		t.Fatal(err)
	}

	if _, err := b.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: aws.String("tagged-bucket"),
		Tagging: &s3types.Tagging{TagSet: []s3types.Tag{
			{Key: aws.String("env"), Value: aws.String("prod")}, {Key: aws.String("team"), Value: aws.String("a")},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	r.bucket = "arn:aws:s3:::tagged-bucket"

	q := sqsClient(s, "test")
	created, err := q.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("tagged-queue"), Tags: map[string]string{"env": "dev"},
	})
	if err != nil {
		t.Fatal(err)
	}

	r.queueURL = *created.QueueUrl
	attrs, err := q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: created.QueueUrl, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatal(err)
	}

	r.queue = attrs.Attributes["QueueArn"]

	d := client(s, "test", "us-east-1")
	table, err := d.CreateTable(ctx, tableWithTags("tagged-table", "env", "prod", "team", "b"))
	if err != nil {
		t.Fatal(err)
	}

	r.table = *table.TableDescription.TableArn

	// A table in another Region is never reported to us-east-1 callers.
	if _, err = client(s, "test", "eu-west-1").CreateTable(ctx, tableWithTags("elsewhere", "env", "prod")); err != nil {
		t.Fatal(err)
	}

	role, err := iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String("tagged-role"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[]}`),
		Tags:                     []iamtypes.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	r.role = *role.Role.Arn

	key, err := kmsClient(s, "test", "us-east-1").CreateKey(ctx, &kms.CreateKeyInput{})
	if err != nil {
		t.Fatal(err)
	}

	r.key = *key.KeyMetadata.Arn

	return r
}

// arns returns the ARNs of a GetResources page.
func arns(mappings []taggingtypes.ResourceTagMapping) []string {
	out := make([]string, 0, len(mappings))
	for _, m := range mappings {
		out = append(out, *m.ResourceARN)
	}

	return out
}

func sortedARNs(list ...string) []string {
	slices.Sort(list)
	return list
}

func TestResourceGroupsTaggingRead(t *testing.T) {
	s := server(t)
	r := createTaggedResources(t, s)
	c := taggingClient(s, "us-east-1")
	ctx := context.Background()

	all, err := c.GetResources(ctx, &tagging.GetResourcesInput{})
	if err != nil {
		t.Fatal(err)
	}

	// The untagged KMS key and the eu-west-1 table are left out.
	if got, want := arns(
		all.ResourceTagMappingList,
	), sortedARNs(
		r.bucket,
		r.queue,
		r.table,
		r.role,
	); !slices.Equal(
		got,
		want,
	) {
		t.Fatalf("GetResources = %v, want %v", got, want)
	}

	for _, m := range all.ResourceTagMappingList {
		if *m.ResourceARN == r.bucket && len(m.Tags) != 2 {
			t.Fatalf("bucket tags = %+v", m.Tags)
		}
	}

	cases := []struct {
		name string
		in   tagging.GetResourcesInput
		want []string
	}{
		{"key and value", tagging.GetResourcesInput{
			TagFilters: []taggingtypes.TagFilter{{Key: aws.String("env"), Values: []string{"prod"}}},
		}, sortedARNs(r.bucket, r.table, r.role)},
		{"any of values", tagging.GetResourcesInput{
			TagFilters: []taggingtypes.TagFilter{{Key: aws.String("team"), Values: []string{"a", "z"}}},
		}, []string{r.bucket}},
		{"filters combine", tagging.GetResourcesInput{
			TagFilters: []taggingtypes.TagFilter{{Key: aws.String("env")}, {Key: aws.String("team")}},
		}, sortedARNs(r.bucket, r.table)},
		{"resource types", tagging.GetResourcesInput{
			ResourceTypeFilters: []string{"dynamodb:table", "sqs"},
		}, sortedARNs(r.queue, r.table)},
		{
			"untagged by ARN",
			tagging.GetResourcesInput{ResourceARNList: []string{r.key, r.queue}},
			sortedARNs(r.key, r.queue),
		},
	}
	for _, tc := range cases {
		out, getErr := c.GetResources(ctx, &tc.in)
		if getErr != nil {
			t.Fatalf("%s: %v", tc.name, getErr)
		}

		if got := arns(out.ResourceTagMappingList); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	var paged []string
	pages := tagging.NewGetResourcesPaginator(c, &tagging.GetResourcesInput{ResourcesPerPage: aws.Int32(1)})
	for pages.HasMorePages() {
		page, pageErr := pages.NextPage(ctx)
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		if len(page.ResourceTagMappingList) > 1 {
			t.Fatalf("page of %d", len(page.ResourceTagMappingList))
		}

		paged = append(paged, arns(page.ResourceTagMappingList)...)
	}

	if !slices.Equal(paged, arns(all.ResourceTagMappingList)) {
		t.Fatalf("paged = %v", paged)
	}

	keys, err := c.GetTagKeys(ctx, &tagging.GetTagKeysInput{})
	if err != nil || !slices.Equal(keys.TagKeys, []string{"env", "team"}) {
		t.Fatalf("GetTagKeys = %+v, %v", keys, err)
	}

	values, err := c.GetTagValues(ctx, &tagging.GetTagValuesInput{Key: aws.String("env")})
	if err != nil || !slices.Equal(values.TagValues, []string{"dev", "prod"}) {
		t.Fatalf("GetTagValues = %+v, %v", values, err)
	}

	elsewhere, err := taggingClient(s, "eu-west-1").GetResources(ctx, &tagging.GetResourcesInput{})
	if err != nil || len(elsewhere.ResourceTagMappingList) != 1 ||
		*elsewhere.ResourceTagMappingList[0].ResourceARN == r.table {
		t.Fatalf("eu-west-1 GetResources = %+v, %v", elsewhere, err)
	}

	_, err = c.GetResources(ctx, &tagging.GetResourcesInput{
		ResourceARNList: []string{r.key},
		TagFilters:      []taggingtypes.TagFilter{{Key: aws.String("env")}},
	})
	code(t, err, "InvalidParameterException")

	_, err = c.GetResources(ctx, &tagging.GetResourcesInput{PaginationToken: aws.String("!")})
	code(t, err, "InvalidParameterException")
}

func TestResourceGroupsTaggingWrite(t *testing.T) {
	s := server(t)
	r := createTaggedResources(t, s)
	c := taggingClient(s, "us-east-1")
	ctx := context.Background()
	missing := "arn:aws:sqs:us-east-1:000000000000:missing"

	tagged, err := c.TagResources(ctx, &tagging.TagResourcesInput{
		ResourceARNList: []string{r.bucket, r.queue, r.table, r.role, r.key, missing},
		Tags:            map[string]string{"owner": "me"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := slices.Collect(maps.Keys(tagged.FailedResourcesMap)); !slices.Equal(got, []string{missing}) {
		t.Fatalf("FailedResourcesMap = %+v", tagged.FailedResourcesMap)
	}

	if f := tagged.FailedResourcesMap[missing]; f.ErrorCode != taggingtypes.ErrorCodeInvalidParameterException ||
		f.StatusCode != 400 {
		t.Fatalf("failure = %+v", f)
	}

	// Each service reports the tag through its own API; S3 keeps the bucket's other tags.
	bucketTags, err := s3Client(s, "test", "us-east-1").GetBucketTagging(ctx,
		&s3.GetBucketTaggingInput{Bucket: aws.String("tagged-bucket")})
	if err != nil || len(bucketTags.TagSet) != 3 {
		t.Fatalf("bucket tags = %+v, %v", bucketTags, err)
	}

	queueTags, err := sqsClient(s, "test").ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: &r.queueURL})
	if err != nil || queueTags.Tags["owner"] != "me" || queueTags.Tags["env"] != "dev" {
		t.Fatalf("queue tags = %+v, %v", queueTags, err)
	}

	tableTags, err := client(s, "test", "us-east-1").ListTagsOfResource(ctx,
		&sdk.ListTagsOfResourceInput{ResourceArn: &r.table})
	if err != nil || len(tableTags.Tags) != 3 {
		t.Fatalf("table tags = %+v, %v", tableTags, err)
	}

	roleTags, err := iamClient(s, "test", "us-east-1").ListRoleTags(ctx,
		&iam.ListRoleTagsInput{RoleName: aws.String("tagged-role")})
	if err != nil || len(roleTags.Tags) != 2 {
		t.Fatalf("role tags = %+v, %v", roleTags, err)
	}

	keyTags, err := kmsClient(s, "test", "us-east-1").ListResourceTags(ctx, &kms.ListResourceTagsInput{KeyId: &r.key})
	if err != nil || len(keyTags.Tags) != 1 {
		t.Fatalf("key tags = %+v, %v", keyTags, err)
	}

	untagged, err := c.UntagResources(ctx, &tagging.UntagResourcesInput{
		ResourceARNList: []string{r.bucket, r.queue},
		TagKeys:         []string{"owner", "env"},
	})
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatalf("UntagResources = %+v, %v", untagged, err)
	}

	bucketTags, err = s3Client(s, "test", "us-east-1").GetBucketTagging(ctx,
		&s3.GetBucketTaggingInput{Bucket: aws.String("tagged-bucket")})
	if err != nil || len(bucketTags.TagSet) != 1 || *bucketTags.TagSet[0].Key != "team" {
		t.Fatalf("bucket tags = %+v, %v", bucketTags, err)
	}

	queueTags, err = sqsClient(s, "test").ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: &r.queueURL})
	if err != nil || len(queueTags.Tags) != 0 {
		t.Fatalf("queue tags = %+v, %v", queueTags, err)
	}

	// The untagged queue is no longer reported.
	out, err := c.GetResources(ctx, &tagging.GetResourcesInput{ResourceTypeFilters: []string{"sqs"}})
	if err != nil || len(out.ResourceTagMappingList) != 0 {
		t.Fatalf("GetResources = %+v, %v", out, err)
	}

	_, err = c.TagResources(ctx, &tagging.TagResourcesInput{
		ResourceARNList: []string{r.queue}, Tags: map[string]string{"aws:reserved": "x"},
	})
	code(t, err, "InvalidParameterException")
}

func TestResourceGroupsTaggingEC2(t *testing.T) {
	s := server(t)
	e := ec2Client(s, "test", "us-east-1")
	c := taggingClient(s, "us-east-1")
	ctx := context.Background()

	vpc, err := e.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.9.0.0/16"),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVpc, Tags: ec2Tags("env", "ec2test"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	vpcID := aws.ToString(vpc.Vpc.VpcId)
	vpcARN := "arn:aws:ec2:us-east-1:000000000000:vpc/" + vpcID

	launched, err := e.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance, Tags: ec2Tags("env", "ec2test"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	instanceID := aws.ToString(launched.Instances[0].InstanceId)
	instanceARN := "arn:aws:ec2:us-east-1:000000000000:instance/" + instanceID

	found, err := c.GetResources(ctx, &tagging.GetResourcesInput{
		TagFilters: []taggingtypes.TagFilter{{Key: aws.String("env"), Values: []string{"ec2test"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got, want := arns(found.ResourceTagMappingList), sortedARNs(vpcARN, instanceARN); !slices.Equal(got, want) {
		t.Fatalf("GetResources = %v, want %v", got, want)
	}

	vpcs, err := c.GetResources(ctx, &tagging.GetResourcesInput{ResourceTypeFilters: []string{"ec2:vpc"}})
	if err != nil {
		t.Fatal(err)
	}

	if got := arns(vpcs.ResourceTagMappingList); !slices.Equal(got, []string{vpcARN}) {
		t.Fatalf("ec2:vpc = %v", got)
	}

	tagged, err := c.TagResources(ctx, &tagging.TagResourcesInput{
		ResourceARNList: []string{vpcARN, instanceARN}, Tags: map[string]string{"owner": "me"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("TagResources = %+v, %v", tagged, err)
	}

	described, err := e.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil || len(described.Vpcs) != 1 || len(described.Vpcs[0].Tags) != 2 {
		t.Fatalf("DescribeVpcs = %+v, %v", described, err)
	}

	untagged, err := c.UntagResources(ctx, &tagging.UntagResourcesInput{
		ResourceARNList: []string{vpcARN, instanceARN}, TagKeys: []string{"owner"},
	})
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatalf("UntagResources = %+v, %v", untagged, err)
	}

	tags, err := e.DescribeTags(ctx, &ec2.DescribeTagsInput{Filters: []ec2types.Filter{
		{Name: aws.String("key"), Values: []string{"owner"}},
	}})
	if err != nil || len(tags.Tags) != 0 {
		t.Fatalf("DescribeTags = %+v, %v", tags, err)
	}

	described, err = e.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil || len(described.Vpcs[0].Tags) != 1 {
		t.Fatalf("DescribeVpcs = %+v, %v", described, err)
	}

	// A terminated instance leaves the inventory.
	if _, err = e.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instanceID}}); err != nil {
		t.Fatal(err)
	}

	found, err = c.GetResources(ctx, &tagging.GetResourcesInput{ResourceTypeFilters: []string{"ec2:instance"}})
	if err != nil || len(found.ResourceTagMappingList) != 0 {
		t.Fatalf("after terminate = %+v, %v", found, err)
	}
}
