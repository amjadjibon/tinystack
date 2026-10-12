package opensearch_test

import (
	"context"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/opensearch"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
}

func testService() *opensearch.Service {
	return opensearch.New(memory.New(nil, nil))
}

func TestDomainLifecycle(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// 1. CreateDomain
	created, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName:    new("my-domain"),
		EngineVersion: new("OpenSearch_2.11"),
		ClusterConfig: &api.ClusterConfig{
			InstanceType:  new("m5.large.search"),
			InstanceCount: new(int64(2)),
		},
		EBSOptions: &api.EBSOptions{
			EBSEnabled: new(true),
			VolumeSize: new(int64(20)),
			VolumeType: new("gp3"),
		},
		AccessPolicies: new("{\"Version\":\"2012-10-17\",\"Statement\":[]}"),
		TagList: []api.Tag{
			{Key: new("env"), Value: new("dev")},
		},
	})
	if err != nil {
		t.Fatalf("CreateDomain failed: %v", err)
	}

	status := created.DomainStatus
	if *status.DomainName != "my-domain" {
		t.Fatalf("expected domain name my-domain, got %s", *status.DomainName)
	}

	if *status.DomainId != "123456789012/my-domain" {
		t.Fatalf("expected domain id 123456789012/my-domain, got %s", *status.DomainId)
	}

	if !strings.HasPrefix(*status.ARN, "arn:aws:es:us-east-1:123456789012:domain/my-domain") {
		t.Fatalf("unexpected ARN %s", *status.ARN)
	}

	if !*status.Created || *status.Deleted || *status.Processing {
		t.Fatalf("unexpected lifecycle flags: Created=%v, Deleted=%v, Processing=%v",
			*status.Created, *status.Deleted, *status.Processing)
	}

	if !strings.Contains(*status.Endpoint, "my-domain") {
		t.Fatalf("unexpected endpoint %s", *status.Endpoint)
	}

	if *status.EngineVersion != "OpenSearch_2.11" {
		t.Fatalf("unexpected engine version %s", *status.EngineVersion)
	}

	if *status.ClusterConfig.InstanceCount != 2 {
		t.Fatalf("expected instance count 2, got %d", *status.ClusterConfig.InstanceCount)
	}

	if *status.EBSOptions.VolumeSize != 20 {
		t.Fatalf("expected volume size 20, got %d", *status.EBSOptions.VolumeSize)
	}

	// 2. Duplicate CreateDomain must fail
	_, err = svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName: new("my-domain"),
	})
	if err == nil {
		t.Fatal("expected duplicate CreateDomain to fail")
	}

	// 3. Validation failure
	_, err = svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName: new("Invalid_Name!"),
	})
	if err == nil {
		t.Fatal("expected invalid domain name to fail")
	}

	// Invalid access policies JSON
	_, err = svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName:     new("bad-policy-domain"),
		AccessPolicies: new("invalid json"),
	})
	if err == nil {
		t.Fatal("expected invalid access policies to fail")
	}

	// 4. DescribeDomain
	desc, err := svc.DescribeDomain(ctx, &api.DescribeDomainRequest{
		DomainName: new("my-domain"),
	})
	if err != nil {
		t.Fatalf("DescribeDomain failed: %v", err)
	}

	if *desc.DomainStatus.DomainName != "my-domain" {
		t.Fatalf("expected my-domain, got %s", *desc.DomainStatus.DomainName)
	}

	// Describe non-existent domain
	_, err = svc.DescribeDomain(ctx, &api.DescribeDomainRequest{
		DomainName: new("does-not-exist"),
	})
	if err == nil {
		t.Fatal("expected DescribeDomain for non-existent domain to fail")
	}

	// 5. DescribeDomainConfig
	cfg, err := svc.DescribeDomainConfig(ctx, &api.DescribeDomainConfigRequest{
		DomainName: new("my-domain"),
	})
	if err != nil {
		t.Fatalf("DescribeDomainConfig failed: %v", err)
	}

	if *cfg.DomainConfig.EngineVersion.Options != "OpenSearch_2.11" {
		t.Fatalf("unexpected engine version in config: %s", *cfg.DomainConfig.EngineVersion.Options)
	}

	if *cfg.DomainConfig.EngineVersion.Status.State != "Active" {
		t.Fatalf("expected state Active, got %s", *cfg.DomainConfig.EngineVersion.Status.State)
	}

	// 6. UpdateDomainConfig
	upd, err := svc.UpdateDomainConfig(ctx, &api.UpdateDomainConfigRequest{
		DomainName: new("my-domain"),
		ClusterConfig: &api.ClusterConfig{
			InstanceCount: new(int64(4)),
		},
		EBSOptions: &api.EBSOptions{
			VolumeSize: new(int64(50)),
		},
		DomainEndpointOptions: &api.DomainEndpointOptions{
			EnforceHTTPS: new(true),
		},
	})
	if err != nil {
		t.Fatalf("UpdateDomainConfig failed: %v", err)
	}

	if *upd.DomainConfig.ClusterConfig.Options.InstanceCount != 4 {
		t.Fatalf("expected updated instance count 4, got %d",
			*upd.DomainConfig.ClusterConfig.Options.InstanceCount)
	}

	if *upd.DomainConfig.EBSOptions.Options.VolumeSize != 50 {
		t.Fatalf("expected updated volume size 50, got %d",
			*upd.DomainConfig.EBSOptions.Options.VolumeSize)
	}

	if !*upd.DomainConfig.DomainEndpointOptions.Options.EnforceHTTPS {
		t.Fatalf("expected EnforceHTTPS to be true")
	}

	// Verify DescribeDomain reflects update
	descAfter, err := svc.DescribeDomain(ctx, &api.DescribeDomainRequest{
		DomainName: new("my-domain"),
	})
	if err != nil {
		t.Fatalf("DescribeDomain after update failed: %v", err)
	}

	if *descAfter.DomainStatus.ClusterConfig.InstanceCount != 4 {
		t.Fatalf("expected instance count 4, got %d", *descAfter.DomainStatus.ClusterConfig.InstanceCount)
	}

	// 7. DescribeDomains
	descs, err := svc.DescribeDomains(ctx, &api.DescribeDomainsRequest{
		DomainNames: []string{"my-domain"},
	})
	if err != nil {
		t.Fatalf("DescribeDomains failed: %v", err)
	}

	if len(descs.DomainStatusList) != 1 {
		t.Fatalf("expected 1 domain status, got %d", len(descs.DomainStatusList))
	}

	// DescribeDomains validation: empty names
	_, err = svc.DescribeDomains(ctx, &api.DescribeDomainsRequest{
		DomainNames: []string{},
	})
	if err == nil {
		t.Fatal("expected DescribeDomains with 0 names to fail")
	}

	// DescribeDomains validation: > 5 names
	_, err = svc.DescribeDomains(ctx, &api.DescribeDomainsRequest{
		DomainNames: []string{"d1", "d2", "d3", "d4", "d5", "d6"},
	})
	if err == nil {
		t.Fatal("expected DescribeDomains with 6 names to fail")
	}

	// 8. DeleteDomain
	deleted, err := svc.DeleteDomain(ctx, &api.DeleteDomainRequest{
		DomainName: new("my-domain"),
	})
	if err != nil {
		t.Fatalf("DeleteDomain failed: %v", err)
	}

	if !*deleted.DomainStatus.Deleted || *deleted.DomainStatus.Processing {
		t.Fatalf("unexpected deleted status: Deleted=%v, Processing=%v",
			*deleted.DomainStatus.Deleted, *deleted.DomainStatus.Processing)
	}

	// 9. DescribeDomain after deletion
	_, err = svc.DescribeDomain(ctx, &api.DescribeDomainRequest{
		DomainName: new("my-domain"),
	})
	if err == nil {
		t.Fatal("expected DescribeDomain after delete to fail")
	}
}

