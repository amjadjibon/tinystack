package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pipes"
	tagging "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func TestPipesTaggingAPI(t *testing.T) {
	s := server(t)
	c := pipesClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	_, source := queueARN(t, q, "origin")
	_, target := queueARN(t, q, "sink")
	in := sqsPipe("tagged", source, target)
	in.Tags = map[string]string{"team": "core"}
	created, err := c.CreatePipe(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	tags := tagging.New(tagging.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
	found, err := tags.GetResources(ctx, &tagging.GetResourcesInput{ResourceTypeFilters: []string{"pipes"}})
	if err != nil || len(found.ResourceTagMappingList) != 1 ||
		aws.ToString(found.ResourceTagMappingList[0].ResourceARN) != aws.ToString(created.Arn) {
		t.Fatalf("%+v %v", found, err)
	}

	tagged, err := tags.TagResources(ctx, &tagging.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(created.Arn)}, Tags: map[string]string{"env": "dev"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("%+v %v", tagged, err)
	}

	got, err := c.ListTagsForResource(ctx, &pipes.ListTagsForResourceInput{ResourceArn: created.Arn})
	if err != nil || got.Tags["env"] != "dev" || got.Tags["team"] != "core" {
		t.Fatalf("%+v %v", got, err)
	}

	untagged, err := tags.UntagResources(ctx, &tagging.UntagResourcesInput{
		ResourceARNList: []string{aws.ToString(created.Arn)}, TagKeys: []string{"team"},
	})
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatalf("%+v %v", untagged, err)
	}

	got, _ = c.ListTagsForResource(ctx, &pipes.ListTagsForResourceInput{ResourceArn: created.Arn})
	if len(got.Tags) != 1 || got.Tags["env"] != "dev" {
		t.Fatalf("%+v", got)
	}
}
