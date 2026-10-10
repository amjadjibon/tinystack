package config

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/config"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func configContext() context.Context {
	return request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})
}

func testRule() *api.ConfigRule {
	return &api.ConfigRule{
		ConfigRuleName: new("required-tags"),
		Description:    new("persisted"),
		Source:         &api.Source{Owner: new("AWS"), SourceIdentifier: new("REQUIRED_TAGS")},
		InputParameters: new(
			`{"tag1Key":"Owner"}`,
		),
		Scope: &api.Scope{ComplianceResourceTypes: []string{"AWS::S3::Bucket"}},
	}
}

func TestRuleInvalidUpdatesAreAtomic(t *testing.T) {
	ctx := configContext()
	s := New(&memory.Store{}, nil)
	if _, err := s.PutConfigRule(ctx, &api.PutConfigRuleRequest{ConfigRule: testRule()}); err != nil {
		t.Fatal(err)
	}
	before, err := s.DescribeConfigRules(ctx, &api.DescribeConfigRulesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		change func(*api.ConfigRule)
		name   string
	}{
		{name: "missing source", change: func(r *api.ConfigRule) { r.Source = nil }},
		{name: "missing owner", change: func(r *api.ConfigRule) { r.Source.Owner = nil }},
		{name: "custom owner", change: func(r *api.ConfigRule) { r.Source.Owner = new("CUSTOM_LAMBDA") }},
		{name: "unknown identifier", change: func(r *api.ConfigRule) { r.Source.SourceIdentifier = new("BOGUS") }},
		{name: "bad parameters", change: func(r *api.ConfigRule) { r.InputParameters = new(`[]`) }},
		{name: "empty parameters", change: func(r *api.ConfigRule) { r.InputParameters = new("") }},
		{
			name:   "resource ID without type",
			change: func(r *api.ConfigRule) { r.Scope = &api.Scope{ComplianceResourceId: new("id")} },
		},
		{
			name:   "tag value without key",
			change: func(r *api.ConfigRule) { r.Scope = &api.Scope{TagValue: new("value")} },
		},
		{name: "mixed scope", change: func(r *api.ConfigRule) { r.Scope.TagKey = new("Owner") }},
		{
			name:   "unknown resource type",
			change: func(r *api.ConfigRule) { r.Scope.ComplianceResourceTypes = []string{"not-a-type"} },
		},
		{name: "read-only lifecycle", change: func(r *api.ConfigRule) { r.ConfigRuleState = new("DELETING") }},
		{name: "identity mismatch", change: func(r *api.ConfigRule) {
			r.ConfigRuleId = before.ConfigRules[0].ConfigRuleId
			r.ConfigRuleArn = new("foreign")
		}},
		{name: "empty name with identity", change: func(r *api.ConfigRule) {
			r.ConfigRuleId = before.ConfigRules[0].ConfigRuleId
			r.ConfigRuleName = new("")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRule()
			tc.change(r)
			_, e := s.PutConfigRule(ctx, &api.PutConfigRuleRequest{ConfigRule: r})
			var awsErr *awserror.Error
			if !errors.As(e, &awsErr) {
				t.Fatalf("expected modeled error, got %v", e)
			}
			after, e := s.DescribeConfigRules(ctx, &api.DescribeConfigRulesRequest{})
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("malformed update changed record: %+v", after)
			}
		})
	}
}

func TestRecordingGroupValidation(t *testing.T) {
	cases := []struct {
		group *api.RecordingGroup
		name  string
		valid bool
	}{
		{name: "default", group: nil, valid: true},
		{
			name:  "all",
			group: &api.RecordingGroup{AllSupported: new(true), IncludeGlobalResourceTypes: new(true)},
			valid: true,
		},
		{name: "explicit", group: &api.RecordingGroup{ResourceTypes: []string{"AWS::EC2::Instance"}}, valid: true},
		{name: "empty", group: &api.RecordingGroup{}, valid: false},
		{name: "empty explicit", group: &api.RecordingGroup{AllSupported: new(false)}, valid: false},
		{
			name:  "all and list",
			group: &api.RecordingGroup{AllSupported: new(true), ResourceTypes: []string{"AWS::S3::Bucket"}},
			valid: false,
		},
		{
			name: "global and list",
			group: &api.RecordingGroup{
				IncludeGlobalResourceTypes: new(true),
				ResourceTypes:              []string{"AWS::S3::Bucket"},
			},
			valid: false,
		},
		{
			name:  "duplicates",
			group: &api.RecordingGroup{ResourceTypes: []string{"AWS::S3::Bucket", "AWS::S3::Bucket"}},
			valid: false,
		},
		{
			name:  "unsupported type",
			group: &api.RecordingGroup{ResourceTypes: []string{"AWS::EC2::Unknown"}},
			valid: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkGroup(tc.group); (err == nil) != tc.valid {
				t.Fatalf("group validation: %v", err)
			}
		})
	}
}

func TestConcurrentTagUpdatesAndLimitRollback(t *testing.T) {
	ctx := configContext()
	s := New(&memory.Store{}, nil)
	if _, err := s.PutConfigRule(ctx, &api.PutConfigRuleRequest{ConfigRule: testRule()}); err != nil {
		t.Fatal(err)
	}
	described, _ := s.DescribeConfigRules(ctx, &api.DescribeConfigRulesRequest{})
	arn := described.ConfigRules[0].ConfigRuleArn
	var workers sync.WaitGroup
	for i := range 50 {
		workers.Go(func() {
			_, err := s.TagResource(
				ctx,
				&api.TagResourceRequest{
					ResourceArn: arn,
					Tags:        []api.Tag{{Key: new(fmt.Sprintf("k%02d", i)), Value: new("value")}},
				},
			)
			if err != nil {
				t.Errorf("tag: %v", err)
			}
		})
	}
	workers.Wait()
	_, err := s.TagResource(
		ctx,
		&api.TagResourceRequest{ResourceArn: arn, Tags: []api.Tag{{Key: new("overflow"), Value: new("value")}}},
	)
	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) || awsErr.Code != "TooManyTagsException" {
		t.Fatalf("limit: %v", err)
	}
	tags, err := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 50 {
		t.Fatalf("lost tags: %+v %v", tags, err)
	}
	_, err = s.TagResource(
		ctx,
		&api.TagResourceRequest{
			ResourceArn: arn,
			Tags: []api.Tag{
				{Key: new("k00"), Value: new("changed")},
				{Key: new("aws:reserved"), Value: new("bad")},
			},
		},
	)
	if err == nil {
		t.Fatal("reserved tag accepted")
	}
	tags, _ = s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceArn: arn, Limit: new(int64(1))})
	if common.Value(tags.Tags[0].Value) != "value" {
		t.Fatal("partial tag update")
	}
}
