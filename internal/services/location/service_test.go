package location

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/location"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func local() context.Context { return scoped("123456789012", "ap-southeast-1") }
func scoped(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func create(t *testing.T, s *Service, name string) *api.CreateTrackerResponse {
	t.Helper()
	r, e := s.CreateTracker(local(), &api.CreateTrackerRequest{TrackerName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func describe(t *testing.T, s *Service, name string) *api.DescribeTrackerResponse {
	t.Helper()
	r, e := s.DescribeTracker(local(), &api.DescribeTrackerRequest{TrackerName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func wantError(t *testing.T, e error, code string, status int) {
	t.Helper()
	var a *awserror.Error
	if !errors.As(e, &a) || a.Code != code || a.StatusCode != status || a.Message == "" {
		t.Fatalf("wanted %s/%d: %v", code, status, e)
	}
}

func TestTrackerCRUDDefaultsScopeOwnershipAndNoWrite(t *testing.T) {
	s := New(memory.New(nil, nil))
	ctx := local()
	in := &api.CreateTrackerRequest{
		TrackerName: new("Test._-123"),
		Description: new("initial"),
		Tags:        map[string]string{"owner": "caller"},
	}
	created, e := s.CreateTracker(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	if *created.TrackerName != *in.TrackerName ||
		*created.TrackerArn != "arn:aws:geo:ap-southeast-1:123456789012:tracker/Test._-123" {
		t.Fatal(created)
	}
	*in.Description = "mutated"
	in.Tags["owner"] = "mutated"
	*created.TrackerName = "wrong"
	*created.TrackerArn = "wrong"
	before := describe(t, s, "Test._-123")
	if *before.Description != "initial" || before.Tags["owner"] != "caller" ||
		*before.PositionFiltering != "TimeBased" ||
		!before.CreateTime.Equal(*before.UpdateTime) {
		t.Fatal(before)
	}
	before.Tags["owner"] = "output-mutation"
	*before.Description = "output-mutation"
	before = describe(t, s, "Test._-123")
	for _, c := range []context.Context{scoped("999999999999", "ap-southeast-1"), scoped("123456789012", "us-west-2")} {
		_, e = s.DescribeTracker(c, &api.DescribeTrackerRequest{TrackerName: new("Test._-123")})
		wantError(t, e, "ResourceNotFoundException", 404)
		_, e = s.CreateTracker(c, &api.CreateTrackerRequest{TrackerName: new("Test._-123")})
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = s.CreateTracker(ctx, &api.CreateTrackerRequest{TrackerName: new("Test._-123")})
	wantError(t, e, "ConflictException", 409)
	u := &api.UpdateTrackerRequest{TrackerName: new("Test._-123"), PositionFiltering: new("DistanceBased")}
	out, e := s.UpdateTracker(ctx, u)
	if e != nil {
		t.Fatal(e)
	}
	if *out.TrackerName != "Test._-123" || *out.TrackerArn != *before.TrackerArn {
		t.Fatal("lost update label", out)
	}
	*u.TrackerName = "wrong"
	*u.PositionFiltering = "wrong"
	*out.TrackerName = "wrong"
	after := describe(t, s, "Test._-123")
	if *after.PositionFiltering != "DistanceBased" || *after.Description != "initial" ||
		!after.CreateTime.Equal(*before.CreateTime) ||
		!after.UpdateTime.After(*before.UpdateTime) {
		t.Fatal(after)
	}
	_, e = s.UpdateTracker(ctx, &api.UpdateTrackerRequest{TrackerName: new("Test._-123")})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(after, describe(t, s, "Test._-123")) {
		t.Fatal("no-op metadata")
	}
	for _, u := range []*api.UpdateTrackerRequest{
		{TrackerName: new("Test._-123"), Description: new("must not write"), PositionFiltering: new("invalid")},
		{TrackerName: new("Test._-123"), Description: new(strings.Repeat("x", 1001))},
	} {
		_, e = s.UpdateTracker(ctx, u)
		wantError(t, e, "ValidationException", 400)
		if !reflect.DeepEqual(after, describe(t, s, "Test._-123")) {
			t.Fatal("invalid partial write")
		}
	}
	_, e = s.UpdateTracker(ctx, &api.UpdateTrackerRequest{TrackerName: new("Test._-123"), Description: new("")})
	if e != nil || *describe(t, s, "Test._-123").Description != "" {
		t.Fatal("clear", e)
	}
	_, e = s.DeleteTracker(ctx, &api.DeleteTrackerRequest{TrackerName: new("Test._-123")})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteTracker(ctx, &api.DeleteTrackerRequest{TrackerName: new("Test._-123")})
	wantError(t, e, "ResourceNotFoundException", 404)
	fresh := create(t, s, "Test._-123")
	if !fresh.CreateTime.After(*before.CreateTime) || *fresh.TrackerArn != *before.TrackerArn {
		t.Fatal("recreation metadata")
	}
}

func TestTrackerFiniteConfigurationAndRequiredMatrix(t *testing.T) {
	for i, filter := range []string{"TimeBased", "DistanceBased", "AccuracyBased"} {
		s := New(memory.New(nil, nil))
		n := fmt.Sprintf("filter-%d", i)
		_, e := s.CreateTracker(
			local(),
			&api.CreateTrackerRequest{
				TrackerName:       new(n),
				PositionFiltering: new(filter),
				Description:       new(strings.Repeat("界", 1000)),
				Tags:              map[string]string{},
			},
		)
		if e != nil || *describe(t, s, n).PositionFiltering != filter {
			t.Fatal(filter, e)
		}
	}
	invalidInputs := []*api.CreateTrackerRequest{
		nil,
		{},
		{TrackerName: new("")},
		{TrackerName: new("has space")},
		{TrackerName: new("斜")},
		{TrackerName: new(strings.Repeat("x", 101))},
		{TrackerName: new("slash/name")},
		{TrackerName: new("valid"), Description: new(strings.Repeat("界", 1001))},
		{TrackerName: new("valid"), Description: new(string([]byte{255}))},
		{TrackerName: new("valid"), PositionFiltering: new("")},
		{TrackerName: new("valid"), PositionFiltering: new("timebased")},
	}
	s := New(memory.New(nil, nil))
	for _, in := range invalidInputs {
		_, e := s.CreateTracker(local(), in)
		wantError(t, e, "ValidationException", 400)
	}
	for _, n := range []string{"a", "-", strings.Repeat("x", 100)} {
		create(t, s, n)
	}
	if _, e := s.CreateTracker(
		context.Background(),
		&api.CreateTrackerRequest{TrackerName: new("no-scope")},
	); e == nil {
		t.Fatal("scope required")
	}
	for _, call := range []func() error{
		func() error { _, e := s.DescribeTracker(local(), nil); return e },
		func() error { _, e := s.UpdateTracker(local(), nil); return e },
		func() error { _, e := s.DeleteTracker(local(), nil); return e },
		func() error { _, e := s.ListTrackers(local(), nil); return e },
		func() error { _, e := s.ListTagsForResource(local(), nil); return e },
		func() error { _, e := s.TagResource(local(), nil); return e },
		func() error { _, e := s.UntagResource(local(), nil); return e },
	} {
		wantError(t, call(), "ValidationException", 400)
	}
}

func TestTrackerTagsARNBoundariesOverflowAndNoOps(t *testing.T) {
	s := New(memory.New(nil, nil))
	r := create(t, s, "tags")
	ctx := local()
	tags := map[string]string{
		strings.Repeat("界", 128): strings.Repeat("語", 256),
		"empty":                  "",
		"spaces allowed":         " +-=.,_:/@",
	}
	_, e := s.TagResource(ctx, &api.TagResourceRequest{ResourceArn: r.TrackerArn, Tags: tags})
	if e != nil {
		t.Fatal(e)
	}
	tags["empty"] = "mutated"
	listed, e := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceArn: r.TrackerArn})
	if e != nil || listed.Tags["empty"] != "" {
		t.Fatal(listed, e)
	}
	listed.Tags["empty"] = "output"
	before := describe(t, s, "tags")
	for _, tags := range []map[string]string{
		{"": "x"},
		{"aws:reserved": "x"},
		{"AWS:reserved": "x"},
		{"!": "x"},
		{strings.Repeat("x", 129): "x"},
		{"key": strings.Repeat("界", 257)},
		{"key": "\n"},
		{"key": string([]byte{255})},
	} {
		_, e = s.TagResource(ctx, &api.TagResourceRequest{ResourceArn: r.TrackerArn, Tags: tags})
		wantError(t, e, "ValidationException", 400)
		if !reflect.DeepEqual(before, describe(t, s, "tags")) {
			t.Fatal("invalid tags wrote")
		}
	}
	for _, resourceARN := range []string{
		"",
		strings.Replace(*r.TrackerArn, ":geo:", ":location:", 1),
		strings.Replace(*r.TrackerArn, "123456789012", "999999999999", 1),
		strings.Replace(*r.TrackerArn, "ap-southeast-1", "us-east-1", 1),
		strings.Replace(*r.TrackerArn, "tracker/", "map/", 1),
		*r.TrackerArn + "/bad",
	} {
		_, e = s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceArn: new(resourceARN)})
		wantError(t, e, "ValidationException", 400)
	}
	_, e = s.ListTagsForResource(
		ctx,
		&api.ListTagsForResourceRequest{
			ResourceArn: new(arn(request.Scope{AccountID: "123456789012", Region: "ap-southeast-1"}, "absent")),
		},
	)
	wantError(t, e, "ResourceNotFoundException", 404)
	fill := map[string]string{}
	for i := range 47 {
		fill[fmt.Sprintf("key-%02d", i)] = "value"
	}
	_, e = s.TagResource(ctx, &api.TagResourceRequest{ResourceArn: r.TrackerArn, Tags: fill})
	if e != nil {
		t.Fatal(e)
	}
	before = describe(t, s, "tags")
	_, e = s.TagResource(
		ctx,
		&api.TagResourceRequest{
			ResourceArn: r.TrackerArn,
			Tags:        map[string]string{"extra": "overflow", "empty": "would change"},
		},
	)
	wantError(t, e, "ValidationException", 400)
	if !reflect.DeepEqual(before, describe(t, s, "tags")) {
		t.Fatal("overflow wrote")
	}
	for _, tags := range []map[string]string{{}, before.Tags} {
		_, e = s.TagResource(ctx, &api.TagResourceRequest{ResourceArn: r.TrackerArn, Tags: tags})
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = s.UntagResource(
		ctx,
		&api.UntagResourceRequest{ResourceArn: r.TrackerArn, TagKeys: []string{"absent", "absent"}},
	)
	if e != nil || !reflect.DeepEqual(before, describe(t, s, "tags")) {
		t.Fatal("tag no-op", e)
	}
	for _, keys := range [][]string{nil, {}, make([]string, 51), {"aws:reserved"}, {""}} {
		_, e = s.UntagResource(ctx, &api.UntagResourceRequest{ResourceArn: r.TrackerArn, TagKeys: keys})
		wantError(t, e, "ValidationException", 400)
	}
	_, e = s.UntagResource(
		ctx,
		&api.UntagResourceRequest{ResourceArn: r.TrackerArn, TagKeys: []string{"empty", "empty"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	after := describe(t, s, "tags")
	if len(after.Tags) != 49 || !after.UpdateTime.After(*before.UpdateTime) {
		t.Fatal(after)
	}
}

func TestTrackerConcurrentUniquenessAndDisjointConfigTagMerges(t *testing.T) {
	s := New(memory.New(nil, nil))
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, e := s.CreateTracker(local(), &api.CreateTrackerRequest{TrackerName: new("concurrent")})
			if e == nil {
				winners.Add(1)
			} else {
				wantError(t, e, "ConflictException", 409)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal(winners.Load())
	}
	r := describe(t, s, "concurrent")
	wg.Go(func() {
		_, e := s.UpdateTracker(
			local(),
			&api.UpdateTrackerRequest{TrackerName: r.TrackerName, Description: new("description")},
		)
		if e != nil {
			t.Error(e)
		}
	})
	wg.Go(func() {
		_, e := s.UpdateTracker(
			local(),
			&api.UpdateTrackerRequest{TrackerName: r.TrackerName, PositionFiltering: new("AccuracyBased")},
		)
		if e != nil {
			t.Error(e)
		}
	})
	for i := range 20 {
		wg.Go(func() {
			_, e := s.TagResource(
				local(),
				&api.TagResourceRequest{
					ResourceArn: r.TrackerArn,
					Tags:        map[string]string{fmt.Sprintf("tag-%d", i): "value"},
				},
			)
			if e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	after := describe(t, s, "concurrent")
	if len(after.Tags) != 20 || *after.Description != "description" || *after.PositionFiltering != "AccuracyBased" ||
		!after.CreateTime.Equal(*r.CreateTime) {
		t.Fatal(after)
	}
}

func TestTrackerPaginationScopeMalformedDeletedAndRecreatedCursor(t *testing.T) {
	s := New(memory.New(nil, nil))
	for _, n := range []string{"z", "b", "a", "c"} {
		create(t, s, n)
	}
	p, e := s.ListTrackers(local(), &api.ListTrackersRequest{MaxResults: new(int64(1))})
	if e != nil || len(p.Entries) != 1 || *p.Entries[0].TrackerName != "a" || p.NextToken == nil {
		t.Fatal(p, e)
	}
	*p.Entries[0].Description = "mutated"
	if *describe(t, s, "a").Description != "" {
		t.Fatal("list ownership")
	}
	for _, c := range []context.Context{scoped("999999999999", "ap-southeast-1"), scoped("123456789012", "us-east-1")} {
		_, e = s.ListTrackers(c, &api.ListTrackersRequest{NextToken: p.NextToken})
		wantError(t, e, "ValidationException", 400)
	}
	for _, tok := range []string{"", "bad!", "e30", *p.NextToken + "extra"} {
		_, e = s.ListTrackers(local(), &api.ListTrackersRequest{NextToken: new(tok)})
		wantError(t, e, "ValidationException", 400)
	}
	for _, size := range []int64{0, -1, 101} {
		_, e = s.ListTrackers(local(), &api.ListTrackersRequest{MaxResults: new(size)})
		wantError(t, e, "ValidationException", 400)
	}
	_, e = s.DeleteTracker(local(), &api.DeleteTrackerRequest{TrackerName: new("a")})
	if e != nil {
		t.Fatal(e)
	}
	next, e := s.ListTrackers(local(), &api.ListTrackersRequest{MaxResults: new(int64(2)), NextToken: p.NextToken})
	if e != nil || len(next.Entries) != 2 || *next.Entries[0].TrackerName != "b" ||
		*next.Entries[1].TrackerName != "c" ||
		next.NextToken == nil {
		t.Fatal(next, e)
	}
	end, e := s.ListTrackers(local(), &api.ListTrackersRequest{NextToken: next.NextToken})
	if e != nil || len(end.Entries) != 1 || *end.Entries[0].TrackerName != "z" || end.NextToken != nil {
		t.Fatal(end, e)
	}
	create(t, s, "a")
	_, e = s.ListTrackers(local(), &api.ListTrackersRequest{NextToken: p.NextToken})
	wantError(t, e, "ValidationException", 400)
	for i := range 101 {
		create(t, s, fmt.Sprintf("d-%03d", i))
	}
	all, e := s.ListTrackers(local(), &api.ListTrackersRequest{})
	if e != nil || len(all.Entries) != 100 || all.NextToken == nil {
		t.Fatal("default page", e)
	}
}

func TestTrackerCFFinalTagsLiveSnapshotCompensationRevisionAndIncarnation(t *testing.T) {
	s := New(memory.New(nil, nil))
	r := create(t, s, "cf")
	owned, desired := map[string]string{}, map[string]string{}
	for i := range 50 {
		owned[fmt.Sprintf("old-%02d", i)] = "old"
		desired[fmt.Sprintf("new-%02d", i)] = "new"
	}
	_, e := s.TagResource(local(), &api.TagResourceRequest{ResourceArn: r.TrackerArn, Tags: owned})
	if e != nil {
		t.Fatal(e)
	}
	before := describe(t, s, "cf")
	b, a, e := s.CloudFormationUpdate(
		local(),
		&api.UpdateTrackerRequest{TrackerName: new("cf"), Description: new("updated")},
		owned,
		desired,
	)
	if e != nil || len(a.Tracker.Tags) != 50 || !maps.Equal(desired, a.Tracker.Tags) {
		t.Fatal(a, e)
	}
	restored, e := s.CloudFormationRestore(local(), b, a.Revision)
	if e != nil || !reflect.DeepEqual(before, restored.Tracker) || restored.Revision == b.Revision {
		t.Fatal("exact snapshot/fresh revision", e)
	}
	_, e = s.CloudFormationRestore(local(), b, a.Revision)
	wantError(t, e, "ValidationException", 400)
	// A live external key survives owned-key shrink; final overflow rejects all changes.
	_, e = s.UntagResource(local(), &api.UntagResourceRequest{ResourceArn: r.TrackerArn, TagKeys: []string{"old-00"}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{ResourceArn: r.TrackerArn, Tags: map[string]string{"external": "live"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	before = describe(t, s, "cf")
	_, _, e = s.CloudFormationUpdate(
		local(),
		&api.UpdateTrackerRequest{TrackerName: new("cf"), Description: new("overflow")},
		owned,
		desired,
	)
	wantError(t, e, "ValidationException", 400)
	if !reflect.DeepEqual(before, describe(t, s, "cf")) {
		t.Fatal("CF overflow wrote")
	}
	b, a, e = s.CloudFormationUpdate(
		local(),
		&api.UpdateTrackerRequest{TrackerName: new("cf"), Description: new("change")},
		owned,
		map[string]string{},
	)
	if e != nil || !maps.Equal(a.Tracker.Tags, map[string]string{"external": "live"}) {
		t.Fatal(a, e)
	}
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{ResourceArn: r.TrackerArn, Tags: map[string]string{"later": "write"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.CloudFormationRestore(local(), b, a.Revision)
	wantError(t, e, "ValidationException", 400)
	_, e = s.DeleteTracker(local(), &api.DeleteTrackerRequest{TrackerName: r.TrackerName})
	if e != nil {
		t.Fatal(e)
	}
	create(t, s, "cf")
	_, e = s.CloudFormationRestore(local(), b, a.Revision)
	wantError(t, e, "ValidationException", 400)
}

func TestTrackerInventoryExactIdentityFlagsAndOutputOwnership(t *testing.T) {
	s := New(memory.New(nil, nil))
	created := create(t, s, "Inventory._-123")
	_, e := s.TagResource(
		local(),
		&api.TagResourceRequest{ResourceArn: created.TrackerArn, Tags: map[string]string{"owner": "live"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	out, e := s.Resources(context.Background())
	if e != nil || len(out) != 1 {
		t.Fatal(out, e)
	}
	r := out[0]
	if r.Name != *created.TrackerName || r.ARN != *created.TrackerArn || r.Account != "123456789012" ||
		r.Region != "ap-southeast-1" ||
		r.Type != trackerKind {
		t.Fatal(r)
	}
	details, ok := r.Details.(map[string]any)
	if !ok || details["RecordsOnly"] != true || details["ExecutionReady"] != false {
		t.Fatal(r)
	}
	cfg, ok := details["Configuration"].(*api.DescribeTrackerResponse)
	if !ok || *cfg.TrackerName != *created.TrackerName || *cfg.TrackerArn != *created.TrackerArn {
		t.Fatal(details)
	}
	cfg.Tags["owner"] = "metadata mutation"
	r.Tags["owner"] = "inventory mutation"
	if describe(t, s, *created.TrackerName).Tags["owner"] != "live" {
		t.Fatal("inventory retained output ownership")
	}
}
