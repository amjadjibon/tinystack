package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

func TestCloudFrontKVSControl(t *testing.T) {
	c := cloudfrontClient(server(t))
	ctx := context.Background()
	a, err := c.CreateKeyValueStore(ctx, &cloudfront.CreateKeyValueStoreInput{Name: new("alpha")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(a.KeyValueStore.Status) != "PROVISIONING" {
		t.Fatal(a)
	}
	_, err = c.CreateKeyValueStore(ctx, &cloudfront.CreateKeyValueStoreInput{Name: new("alpha")})
	wantAccessAnalyzerError(t, err, "EntityAlreadyExists")
	_, err = c.CreateKeyValueStore(ctx, &cloudfront.CreateKeyValueStoreInput{Name: new("beta")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.DescribeKeyValueStore(ctx, &cloudfront.DescribeKeyValueStoreInput{Name: new("alpha")})
	if err != nil || aws.ToString(got.KeyValueStore.Status) != "READY" {
		t.Fatalf("describe: %+v %v", got, err)
	}
	page, err := c.ListKeyValueStores(
		ctx,
		&cloudfront.ListKeyValueStoresInput{MaxItems: new(int32(1)), Status: new("READY")},
	)
	if err != nil || len(page.KeyValueStoreList.Items) != 1 || page.KeyValueStoreList.NextMarker == nil {
		t.Fatalf("page: %+v %v", page, err)
	}
	page, err = c.ListKeyValueStores(
		ctx,
		&cloudfront.ListKeyValueStoresInput{MaxItems: new(int32(1)), Marker: page.KeyValueStoreList.NextMarker},
	)
	if err != nil || len(page.KeyValueStoreList.Items) != 1 {
		t.Fatalf("next: %+v %v", page, err)
	}
	_, err = c.UpdateKeyValueStore(
		ctx,
		&cloudfront.UpdateKeyValueStoreInput{Name: new("alpha"), Comment: new("changed"), IfMatch: new("stale")},
	)
	wantAccessAnalyzerError(t, err, "PreconditionFailed")
	updated, err := c.UpdateKeyValueStore(
		ctx,
		&cloudfront.UpdateKeyValueStoreInput{Name: new("alpha"), Comment: new("changed"), IfMatch: got.ETag},
	)
	if err != nil || *updated.ETag == *got.ETag {
		t.Fatalf("update: %+v %v", updated, err)
	}
	_, err = c.TagResource(
		ctx,
		&cloudfront.TagResourceInput{
			Resource: a.KeyValueStore.ARN,
			Tags:     &types.Tags{Items: []types.Tag{{Key: new("team"), Value: new("core")}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTagsForResource(ctx, &cloudfront.ListTagsForResourceInput{Resource: a.KeyValueStore.ARN})
	if err != nil || len(tags.Tags.Items) != 1 {
		t.Fatalf("tags: %+v %v", tags, err)
	}
	_, err = c.UntagResource(
		ctx,
		&cloudfront.UntagResourceInput{Resource: a.KeyValueStore.ARN, TagKeys: &types.TagKeys{Items: []string{"team"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteKeyValueStore(ctx, &cloudfront.DeleteKeyValueStoreInput{Name: new("alpha"), IfMatch: updated.ETag})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DescribeKeyValueStore(ctx, &cloudfront.DescribeKeyValueStoreInput{Name: new("alpha")})
	wantAccessAnalyzerError(t, err, "EntityNotFound")
}
