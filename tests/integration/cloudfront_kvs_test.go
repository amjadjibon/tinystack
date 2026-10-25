package integration_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
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

func TestCloudFrontKVSCloudFormation(t *testing.T) {
	s := server(t)
	c := cfnClient(s)
	ctx := context.Background()
	template := func(comment, tag string) string {
		return fmt.Sprintf(
			`{"Resources":{"Store":{"Type":"AWS::CloudFront::KeyValueStore",
"Properties":{"Name":"stack-kvs"%s,"Tags":[{"Key":"team","Value":%q}]}}},
"Outputs":{"Arn":{"Value":{"Fn::GetAtt":["Store","Arn"]}},
"Id":{"Value":{"Fn::GetAtt":["Store","Id"]}}}}`,
			comment,
			tag,
		)
	}

	_, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("kvs-stack"),
			TemplateBody: new(template(`,"Comment":"original"`, "core")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	initial := wantStatus(ctx, t, c, "kvs-stack", cfntypes.StackStatusCreateComplete)
	ids := outputsOf(new(initial))
	_, err = c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("kvs-stack"), TemplateBody: new(template("", "platform"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	updated := wantStatus(ctx, t, c, "kvs-stack", cfntypes.StackStatusUpdateComplete)
	if !reflect.DeepEqual(ids, outputsOf(new(updated))) {
		t.Fatal("comment/tag change replaced store")
	}

	got, err := cloudfrontClient(
		s,
	).DescribeKeyValueStore(ctx, &cloudfront.DescribeKeyValueStoreInput{Name: new("stack-kvs")})
	if err != nil || aws.ToString(got.KeyValueStore.Comment) != "" {
		t.Fatalf("comment removal %+v %v", got, err)
	}

	tags, err := cloudfrontClient(
		s,
	).ListTagsForResource(ctx, &cloudfront.ListTagsForResourceInput{Resource: got.KeyValueStore.ARN})
	if err != nil || len(tags.Tags.Items) != 1 || aws.ToString(tags.Tags.Items[0].Value) != "platform" {
		t.Fatalf("tags %+v %v", tags, err)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("kvs-stack")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "KVS stack deletion", func() bool {
		_, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("kvs-stack")})
		if describeErr != nil {
			wantAccessAnalyzerError(t, describeErr, "ValidationError")
			return true
		}

		return false
	})
	_, err = cloudfrontClient(
		s,
	).DescribeKeyValueStore(ctx, &cloudfront.DescribeKeyValueStoreInput{Name: new("stack-kvs")})
	wantAccessAnalyzerError(t, err, "EntityNotFound")
}
