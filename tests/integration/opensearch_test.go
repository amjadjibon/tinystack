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

func TestOpenSearchVersionsAndPendingConfig(t *testing.T) {
	ctx := context.Background()
	c := opensearchClient(server(t), "test", "us-east-1")
	name := new("versioned")
	listed, err := c.ListVersions(ctx, &sdk.ListVersionsInput{MaxResults: 2})
	if err != nil || len(listed.Versions) != 2 || listed.NextToken == nil {
		t.Fatalf("versions: %+v %v", listed, err)
	}
	rest, err := c.ListVersions(ctx, &sdk.ListVersionsInput{NextToken: listed.NextToken})
	if err != nil || len(rest.Versions) == 0 || rest.Versions[0] == listed.Versions[0] {
		t.Fatalf("pagination: %+v %v", rest, err)
	}
	all, err := c.GetCompatibleVersions(ctx, &sdk.GetCompatibleVersionsInput{})
	if err != nil || len(all.CompatibleVersions) == 0 {
		t.Fatalf("compatibility: %+v %v", all, err)
	}
	created, err := c.CreateDomain(
		ctx,
		&sdk.CreateDomainInput{
			DomainName:      name,
			EngineVersion:   new("OpenSearch_2.11"),
			AdvancedOptions: map[string]string{"override_main_response_version": "true"},
			EBSOptions: &types.EBSOptions{
				VolumeType: types.VolumeTypeGp3,
				Iops:       new(int32(3000)),
				Throughput: new(int32(125)),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.DomainStatus.AdvancedOptions["override_main_response_version"] != "true" ||
		created.DomainStatus.AdvancedOptions["rest.action.multi_allow_explicit_index"] != "true" {
		t.Fatal(created.DomainStatus.AdvancedOptions)
	}
	before, err := c.DescribeDomainConfig(ctx, &sdk.DescribeDomainConfigInput{DomainName: name})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := c.UpdateDomainConfig(
		ctx,
		&sdk.UpdateDomainConfigInput{
			DomainName:      name,
			AdvancedOptions: map[string]string{"override_main_response_version": "false"},
			EBSOptions:      &types.EBSOptions{VolumeType: types.VolumeTypeGp2},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	option := updated.DomainConfig.AdvancedOptions
	if option.Status.State != types.OptionStateProcessing || option.Status.UpdateVersion != 2 ||
		!option.Status.UpdateDate.After(*before.DomainConfig.AdvancedOptions.Status.UpdateDate) ||
		!option.Status.CreationDate.Equal(*before.DomainConfig.AdvancedOptions.Status.CreationDate) {
		t.Fatalf("option status: %+v", option.Status)
	}
	if updated.DomainConfig.EBSOptions.Options.Iops != nil ||
		updated.DomainConfig.EBSOptions.Options.Throughput != nil ||
		updated.DomainConfig.ClusterConfig.Status.State != types.OptionStateActive {
		t.Fatal(updated.DomainConfig)
	}
	batch, err := c.DescribeDomains(ctx, &sdk.DescribeDomainsInput{DomainNames: []string{*name}})
	if err != nil || !aws.ToBool(batch.DomainStatusList[0].Processing) ||
		len(batch.DomainStatusList[0].ModifyingProperties) != 2 ||
		string(batch.DomainStatusList[0].DomainProcessingStatus) != "Modifying" {
		t.Fatalf("pending batch: %+v %v", batch, err)
	}
	awaitOpenSearchDomain(ctx, t, c, *name)
	active, err := c.DescribeDomainConfig(ctx, &sdk.DescribeDomainConfigInput{DomainName: name})
	if err != nil || active.DomainConfig.AdvancedOptions.Status.State != types.OptionStateActive {
		t.Fatalf("active: %+v %v", active, err)
	}
	if _, updateErr := c.UpdateDomainConfig(
		ctx,
		&sdk.UpdateDomainConfigInput{DomainName: name, AdvancedOptions: map[string]string{"unsupported": "true"}},
	); updateErr == nil {
		t.Fatal("unknown advanced option accepted")
	}
	compatible, err := c.GetCompatibleVersions(ctx, &sdk.GetCompatibleVersionsInput{DomainName: name})
	if err != nil || len(compatible.CompatibleVersions) != 1 ||
		len(compatible.CompatibleVersions[0].TargetVersions) == 0 {
		t.Fatalf("domain compatibility: %+v %v", compatible, err)
	}
	for _, check := range []bool{true, false} {
		_, err = c.UpgradeDomain(
			ctx,
			&sdk.UpgradeDomainInput{
				DomainName:       name,
				TargetVersion:    new("OpenSearch_2.13"),
				PerformCheckOnly: new(check),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		status, statusErr := c.GetUpgradeStatus(ctx, &sdk.GetUpgradeStatusInput{DomainName: name})
		if statusErr != nil || status.StepStatus != types.UpgradeStatusInProgress {
			t.Fatalf("upgrade status: %+v %v", status, statusErr)
		}
		pending := openSearchDomain(ctx, t, c, *name)
		if !aws.ToBool(pending.UpgradeProcessing) || aws.ToString(pending.EngineVersion) != "OpenSearch_2.11" ||
			string(pending.DomainProcessingStatus) != "UpgradingEngineVersion" {
			t.Fatal(pending)
		}
		done := awaitOpenSearchDomain(ctx, t, c, *name)
		want := "OpenSearch_2.13"
		if check {
			want = "OpenSearch_2.11"
		}
		if aws.ToString(done.EngineVersion) != want || string(done.DomainProcessingStatus) != "Active" {
			t.Fatal(done)
		}
	}
	history, err := c.GetUpgradeHistory(ctx, &sdk.GetUpgradeHistoryInput{DomainName: name, MaxResults: 1})
	if err != nil || len(history.UpgradeHistories) != 1 || history.NextToken == nil ||
		history.UpgradeHistories[0].UpgradeStatus != types.UpgradeStatusSucceeded {
		t.Fatalf("history: %+v %v", history, err)
	}
	next, err := c.GetUpgradeHistory(ctx, &sdk.GetUpgradeHistoryInput{DomainName: name, NextToken: history.NextToken})
	if err != nil || len(next.UpgradeHistories) != 1 {
		t.Fatalf("history next: %+v %v", next, err)
	}
	status, statusErr := c.GetUpgradeStatus(ctx, &sdk.GetUpgradeStatusInput{DomainName: name})
	if statusErr != nil || status.StepStatus != types.UpgradeStatusSucceeded {
		t.Fatalf("completed upgrade: %+v %v", status, statusErr)
	}
	if _, invalidErr := c.ListVersions(ctx, &sdk.ListVersionsInput{NextToken: new("!")}); invalidErr == nil {
		t.Fatal("invalid token accepted")
	}
	if _, invalidErr := c.ListVersions(ctx, &sdk.ListVersionsInput{MaxResults: 101}); invalidErr == nil {
		t.Fatal("invalid limit accepted")
	}
	if _, invalidErr := c.GetUpgradeHistory(
		ctx,
		&sdk.GetUpgradeHistoryInput{DomainName: name, NextToken: new("!")},
	); invalidErr == nil {
		t.Fatal("invalid history token accepted")
	}
	if _, invalidErr := c.CreateDomain(
		ctx,
		&sdk.CreateDomainInput{
			DomainName:      new("bad-options"),
			AdvancedOptions: map[string]string{"override_main_response_version": "invalid"},
		},
	); invalidErr == nil {
		t.Fatal("invalid advanced value accepted")
	}
	// Zero explicitly removes provisioned values without changing the volume type.
	if _, updateErr := c.UpdateDomainConfig(
		ctx,
		&sdk.UpdateDomainConfigInput{
			DomainName: name,
			EBSOptions: &types.EBSOptions{
				VolumeType: types.VolumeTypeGp3,
				Iops:       new(int32(3000)),
				Throughput: new(int32(125)),
			},
		},
	); updateErr != nil {
		t.Fatal(updateErr)
	}
	awaitOpenSearchDomain(ctx, t, c, *name)
	cleared, clearErr := c.UpdateDomainConfig(
		ctx,
		&sdk.UpdateDomainConfigInput{
			DomainName: name,
			EBSOptions: &types.EBSOptions{Iops: new(int32(0)), Throughput: new(int32(0))},
		},
	)
	if clearErr != nil || cleared.DomainConfig.EBSOptions.Options.Iops != nil ||
		cleared.DomainConfig.EBSOptions.Options.Throughput != nil {
		t.Fatalf("clear provisioned values: %+v %v", cleared, clearErr)
	}
	awaitOpenSearchDomain(ctx, t, c, *name)

	for _, target := range []string{"OpenSearch_2.13", "OpenSearch_1.3", "nonsense"} {
		if _, upgradeErr := c.UpgradeDomain(
			ctx,
			&sdk.UpgradeDomainInput{DomainName: name, TargetVersion: new(target)},
		); upgradeErr == nil {
			t.Fatalf("accepted invalid target %s", target)
		}
	}
	if _, err := c.GetCompatibleVersions(ctx, &sdk.GetCompatibleVersionsInput{DomainName: new("missing")}); err == nil {
		t.Fatal("missing domain accepted")
	}
}
