package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/opensearch"
	"github.com/aws/aws-sdk-go-v2/service/opensearch/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func opensearchClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func TestOpenSearchSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := opensearchClient(s, "test", "us-east-1")

	// 1. CreateDomain
	created, err := c.CreateDomain(ctx, &sdk.CreateDomainInput{
		DomainName:    aws.String("test-domain"),
		EngineVersion: aws.String("OpenSearch_2.11"),
		ClusterConfig: &types.ClusterConfig{
			InstanceCount: aws.Int32(2),
		},
		EBSOptions: &types.EBSOptions{
			EBSEnabled: aws.Bool(true),
			VolumeType: types.VolumeTypeGp2,
			VolumeSize: aws.Int32(20),
		},
		TagList: []types.Tag{
			{Key: aws.String("Environment"), Value: aws.String("Test")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	domainARN := created.DomainStatus.ARN
	if !strings.HasPrefix(aws.ToString(domainARN), "arn:aws:es:us-east-1:") {
		t.Fatalf("unexpected domain ARN: %s", aws.ToString(domainARN))
	}
	if !strings.Contains(aws.ToString(created.DomainStatus.Endpoint), "search-test-domain-") {
		t.Fatalf("unexpected endpoint: %s", aws.ToString(created.DomainStatus.Endpoint))
	}

	// Duplicate create should fail
	_, err = c.CreateDomain(ctx, &sdk.CreateDomainInput{
		DomainName: aws.String("test-domain"),
	})
	if err == nil {
		t.Fatal("expected duplicate CreateDomain to fail")
	}

	// 2. DescribeDomain
	desc, err := c.DescribeDomain(ctx, &sdk.DescribeDomainInput{
		DomainName: aws.String("test-domain"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(desc.DomainStatus.DomainName) != "test-domain" {
		t.Fatalf("expected domain name test-domain, got %s", aws.ToString(desc.DomainStatus.DomainName))
	}

	// 3. DescribeDomains (batch)
	batchDesc, err := c.DescribeDomains(ctx, &sdk.DescribeDomainsInput{
		DomainNames: []string{"test-domain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(batchDesc.DomainStatusList) != 1 {
		t.Fatalf("expected 1 domain status, got %d", len(batchDesc.DomainStatusList))
	}

	// 4. DescribeDomainConfig
	cfg, err := c.DescribeDomainConfig(ctx, &sdk.DescribeDomainConfigInput{
		DomainName: aws.String("test-domain"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DomainConfig == nil || cfg.DomainConfig.EngineVersion == nil {
		t.Fatal("expected DomainConfig with EngineVersion")
	}

	// 5. ListDomainNames
	list, err := c.ListDomainNames(ctx, &sdk.ListDomainNamesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.DomainNames) != 1 || aws.ToString(list.DomainNames[0].DomainName) != "test-domain" {
		t.Fatalf("unexpected ListDomainNames: %+v", list.DomainNames)
	}

	// 6. UpdateDomainConfig
	upd, err := c.UpdateDomainConfig(ctx, &sdk.UpdateDomainConfigInput{
		DomainName: aws.String("test-domain"),
		ClusterConfig: &types.ClusterConfig{
			InstanceCount: aws.Int32(4),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if *upd.DomainConfig.ClusterConfig.Options.InstanceCount != 4 {
		t.Fatalf("expected 4 instances, got %d", *upd.DomainConfig.ClusterConfig.Options.InstanceCount)
	}

	// 7. Tags (AddTags, ListTags, RemoveTags)
	_, err = c.AddTags(ctx, &sdk.AddTagsInput{
		ARN: domainARN,
		TagList: []types.Tag{
			{Key: aws.String("Project"), Value: aws.String("SearchApp")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{
		ARN: domainARN,
	})
	if err != nil || len(tags.TagList) != 2 {
		t.Fatalf("expected 2 tags, got %+v (err: %v)", tags, err)
	}

	_, err = c.RemoveTags(ctx, &sdk.RemoveTagsInput{
		ARN:     domainARN,
		TagKeys: []string{"Environment"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err = c.ListTags(ctx, &sdk.ListTagsInput{
		ARN: domainARN,
	})
	if err != nil || len(tags.TagList) != 1 || aws.ToString(tags.TagList[0].Key) != "Project" {
		t.Fatalf("expected 1 tag Project, got %+v (err: %v)", tags, err)
	}

	// RGTA Tagging
	taggingClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := taggingClient.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(domainARN)},
		Tags:            map[string]string{"rgta": "works"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("RGTA TagResources failed: %+v (err: %v)", tagged, err)
	}

	tags, err = c.ListTags(ctx, &sdk.ListTagsInput{
		ARN: domainARN,
	})
	if err != nil || len(tags.TagList) != 2 {
		t.Fatalf("expected 2 tags after RGTA, got %+v", tags)
	}

	// 8. DeleteDomain
	deleted, err := c.DeleteDomain(ctx, &sdk.DeleteDomainInput{
		DomainName: aws.String("test-domain"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !aws.ToBool(deleted.DomainStatus.Deleted) {
		t.Fatal("expected Deleted to be true")
	}

	// Post delete checks
	_, err = c.DescribeDomain(ctx, &sdk.DescribeDomainInput{
		DomainName: aws.String("test-domain"),
	})
	if err == nil {
		t.Fatal("expected DescribeDomain to fail after delete")
	}
}
