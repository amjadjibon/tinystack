package accessanalyzer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/accessanalyzer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/accessanalyzer"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
}

func TestAnalyzerLifecycle(t *testing.T) {
	s := accessanalyzer.New(memory.New(nil, nil))
	ctx := testContext()

	// Create
	out, err := s.CreateAnalyzer(ctx, &api.CreateAnalyzerRequest{
		AnalyzerName: new("test-analyzer"),
		Type:         new("ACCOUNT"),
		Tags:         map[string]string{"env": "test"},
		ArchiveRules: []api.InlineArchiveRule{
			{
				RuleName: new("rule-1"),
				Filter: map[string]api.Criterion{
					"isPublic": {
						Eq: []string{"false"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if out.Arn == nil || *out.Arn == "" {
		t.Fatal("expected non-empty ARN")
	}

	// Conflict on duplicate name
	_, err = s.CreateAnalyzer(ctx, &api.CreateAnalyzerRequest{
		AnalyzerName: new("test-analyzer"),
		Type:         new("ACCOUNT"),
	})
	wantCode(t, err, "ConflictException")

	// Inline archive rule names must be unique.
	rule := api.InlineArchiveRule{
		RuleName: new("twice"),
		Filter:   map[string]api.Criterion{"isPublic": {Eq: []string{"true"}}},
	}
	_, err = s.CreateAnalyzer(ctx, &api.CreateAnalyzerRequest{
		AnalyzerName: new("duplicate-rules"),
		Type:         new("ACCOUNT"),
		ArchiveRules: []api.InlineArchiveRule{rule, rule},
	})
	wantCode(t, err, "ValidationException")

	// Model constraints are checked by the generated Invoke.
	_, err = api.Descriptor(s).Operations["CreateAnalyzer"].Invoke(ctx, &api.CreateAnalyzerRequest{
		AnalyzerName: new("invalid-type"),
		Type:         new("UNKNOWN"),
	})
	wantCode(t, err, "ValidationException")

	// Validation on invalid unused access age
	_, err = s.CreateAnalyzer(ctx, &api.CreateAnalyzerRequest{
		AnalyzerName: new("invalid-age"),
		Type:         new("ACCOUNT_UNUSED_ACCESS"),
		Configuration: &api.AnalyzerConfiguration{
			UnusedAccess: &api.UnusedAccessConfiguration{
				UnusedAccessAge: new(int64(400)),
			},
		},
	})
	wantCode(t, err, "ValidationException")

	// GetAnalyzer
	getOut, err := s.GetAnalyzer(ctx, &api.GetAnalyzerRequest{
		AnalyzerName: new("test-analyzer"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if *getOut.Analyzer.Name != "test-analyzer" {
		t.Fatalf("want test-analyzer, got %s", *getOut.Analyzer.Name)
	}

	if *getOut.Analyzer.Status != "ACTIVE" {
		t.Fatalf("want ACTIVE, got %s", *getOut.Analyzer.Status)
	}

	if getOut.Analyzer.Tags["env"] != "test" {
		t.Fatalf("want tag env=test, got %v", getOut.Analyzer.Tags)
	}

	// ListAnalyzers
	listOut, err := s.ListAnalyzers(ctx, &api.ListAnalyzersRequest{
		MaxResults: new(int64(10)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(listOut.Analyzers) != 1 {
		t.Fatalf("want 1 analyzer, got %d", len(listOut.Analyzers))
	}

	// ListAnalyzers with type filter
	listFiltered, err := s.ListAnalyzers(ctx, &api.ListAnalyzersRequest{
		Type: new("ORGANIZATION"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(listFiltered.Analyzers) != 0 {
		t.Fatalf("want 0 analyzers for ORGANIZATION, got %d", len(listFiltered.Analyzers))
	}

	// UpdateAnalyzer
	updateOut, err := s.UpdateAnalyzer(ctx, &api.UpdateAnalyzerRequest{
		AnalyzerName: new("test-analyzer"),
		Configuration: &api.AnalyzerConfiguration{
			UnusedAccess: &api.UnusedAccessConfiguration{
				UnusedAccessAge: new(int64(90)),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if updateOut.Configuration == nil || *updateOut.Configuration.UnusedAccess.UnusedAccessAge != 90 {
		t.Fatal("expected updated unusedAccessAge to be 90")
	}

	// DeleteAnalyzer
	_, err = s.DeleteAnalyzer(ctx, &api.DeleteAnalyzerRequest{
		AnalyzerName: new("test-analyzer"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Get after delete -> ResourceNotFoundException
	_, err = s.GetAnalyzer(ctx, &api.GetAnalyzerRequest{
		AnalyzerName: new("test-analyzer"),
	})
	wantCode(t, err, "ResourceNotFoundException")

	// Archive rule created inline should also be deleted
	_, err = s.GetArchiveRule(ctx, &api.GetArchiveRuleRequest{
		AnalyzerName: new("test-analyzer"),
		RuleName:     new("rule-1"),
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestArchiveRuleLifecycle(t *testing.T) {
	s := accessanalyzer.New(memory.New(nil, nil))
	ctx := testContext()

	_, err := s.CreateAnalyzer(ctx, &api.CreateAnalyzerRequest{
		AnalyzerName: new("my-analyzer"),
		Type:         new("ACCOUNT"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// CreateArchiveRule
	_, err = s.CreateArchiveRule(ctx, &api.CreateArchiveRuleRequest{
		AnalyzerName: new("my-analyzer"),
		RuleName:     new("rule-alpha"),
		Filter: map[string]api.Criterion{
			"isPublic": {
				Eq: []string{"false"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Duplicate rule -> ConflictException
	_, err = s.CreateArchiveRule(ctx, &api.CreateArchiveRuleRequest{
		AnalyzerName: new("my-analyzer"),
		RuleName:     new("rule-alpha"),
		Filter: map[string]api.Criterion{
			"isPublic": {
				Eq: []string{"false"},
			},
		},
	})
	wantCode(t, err, "ConflictException")

	// Invalid filter -> ValidationException
	_, err = s.CreateArchiveRule(ctx, &api.CreateArchiveRuleRequest{
		AnalyzerName: new("my-analyzer"),
		RuleName:     new("invalid-rule"),
		Filter: map[string]api.Criterion{
			"isPublic": {},
		},
	})
	wantCode(t, err, "ValidationException")

	// GetArchiveRule
	ruleOut, err := s.GetArchiveRule(ctx, &api.GetArchiveRuleRequest{
		AnalyzerName: new("my-analyzer"),
		RuleName:     new("rule-alpha"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if *ruleOut.ArchiveRule.RuleName != "rule-alpha" {
		t.Fatalf("want rule-alpha, got %s", *ruleOut.ArchiveRule.RuleName)
	}

	// ListArchiveRules
	listOut, err := s.ListArchiveRules(ctx, &api.ListArchiveRulesRequest{
		AnalyzerName: new("my-analyzer"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(listOut.ArchiveRules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(listOut.ArchiveRules))
	}

	// UpdateArchiveRule
	_, err = s.UpdateArchiveRule(ctx, &api.UpdateArchiveRuleRequest{
		AnalyzerName: new("my-analyzer"),
		RuleName:     new("rule-alpha"),
		Filter: map[string]api.Criterion{
			"resource": {
				Contains: []string{"arn:aws:s3:::my-bucket"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// ApplyArchiveRule
	arn := "arn:aws:access-analyzer:us-east-1:123456789012:analyzer/my-analyzer"
	_, err = s.ApplyArchiveRule(ctx, &api.ApplyArchiveRuleRequest{
		AnalyzerArn: new(arn),
		RuleName:    new("rule-alpha"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// DeleteArchiveRule
	_, err = s.DeleteArchiveRule(ctx, &api.DeleteArchiveRuleRequest{
		AnalyzerName: new("my-analyzer"),
		RuleName:     new("rule-alpha"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.GetArchiveRule(ctx, &api.GetArchiveRuleRequest{
		AnalyzerName: new("my-analyzer"),
		RuleName:     new("rule-alpha"),
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestFindingsGaps(t *testing.T) {
	s := accessanalyzer.New(memory.New(nil, nil))
	ctx := testContext()

	_, err := s.CreateAnalyzer(ctx, &api.CreateAnalyzerRequest{
		AnalyzerName: new("finding-analyzer"),
		Type:         new("ACCOUNT"),
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := "arn:aws:access-analyzer:us-east-1:123456789012:analyzer/finding-analyzer"

	// ListFindings returns empty
	lf, err := s.ListFindings(ctx, &api.ListFindingsRequest{
		AnalyzerArn: new(arn),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(lf.Findings) != 0 {
		t.Fatalf("expected empty findings, got %d", len(lf.Findings))
	}

	// ListFindingsV2 returns empty
	lf2, err := s.ListFindingsV2(ctx, &api.ListFindingsV2Request{
		AnalyzerArn: new(arn),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(lf2.Findings) != 0 {
		t.Fatalf("expected empty findings v2, got %d", len(lf2.Findings))
	}

	// GetFinding returns ResourceNotFoundException
	_, err = s.GetFinding(ctx, &api.GetFindingRequest{
		AnalyzerArn: new(arn),
		Id:          new("any-id"),
	})
	wantCode(t, err, "ResourceNotFoundException")

	// GetFindingV2 returns ResourceNotFoundException
	_, err = s.GetFindingV2(ctx, &api.GetFindingV2Request{
		AnalyzerArn: new(arn),
		Id:          new("any-id"),
	})
	wantCode(t, err, "ResourceNotFoundException")

	// UpdateFindings returns ResourceNotFoundException
	_, err = s.UpdateFindings(ctx, &api.UpdateFindingsRequest{
		AnalyzerArn: new(arn),
		Ids:         []string{"any-id"},
		Status:      new("ARCHIVED"),
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestTagsAndInventory(t *testing.T) {
	s := accessanalyzer.New(memory.New(nil, nil))
	ctx := testContext()

	_, err := s.CreateAnalyzer(ctx, &api.CreateAnalyzerRequest{
		AnalyzerName: new("tagged-analyzer"),
		Type:         new("ACCOUNT"),
		Tags:         map[string]string{"k1": "v1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := "arn:aws:access-analyzer:us-east-1:123456789012:analyzer/tagged-analyzer"

	// TagResource
	_, err = s.TagResource(ctx, &api.TagResourceRequest{
		ResourceArn: new(arn),
		Tags:        map[string]string{"k2": "v2"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// ListTagsForResource
	tagsOut, err := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{
		ResourceArn: new(arn),
	})
	if err != nil {
		t.Fatal(err)
	}

	if tagsOut.Tags["k1"] != "v1" || tagsOut.Tags["k2"] != "v2" {
		t.Fatalf("expected k1 and k2 tags, got %v", tagsOut.Tags)
	}

	// UntagResource
	_, err = s.UntagResource(ctx, &api.UntagResourceRequest{
		ResourceArn: new(arn),
		TagKeys:     []string{"k1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tagsOut2, err := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{
		ResourceArn: new(arn),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := tagsOut2.Tags["k1"]; ok {
		t.Fatal("expected k1 to be untagged")
	}

	// Inventory
	resources, err := s.Resources(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(resources) != 1 {
		t.Fatalf("want 1 resource in inventory, got %d", len(resources))
	}

	if resources[0].Type != "analyzer" || resources[0].Name != "tagged-analyzer" {
		t.Fatalf("unexpected inventory resource: %+v", resources[0])
	}
}
