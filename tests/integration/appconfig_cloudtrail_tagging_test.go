package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestAppConfigCloudTrailInventoryTagging(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := appconfigClient(s, "test", "us-east-1")
	app, err := c.CreateApplication(ctx, &appconfig.CreateApplicationInput{Name: new("tagged-app")})
	if err != nil {
		t.Fatal(err)
	}
	appARN := fmt.Sprintf("arn:aws:appconfig:us-east-1:000000000000:application/%s", aws.ToString(app.Id))
	_, err = s3Client(
		s,
		"test",
		"us-east-1",
	).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("tagged-trail-bucket")})
	if err != nil {
		t.Fatal(err)
	}
	ct := cloudtrailClient(s, "test", "us-east-1")
	trail, err := ct.CreateTrail(
		ctx,
		&cloudtrail.CreateTrailInput{Name: new("tagged-trail"), S3BucketName: new("tagged-trail-bucket")},
	)
	if err != nil {
		t.Fatal(err)
	}
	tags := taggingClient(s, "us-east-1")
	arns := []string{appARN, aws.ToString(trail.TrailARN)}
	tagged, err := tags.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{ResourceARNList: arns, Tags: map[string]string{"shared": "yes"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("tag: %+v %v", tagged, err)
	}
	listed, err := tags.GetResources(ctx, &resourcegroupstaggingapi.GetResourcesInput{ResourceARNList: arns})
	if err != nil || len(listed.ResourceTagMappingList) != 2 {
		t.Fatalf("inventory duplicated or missing: %+v %v", listed, err)
	}
	for _, r := range listed.ResourceTagMappingList {
		if len(r.Tags) != 1 || aws.ToString(r.Tags[0].Value) != "yes" {
			t.Fatalf("shared tags: %+v", r)
		}
	}
	untagged, err := tags.UntagResources(
		ctx,
		&resourcegroupstaggingapi.UntagResourcesInput{ResourceARNList: arns, TagKeys: []string{"shared"}},
	)
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatalf("untag: %+v %v", untagged, err)
	}
	appTags, err := c.ListTagsForResource(ctx, &appconfig.ListTagsForResourceInput{ResourceArn: new(appARN)})
	if err != nil || len(appTags.Tags) != 0 {
		t.Fatalf("application tags: %+v %v", appTags, err)
	}
	trailTags, err := ct.ListTags(
		ctx,
		&cloudtrail.ListTagsInput{ResourceIdList: []string{aws.ToString(trail.TrailARN)}},
	)
	if err != nil || len(trailTags.ResourceTagList[0].TagsList) != 0 {
		t.Fatalf("trail tags: %+v %v", trailTags, err)
	}
}
