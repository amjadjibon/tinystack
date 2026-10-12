package xray_test

import (
	"context"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/xray"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
}

func testService() *xray.Service {
	return xray.New(memory.New(nil, nil))
}

func TestGroupLifecycle(t *testing.T) {
	ctx := testContext()
	svc := testService()

	created, err := svc.CreateGroup(ctx, &api.CreateGroupRequest{
		GroupName:        new("my-group"),
		FilterExpression: new("service(\"my-service\")"),
		Tags: []api.Tag{
			{Key: new("env"), Value: new("test")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if *created.Group.GroupName != "my-group" {
		t.Fatalf("expected my-group, got %s", *created.Group.GroupName)
	}

	// Duplicate create should fail
	_, err = svc.CreateGroup(ctx, &api.CreateGroupRequest{
		GroupName: new("my-group"),
	})
	if err == nil {
		t.Fatal("expected duplicate group creation to fail")
	}

	// GetGroup by name
	get, err := svc.GetGroup(ctx, &api.GetGroupRequest{
		GroupName: new("my-group"),
	})
	if err != nil || *get.Group.GroupName != "my-group" {
		t.Fatal(get, err)
	}

	// GetGroup by ARN
	getArn, err := svc.GetGroup(ctx, &api.GetGroupRequest{
		GroupARN: created.Group.GroupARN,
	})
	if err != nil || *getArn.Group.GroupName != "my-group" {
		t.Fatal(getArn, err)
	}

	// UpdateGroup
	updated, err := svc.UpdateGroup(ctx, &api.UpdateGroupRequest{
		GroupName:        new("my-group"),
		FilterExpression: new("service(\"new-service\")"),
	})
	if err != nil || *updated.Group.FilterExpression != "service(\"new-service\")" {
		t.Fatal(updated, err)
	}

	// GetGroups
	list, err := svc.GetGroups(ctx, &api.GetGroupsRequest{})
	if err != nil || len(list.Groups) != 1 {
		t.Fatal(list, err)
	}

	// DeleteGroup
	_, err = svc.DeleteGroup(ctx, &api.DeleteGroupRequest{
		GroupName: new("my-group"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// GetGroup after delete should fail
	_, err = svc.GetGroup(ctx, &api.GetGroupRequest{
		GroupName: new("my-group"),
	})
	if err == nil {
		t.Fatal("expected error getting deleted group")
	}
}

func TestSamplingRuleLifecycle(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// Initially, GetSamplingRules should return the Default rule
	rules, err := svc.GetSamplingRules(ctx, &api.GetSamplingRulesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.SamplingRuleRecords) != 1 {
		t.Fatalf("expected 1 default rule, got %d", len(rules.SamplingRuleRecords))
	}
	if *rules.SamplingRuleRecords[0].SamplingRule.RuleName != "Default" {
		t.Fatalf("expected Default rule, got %s", *rules.SamplingRuleRecords[0].SamplingRule.RuleName)
	}

	// Deleting default rule should fail
	_, err = svc.DeleteSamplingRule(ctx, &api.DeleteSamplingRuleRequest{
		RuleName: new("Default"),
	})
	if err == nil {
		t.Fatal("expected error deleting Default rule")
	}

	// Creating custom rule
	custom, err := svc.CreateSamplingRule(ctx, &api.CreateSamplingRuleRequest{
		SamplingRule: &api.SamplingRule{
			RuleName:      new("custom-1"),
			Priority:      new(int64(100)),
			FixedRate:     new(0.1),
			ReservoirSize: new(int64(5)),
			ServiceName:   new("*"),
			ServiceType:   new("*"),
			Host:          new("*"),
			HTTPMethod:    new("GET"),
			URLPath:       new("/api/*"),
			Version:       new(int64(1)),
			ResourceARN:   new("*"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if *custom.SamplingRuleRecord.SamplingRule.RuleName != "custom-1" {
		t.Fatal(custom)
	}

	// GetSamplingRules should list custom rule first (priority 100 < 10000)
	rules, err = svc.GetSamplingRules(ctx, &api.GetSamplingRulesRequest{})
	if err != nil || len(rules.SamplingRuleRecords) != 2 {
		t.Fatal(rules, err)
	}
	if *rules.SamplingRuleRecords[0].SamplingRule.RuleName != "custom-1" {
		t.Fatalf("expected custom-1 first, got %s", *rules.SamplingRuleRecords[0].SamplingRule.RuleName)
	}

	// UpdateSamplingRule
	updated, err := svc.UpdateSamplingRule(ctx, &api.UpdateSamplingRuleRequest{
		SamplingRuleUpdate: &api.SamplingRuleUpdate{
			RuleName:  new("custom-1"),
			FixedRate: new(0.2),
		},
	})
	if err != nil || *updated.SamplingRuleRecord.SamplingRule.FixedRate != 0.2 {
		t.Fatal(updated, err)
	}

	// Delete custom rule
	_, err = svc.DeleteSamplingRule(ctx, &api.DeleteSamplingRuleRequest{
		RuleName: new("custom-1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	rules, err = svc.GetSamplingRules(ctx, &api.GetSamplingRulesRequest{})
	if err != nil || len(rules.SamplingRuleRecords) != 1 {
		t.Fatal(rules, err)
	}
}

func TestTags(t *testing.T) {
	ctx := testContext()
	svc := testService()

	created, err := svc.CreateGroup(ctx, &api.CreateGroupRequest{
		GroupName: new("tagged-group"),
		Tags: []api.Tag{
			{Key: new("k1"), Value: new("v1")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := created.Group.GroupARN

	// ListTagsForResource
	tags, err := svc.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{
		ResourceARN: arn,
	})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "k1" {
		t.Fatal(tags, err)
	}

	// TagResource
	_, err = svc.TagResource(ctx, &api.TagResourceRequest{
		ResourceARN: arn,
		Tags: []api.Tag{
			{Key: new("k2"), Value: new("v2")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err = svc.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{
		ResourceARN: arn,
	})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	// UntagResource
	_, err = svc.UntagResource(ctx, &api.UntagResourceRequest{
		ResourceARN: arn,
		TagKeys:     []string{"k1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err = svc.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{
		ResourceARN: arn,
	})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "k2" {
		t.Fatal(tags, err)
	}
}

func TestInventory(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateGroup(ctx, &api.CreateGroupRequest{
		GroupName: new("test-grp"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.CreateSamplingRule(ctx, &api.CreateSamplingRuleRequest{
		SamplingRule: &api.SamplingRule{
			RuleName:      new("rule-1"),
			Priority:      new(int64(100)),
			FixedRate:     new(0.1),
			ReservoirSize: new(int64(5)),
			ServiceName:   new("*"),
			ServiceType:   new("*"),
			Host:          new("*"),
			HTTPMethod:    new("GET"),
			URLPath:       new("/api/*"),
			Version:       new(int64(1)),
			ResourceARN:   new("*"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Resources(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res) != 2 {
		t.Fatalf("expected 2 inventory resources, got %d", len(res))
	}

	for _, r := range res {
		d, ok := r.Details.(map[string]any)
		if !ok {
			t.Fatalf("expected map[string]any details, got %T", r.Details)
		}
		if d["RecordsOnly"] != true {
			t.Fatalf("expected RecordsOnly=true for %s", r.Name)
		}
		if d["ExecutionReady"] != false {
			t.Fatalf("expected ExecutionReady=false for %s", r.Name)
		}
	}
}

func samplingRule(name string, priority int64) *api.SamplingRule {
	return &api.SamplingRule{
		RuleName:      new(name),
		Priority:      new(priority),
		FixedRate:     new(0.1),
		ReservoirSize: new(int64(1)),
		ServiceName:   new("*"),
		ServiceType:   new("*"),
		Host:          new("*"),
		HTTPMethod:    new("*"),
		URLPath:       new("*"),
		ResourceARN:   new("*"),
		Version:       new(int64(1)),
	}
}

func TestSamplingRulesListInEvaluationOrder(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// Priorities descend while names ascend, so evaluation order differs from key order.
	for i := range 25 {
		name := string(rune('a'+i%26)) + "rule"
		if _, err := svc.CreateSamplingRule(ctx, &api.CreateSamplingRuleRequest{
			SamplingRule: samplingRule(name, int64(100-i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := svc.GetSamplingRules(ctx, &api.GetSamplingRulesRequest{})
	if err != nil || len(all.SamplingRuleRecords) != 26 || all.NextToken != nil {
		t.Fatal(all, err)
	}

	if *all.SamplingRuleRecords[0].SamplingRule.Priority != 76 ||
		*all.SamplingRuleRecords[25].SamplingRule.RuleName != "Default" {
		t.Fatal("rules must be returned in priority order with Default last")
	}

	if *all.SamplingRuleRecords[25].CreatedAt != 0 {
		t.Fatal("untouched Default rule must keep stable epoch timestamps")
	}
}

func TestDefaultSamplingRuleUpdates(t *testing.T) {
	ctx := testContext()
	svc := testService()

	if _, err := svc.UpdateSamplingRule(ctx, &api.UpdateSamplingRuleRequest{
		SamplingRuleUpdate: &api.SamplingRuleUpdate{RuleName: new("Default"), Host: new("example.com")},
	}); err == nil {
		t.Fatal("Default rule matching fields must not change")
	}

	updated, err := svc.UpdateSamplingRule(ctx, &api.UpdateSamplingRuleRequest{
		SamplingRuleUpdate: &api.SamplingRuleUpdate{RuleName: new("Default"), FixedRate: new(0.5)},
	})
	if err != nil || *updated.SamplingRuleRecord.SamplingRule.FixedRate != 0.5 ||
		*updated.SamplingRuleRecord.SamplingRule.Host != "*" {
		t.Fatal(updated, err)
	}
}

func TestSamplingRuleUpdateValidatesWholeCandidate(t *testing.T) {
	ctx := testContext()
	svc := testService()

	if _, err := svc.CreateSamplingRule(ctx, &api.CreateSamplingRuleRequest{
		SamplingRule: samplingRule("api", 10),
	}); err != nil {
		t.Fatal(err)
	}

	for _, update := range []api.SamplingRuleUpdate{
		{RuleName: new("api"), Host: new("")},
		{RuleName: new("api"), HTTPMethod: new("TOOLONGMETHOD")},
		{RuleName: new("api"), Priority: new(int64(10000))},
		{RuleName: new("api"), FixedRate: new(0.2), Attributes: map[string]string{"k": ""}},
	} {
		_, err := svc.UpdateSamplingRule(ctx, &api.UpdateSamplingRuleRequest{SamplingRuleUpdate: &update})
		if err == nil {
			t.Fatalf("update %+v must fail", update)
		}
	}

	got, err := svc.GetSamplingRules(ctx, &api.GetSamplingRulesRequest{})
	if err != nil || *got.SamplingRuleRecords[0].SamplingRule.FixedRate != 0.1 ||
		*got.SamplingRuleRecords[0].SamplingRule.Host != "*" {
		t.Fatal("failed updates must leave the rule unchanged", got, err)
	}
}

func TestGroupPagesResumeAfterDeletedCursor(t *testing.T) {
	ctx := testContext()
	svc := testService()

	for i := range 52 {
		name := "g" + string(rune('A'+i/26)) + string(rune('a'+i%26))
		if _, err := svc.CreateGroup(ctx, &api.CreateGroupRequest{GroupName: new(name)}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := svc.GetGroups(ctx, &api.GetGroupsRequest{})
	if err != nil || len(first.Groups) != 50 || first.NextToken == nil {
		t.Fatal(first, err)
	}

	if _, err = svc.DeleteGroup(ctx, &api.DeleteGroupRequest{GroupName: first.Groups[49].GroupName}); err != nil {
		t.Fatal(err)
	}

	second, err := svc.GetGroups(ctx, &api.GetGroupsRequest{NextToken: first.NextToken})
	if err != nil || len(second.Groups) != 2 || *second.Groups[0].GroupName != "gBy" {
		t.Fatal(second, err)
	}
}