func TestListDomainNamesFiltering(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName:    new("os-cluster"),
		EngineVersion: new("OpenSearch_2.11"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName:    new("es-cluster"),
		EngineVersion: new("Elasticsearch_7.10"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Filter OpenSearch
	listOS, err := svc.ListDomainNames(ctx, &api.ListDomainNamesRequest{
		EngineType: new("OpenSearch"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(listOS.DomainNames) != 1 || *listOS.DomainNames[0].DomainName != "os-cluster" {
		t.Fatalf("expected only os-cluster, got %+v", listOS.DomainNames)
	}

	// Filter Elasticsearch
	listES, err := svc.ListDomainNames(ctx, &api.ListDomainNamesRequest{
		EngineType: new("Elasticsearch"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(listES.DomainNames) != 1 || *listES.DomainNames[0].DomainName != "es-cluster" {
		t.Fatalf("expected only es-cluster, got %+v", listES.DomainNames)
	}

	// No filter: returns both sorted
	listAll, err := svc.ListDomainNames(ctx, &api.ListDomainNamesRequest{})
	if err != nil {
		t.Fatal(err)
	}

	if len(listAll.DomainNames) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(listAll.DomainNames))
	}

	if *listAll.DomainNames[0].DomainName != "es-cluster" || *listAll.DomainNames[1].DomainName != "os-cluster" {
		t.Fatalf("expected sorted [es-cluster, os-cluster], got [%s, %s]",
			*listAll.DomainNames[0].DomainName, *listAll.DomainNames[1].DomainName)
	}
}

func TestTags(t *testing.T) {
	ctx := testContext()
	svc := testService()

	created, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName: new("tagged-domain"),
		TagList: []api.Tag{
			{Key: new("stage"), Value: new("alpha")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := created.DomainStatus.ARN

	// ListTags
	listTags, err := svc.ListTags(ctx, &api.ListTagsRequest{ARN: arn})
	if err != nil {
		t.Fatal(err)
	}

	if len(listTags.TagList) != 1 || *listTags.TagList[0].Key != "stage" {
		t.Fatalf("unexpected tags: %+v", listTags.TagList)
	}

	// AddTags
	_, err = svc.AddTags(ctx, &api.AddTagsRequest{
		ARN: arn,
		TagList: []api.Tag{
			{Key: new("owner"), Value: new("alice")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	listTags2, err := svc.ListTags(ctx, &api.ListTagsRequest{ARN: arn})
	if err != nil {
		t.Fatal(err)
	}

	if len(listTags2.TagList) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(listTags2.TagList))
	}

	// RemoveTags
	_, err = svc.RemoveTags(ctx, &api.RemoveTagsRequest{
		ARN:     arn,
		TagKeys: []string{"stage"},
	})
	if err != nil {
		t.Fatal(err)
	}

	listTags3, err := svc.ListTags(ctx, &api.ListTagsRequest{ARN: arn})
	if err != nil {
		t.Fatal(err)
	}

	if len(listTags3.TagList) != 1 || *listTags3.TagList[0].Key != "owner" {
		t.Fatalf("expected only owner tag, got %+v", listTags3.TagList)
	}
}

func TestInventory(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName: new("inventory-domain"),
		TagList: []api.Tag{
			{Key: new("k1"), Value: new("v1")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	resources, err := svc.Resources(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}

	r := resources[0]
	if r.Name != "inventory-domain" {
		t.Fatalf("expected inventory-domain, got %s", r.Name)
	}

	if r.Type != "domain" {
		t.Fatalf("expected type domain, got %s", r.Type)
	}

	if r.Tags["k1"] != "v1" {
		t.Fatalf("expected tag k1=v1, got %v", r.Tags)
	}

	details := r.Details.(map[string]any)
	if details["RecordsOnly"] != true || details["ExecutionReady"] != false {
		t.Fatalf("unexpected details: %v", r.Details)
	}
}

func TestCandidateBuildingRejectedUpdate(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName: new("candidate-test"),
		ClusterConfig: &api.ClusterConfig{
			InstanceCount: new(int64(1)),
		},
		AccessPolicies: new("{\"Version\":\"2012-10-17\"}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Attempt update with invalid AccessPolicies JSON
	_, err = svc.UpdateDomainConfig(ctx, &api.UpdateDomainConfigRequest{
		DomainName: new("candidate-test"),
		ClusterConfig: &api.ClusterConfig{
			InstanceCount: new(int64(5)),
		},
		AccessPolicies: new("not-valid-json"),
	})
	if err == nil {
		t.Fatal("expected update with invalid json to fail")
	}

	// Verify original record was unchanged
	desc, err := svc.DescribeDomain(ctx, &api.DescribeDomainRequest{
		DomainName: new("candidate-test"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if *desc.DomainStatus.ClusterConfig.InstanceCount != 1 {
		t.Fatalf("expected instance count to remain 1 after rejected update, got %d",
			*desc.DomainStatus.ClusterConfig.InstanceCount)
	}
}

func TestDomainConfigurationValidation(t *testing.T) {
	ctx := testContext()
	svc := testService()

	for name, in := range map[string]*api.CreateDomainRequest{
		"instance type":  {ClusterConfig: &api.ClusterConfig{InstanceType: new("m5.large")}},
		"zero instances": {ClusterConfig: &api.ClusterConfig{InstanceCount: new(int64(0))}},
		"odd zone count": {
			ClusterConfig: &api.ClusterConfig{ZoneAwarenessEnabled: new(true), InstanceCount: new(int64(3))},
		},
		"master w/o type":   {ClusterConfig: &api.ClusterConfig{DedicatedMasterEnabled: new(true)}},
		"volume type":       {EBSOptions: &api.EBSOptions{VolumeType: new("st1")}},
		"small volume":      {EBSOptions: &api.EBSOptions{VolumeSize: new(int64(5))}},
		"gp2 throughput":    {EBSOptions: &api.EBSOptions{Throughput: new(int64(250))}},
		"kms w/o at-rest":   {EncryptionAtRestOptions: &api.EncryptionAtRestOptions{KmsKeyId: new("key")}},
		"tls policy":        {DomainEndpointOptions: &api.DomainEndpointOptions{TLSSecurityPolicy: new("TLS-1-3")}},
		"custom endpoint":   {DomainEndpointOptions: &api.DomainEndpointOptions{CustomEndpointEnabled: new(true)}},
		"volume w/o ebs":    {EBSOptions: &api.EBSOptions{EBSEnabled: new(false), VolumeSize: new(int64(20))}},
		"master w/o enable": {ClusterConfig: &api.ClusterConfig{DedicatedMasterCount: new(int64(3))}},
	} {
		in.DomainName = new("invalid")
		if _, err := svc.CreateDomain(ctx, in); err == nil {
			t.Fatalf("%s: invalid configuration accepted", name)
		}
	}

	if _, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName: new("secure"),
		ClusterConfig: &api.ClusterConfig{
			DedicatedMasterEnabled: new(true),
			DedicatedMasterType:    new("m5.large.search"),
			DedicatedMasterCount:   new(int64(3)),
		},
		EncryptionAtRestOptions:     &api.EncryptionAtRestOptions{Enabled: new(true)},
		NodeToNodeEncryptionOptions: &api.NodeToNodeEncryptionOptions{Enabled: new(true)},
	}); err != nil {
		t.Fatal(err)
	}

	for _, update := range []*api.UpdateDomainConfigRequest{
		{EncryptionAtRestOptions: &api.EncryptionAtRestOptions{Enabled: new(false)}},
		{NodeToNodeEncryptionOptions: &api.NodeToNodeEncryptionOptions{Enabled: new(false)}},
	} {
		update.DomainName = new("secure")
		if _, err := svc.UpdateDomainConfig(ctx, update); err == nil {
			t.Fatal("encryption must not be disabled once enabled")
		}
	}

	// Disabling dedicated masters and EBS drops their dependent settings instead of rejecting stale values.
	updated, err := svc.UpdateDomainConfig(ctx, &api.UpdateDomainConfigRequest{
		DomainName:    new("secure"),
		ClusterConfig: &api.ClusterConfig{DedicatedMasterEnabled: new(false)},
		EBSOptions:    &api.EBSOptions{EBSEnabled: new(false)},
	})
	if err != nil || updated.DomainConfig.ClusterConfig.Options.DedicatedMasterCount != nil ||
		updated.DomainConfig.EBSOptions.Options.VolumeSize != nil {
		t.Fatal(updated, err)
	}
}
