package mwaa

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
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mwaa"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func scoped(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}
func local() context.Context { return scoped("000000000000", "us-east-1") }

type network struct {
	subnets   map[string]*ec2api.Subnet
	badGroups bool
}

func (n *network) MountSubnet(_ storage.ReadTx, scope request.Scope, id string) (*ec2api.Subnet, error) {
	sn, ok := n.subnets[id]
	if !ok || scope.AccountID != "000000000000" || !strings.HasPrefix(*sn.AvailabilityZone, scope.Region) {
		return nil, errors.New("missing")
	}
	return common.Clone(sn), nil
}

func (n *network) MountGroups(_ storage.ReadTx, _ request.Scope, _ string, ids []string) ([]string, error) {
	if n.badGroups || ids[0] != "sg-00000001" {
		return nil, errors.New("missing/wrong VPC")
	}
	return ids, nil
}

type source struct{ bad bool }

func (s *source) CheckMWAASource(_ storage.ReadTx, scope request.Scope, arn string) error {
	if s.bad || scope.AccountID != "000000000000" || arn != "arn:aws:s3:::mwaa-source" {
		return errors.New("invalid scoped source")
	}
	return nil
}

func netRecords() *network {
	return &network{subnets: map[string]*ec2api.Subnet{
		"subnet-00000001": {
			VpcId:              new("vpc-00000001"),
			AvailabilityZone:   new("us-east-1a"),
			AvailabilityZoneId: new("use1-az1"),
		},
		"subnet-00000002": {
			VpcId:              new("vpc-00000001"),
			AvailabilityZone:   new("us-east-1b"),
			AvailabilityZoneId: new("use1-az2"),
		},
	}}
}

func input() *api.CreateEnvironmentInput {
	return &api.CreateEnvironmentInput{
		Name: new(
			"environment",
		),
		AirflowVersion:   new("2.10.3"),
		ExecutionRoleArn: new("arn:aws:iam::000000000000:role/path/mwaa"),
		SourceBucketArn:  new("arn:aws:s3:::mwaa-source"),
		DagS3Path:        new("dags/team"),
		NetworkConfiguration: &api.NetworkConfiguration{
			SubnetIds:        []string{"subnet-00000001", "subnet-00000002"},
			SecurityGroupIds: []string{"sg-00000001"},
		},
		Tags: map[string]string{"owner": "local"},
	}
}

func wantError(t *testing.T, e error, code string, status int) {
	t.Helper()
	var a *awserror.Error
	if !errors.As(e, &a) || a.Code != code || a.StatusCode != status {
		t.Fatalf("want %s/%d got %v", code, status, e)
	}
}

func get(t *testing.T, s *Service) *api.Environment {
	t.Helper()
	o, e := s.GetEnvironment(local(), &api.GetEnvironmentInput{Name: new("environment")})
	if e != nil {
		t.Fatal(e)
	}
	return o.Environment
}

func create(t *testing.T, s *Service, in *api.CreateEnvironmentInput) *string {
	t.Helper()
	o, e := s.CreateEnvironment(local(), in)
	if e != nil {
		t.Fatal(e)
	}
	return o.Arn
}

func TestCRUDDefaultsOwnershipScopeNoOpAndAtomicCandidate(t *testing.T) {
	s := New(memory.New(nil, nil), netRecords(), &source{})
	in := input()
	arn := create(t, s, in)
	before := get(t, s)
	if *before.MinWorkers != 1 || *before.MaxWorkers != 10 || *before.Schedulers != 2 ||
		*before.EnvironmentClass != "mw1.small" ||
		before.CreatedAt == nil ||
		*arn != "arn:aws:airflow:us-east-1:000000000000:environment/environment" {
		t.Fatal(before)
	}
	*in.DagS3Path = "changed"
	in.NetworkConfiguration.SubnetIds[0] = "bad"
	in.Tags["owner"] = "changed"
	*arn = "changed"
	if !reflect.DeepEqual(before, get(t, s)) {
		t.Fatal("input/response alias")
	}
	out := get(t, s)
	*out.Name = "mutated"
	out.NetworkConfiguration.SubnetIds[0] = "bad"
	out.Tags["x"] = "bad"
	if !reflect.DeepEqual(before, get(t, s)) {
		t.Fatal("output alias")
	}
	_, e := s.CreateEnvironment(local(), input())
	wantError(t, e, "ValidationException", 400)
	for _, ctx := range []context.Context{scoped("111111111111", "us-east-1"), scoped("000000000000", "us-west-2")} {
		_, e = s.GetEnvironment(ctx, &api.GetEnvironmentInput{Name: new("environment")})
		wantError(t, e, "ResourceNotFoundException", 404)
		_, e = s.UpdateEnvironment(ctx, &api.UpdateEnvironmentInput{Name: new("environment"), DagS3Path: new("other")})
		wantError(t, e, "ResourceNotFoundException", 404)
		_, e = s.DeleteEnvironment(ctx, &api.DeleteEnvironmentInput{Name: new("environment")})
		wantError(t, e, "ResourceNotFoundException", 404)
	}
	_, e = s.GetEnvironment(local(), &api.GetEnvironmentInput{Name: before.Arn})
	wantError(t, e, "ValidationException", 400)
	_, e = s.UpdateEnvironment(
		local(),
		&api.UpdateEnvironmentInput{
			Name:       new("environment"),
			MinWorkers: new(int64(11)),
			DagS3Path:  new("bad-candidate"),
		},
	)
	wantError(t, e, "ValidationException", 400)
	if !reflect.DeepEqual(before, get(t, s)) {
		t.Fatal("failed candidate wrote")
	}
	_, e = s.UpdateEnvironment(local(), &api.UpdateEnvironmentInput{Name: new("environment")})
	if e != nil || !reflect.DeepEqual(before, get(t, s)) {
		t.Fatal("no-op", e)
	}
	_, e = s.UpdateEnvironment(
		local(),
		&api.UpdateEnvironmentInput{
			Name:             new("environment"),
			EnvironmentClass: new("mw1.medium"),
			MinWorkers:       new(int64(3)),
			MaxWorkers:       new(int64(17)),
			Schedulers:       new(int64(4)),
			DagS3Path:        new("new/prefix"),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	after := get(t, s)
	if *after.DagS3Path != "new/prefix" || *after.MaxWorkers != 17 ||
		!reflect.DeepEqual(before.CreatedAt, after.CreatedAt) ||
		*after.Name != *before.Name ||
		*after.Arn != *before.Arn {
		t.Fatal(after)
	}
	_, e = s.DeleteEnvironment(local(), &api.DeleteEnvironmentInput{Name: new("environment")})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteEnvironment(local(), &api.DeleteEnvironmentInput{Name: new("environment")})
	wantError(t, e, "ResourceNotFoundException", 404)
}

func TestFiniteMatrixRequiredInvalidAndReferences(t *testing.T) {
	for _, version := range []string{"2.10.3", "2.11.0"} {
		for _, class := range []string{"mw1.small", "mw1.medium"} {
			s := New(memory.New(nil, nil), netRecords(), &source{})
			in := input()
			in.AirflowVersion = new(version)
			in.EnvironmentClass = new(class)
			create(t, s, in)
		}
	}
	cases := map[string]func(*api.CreateEnvironmentInput){
		"name":       func(i *api.CreateEnvironmentInput) { i.Name = new("1bad") },
		"long-name":  func(i *api.CreateEnvironmentInput) { i.Name = new(strings.Repeat("a", 81)) },
		"version":    func(i *api.CreateEnvironmentInput) { i.AirflowVersion = nil },
		"deprecated": func(i *api.CreateEnvironmentInput) { i.AirflowVersion = new("2.6.3") },
		"latest":     func(i *api.CreateEnvironmentInput) { i.AirflowVersion = new("latest") },
		"micro":      func(i *api.CreateEnvironmentInput) { i.EnvironmentClass = new("mw1.micro") },
		"role-long-name": func(i *api.CreateEnvironmentInput) {
			i.ExecutionRoleArn = new("arn:aws:iam::000000000000:role/" + strings.Repeat("a", 65))
		},
		"role-long-path": func(i *api.CreateEnvironmentInput) {
			i.ExecutionRoleArn = new("arn:aws:iam::000000000000:role/" + strings.Repeat("a", 512) + "/mwaa")
		},
		"tags-empty":   func(i *api.CreateEnvironmentInput) { i.Tags = map[string]string{} },
		"role-missing": func(i *api.CreateEnvironmentInput) { i.ExecutionRoleArn = nil },
		"role-account": func(i *api.CreateEnvironmentInput) {
			i.ExecutionRoleArn = new("arn:aws:iam::111111111111:role/mwaa")
		},
		"role-service": func(i *api.CreateEnvironmentInput) {
			i.ExecutionRoleArn = new("arn:aws:sts::000000000000:assumed-role/mwaa")
		},
		"role-partition": func(i *api.CreateEnvironmentInput) {
			i.ExecutionRoleArn = new("arn:aws-cn:iam::000000000000:role/mwaa")
		},
		"role-resource": func(i *api.CreateEnvironmentInput) {
			i.ExecutionRoleArn = new("arn:aws:iam::000000000000:user/mwaa")
		},
		"bucket-missing":  func(i *api.CreateEnvironmentInput) { i.SourceBucketArn = nil },
		"bucket-ref":      func(i *api.CreateEnvironmentInput) { i.SourceBucketArn = new("arn:aws:s3:::unknown") },
		"dag-missing":     func(i *api.CreateEnvironmentInput) { i.DagS3Path = nil },
		"dag-empty":       func(i *api.CreateEnvironmentInput) { i.DagS3Path = new("") },
		"dag-url":         func(i *api.CreateEnvironmentInput) { i.DagS3Path = new("s3://bucket/dags") },
		"dag-long":        func(i *api.CreateEnvironmentInput) { i.DagS3Path = new(strings.Repeat("x", 1025)) },
		"network-missing": func(i *api.CreateEnvironmentInput) { i.NetworkConfiguration = nil },
		"subnet-count": func(i *api.CreateEnvironmentInput) {
			i.NetworkConfiguration.SubnetIds = i.NetworkConfiguration.SubnetIds[:1]
		},
		"subnet-dup": func(i *api.CreateEnvironmentInput) {
			i.NetworkConfiguration.SubnetIds[1] = i.NetworkConfiguration.SubnetIds[0]
		},
		"subnet-format": func(i *api.CreateEnvironmentInput) { i.NetworkConfiguration.SubnetIds[0] = "subnet-bad" },
		"subnet-absent": func(i *api.CreateEnvironmentInput) { i.NetworkConfiguration.SubnetIds[0] = "subnet-99999999" },
		"groups-empty":  func(i *api.CreateEnvironmentInput) { i.NetworkConfiguration.SecurityGroupIds = nil },
		"groups-duplicate": func(i *api.CreateEnvironmentInput) {
			i.NetworkConfiguration.SecurityGroupIds = []string{"sg-00000001", "sg-00000001"}
		},
		"group-format": func(i *api.CreateEnvironmentInput) { i.NetworkConfiguration.SecurityGroupIds = []string{"sg-bad"} },
		"group-missing": func(i *api.CreateEnvironmentInput) {
			i.NetworkConfiguration.SecurityGroupIds = []string{"sg-99999999"}
		},
		"min-zero":       func(i *api.CreateEnvironmentInput) { i.MinWorkers = new(int64(0)) },
		"max-high":       func(i *api.CreateEnvironmentInput) { i.MaxWorkers = new(int64(26)) },
		"min-max":        func(i *api.CreateEnvironmentInput) { i.MinWorkers = new(int64(11)) },
		"scheduler-low":  func(i *api.CreateEnvironmentInput) { i.Schedulers = new(int64(1)) },
		"scheduler-high": func(i *api.CreateEnvironmentInput) { i.Schedulers = new(int64(6)) },
		"tags":           func(i *api.CreateEnvironmentInput) { i.Tags = map[string]string{"empty": ""} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := New(memory.New(nil, nil), netRecords(), &source{})
			in := input()
			mutate(in)
			_, e := s.CreateEnvironment(local(), in)
			wantError(t, e, "ValidationException", 400)
			resources, e := s.Resources(local())
			if e != nil || len(resources) != 0 {
				t.Fatal("invalid creation wrote", e)
			}
		})
	}
	for _, kind := range []string{"vpc", "same-az", "local-zone", "same-zone-id", "unsupported-zone", "groups", "bucket"} {
		t.Run(kind, func(t *testing.T) {
			n := netRecords()
			src := &source{}
			switch kind {
			case "vpc":
				n.subnets["subnet-00000002"].VpcId = new("vpc-00000002")
			case "same-az":
				n.subnets["subnet-00000002"].AvailabilityZone = new("us-east-1a")
			case "local-zone":
				n.subnets["subnet-00000002"].AvailabilityZone = new("us-east-1-bos-1a")
			case "same-zone-id":
				n.subnets["subnet-00000002"].AvailabilityZoneId = new("use1-az1")
			case "unsupported-zone":
				n.subnets["subnet-00000002"].AvailabilityZoneId = new("use1-az3")
			case "groups":
				n.badGroups = true
			case "bucket":
				src.bad = true
			}
			s := New(memory.New(nil, nil), n, src)
			_, e := s.CreateEnvironment(local(), input())
			wantError(t, e, "ValidationException", 400)
		})
	}
	_, e := New(
		memory.New(nil, nil),
		netRecords(),
		&source{},
	).CreateEnvironment(scoped("000000000000", "eu-west-1"), input())
	wantError(t, e, "ValidationException", 400)
}

func TestTagsLimitsARNAndAtomicOwnedReplacementRollback(t *testing.T) {
	s := New(memory.New(nil, nil), netRecords(), &source{})
	arn := create(t, s, input())
	badTags := []map[string]string{
		nil,
		{},
		{"": "x"},
		{"aws:reserved": "x"},
		{"AWS:reserved": "x"},
		{"value": ""},
		{"bad!": "x"},
		{"a": strings.Repeat("a", 257)},
		{strings.Repeat("a", 129): "x"},
		{"a": "tab\t"},
	}
	for _, tags := range badTags {
		before := get(t, s)
		_, e := s.TagResource(local(), &api.TagResourceInput{ResourceArn: arn, Tags: tags})
		wantError(t, e, "ValidationException", 400)
		if !reflect.DeepEqual(before, get(t, s)) {
			t.Fatal("tags wrote")
		}
	}
	for _, ref := range []string{"environment", *arn + "/extra", strings.Replace(*arn, "airflow", "mwaa", 1)} {
		_, e := s.TagResource(local(), &api.TagResourceInput{ResourceArn: new(ref), Tags: map[string]string{"a": "b"}})
		wantError(t, e, "ValidationException", 400)
	}
	foreign := strings.Replace(*arn, "000000000000", "111111111111", 1)
	_, e := s.UntagResource(local(), &api.UntagResourceInput{ResourceArn: new(foreign), TagKeys: []string{"owner"}})
	wantError(t, e, "ResourceNotFoundException", 404)
	for _, keys := range [][]string{nil, {"owner", "owner"}, {"aws:reserved"}, {""}} {
		_, e = s.UntagResource(local(), &api.UntagResourceInput{ResourceArn: arn, TagKeys: keys})
		wantError(t, e, "ValidationException", 400)
	}
	owned := map[string]string{"owner": "local"}
	desired := map[string]string{}
	for i := range 50 {
		desired[fmt.Sprintf("new%d", i)] = "value"
	}
	before, after, _, e := s.CloudFormationUpdate(
		local(),
		&api.UpdateEnvironmentInput{Name: new("environment"), DagS3Path: new("changed")},
		owned,
		desired,
	)
	if e != nil || len(after.Tags) != 50 {
		t.Fatal(e)
	}
	next := map[string]string{}
	for i := range 50 {
		next[fmt.Sprintf("other%d", i)] = "other"
	}
	_, after, rev, e := s.CloudFormationUpdate(
		local(),
		&api.UpdateEnvironmentInput{Name: new("environment")},
		desired,
		next,
	)
	if e != nil || len(after.Tags) != 50 {
		t.Fatal("intermediate overflow", e)
	}
	_, _, _, e = s.CloudFormationUpdate(
		local(),
		&api.UpdateEnvironmentInput{Name: new("environment"), DagS3Path: new("must-not-write")},
		nil,
		map[string]string{"external": "overflow"},
	)
	wantError(t, e, "ValidationException", 400)
	if !reflect.DeepEqual(after, get(t, s)) {
		t.Fatal("overflow wrote config")
	}
	restored, e := s.CloudFormationRestore(local(), before, rev)
	if e != nil || !reflect.DeepEqual(restored, before) {
		t.Fatal("restore live state", e)
	}
	_, e = s.CloudFormationRestore(local(), after, rev)
	wantError(t, e, "ValidationException", 400)
	_, e = s.TagResource(
		local(),
		&api.TagResourceInput{
			ResourceArn: arn,
			Tags:        map[string]string{"日本": "文字", strings.Repeat("x", 128): strings.Repeat("v", 256)},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	tags, e := s.ListTagsForResource(local(), &api.ListTagsForResourceInput{ResourceArn: arn})
	if e != nil {
		t.Fatal(e)
	}
	tags.Tags["owner"] = "alias"
	if get(t, s).Tags["owner"] != "local" {
		t.Fatal("tag alias")
	}
	_, e = s.UntagResource(local(), &api.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"owner", "absent"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := get(t, s).Tags["owner"]; ok {
		t.Fatal("remove")
	}
}

func TestConcurrentUniquenessUpdatesAndTagMerges(t *testing.T) {
	s := New(memory.New(nil, nil), netRecords(), &source{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 20 {
		wg.Go(func() {
			_, e := s.CreateEnvironment(local(), input())
			if e == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal(successes.Load())
	}
	arn := get(t, s).Arn
	errs := make(chan error, 40)
	for i := range 30 {
		wg.Go(func() {
			_, e := s.TagResource(
				local(),
				&api.TagResourceInput{ResourceArn: arn, Tags: map[string]string{fmt.Sprintf("key%d", i): "value"}},
			)
			errs <- e
		})
	}
	wg.Go(func() {
		_, e := s.UpdateEnvironment(
			local(),
			&api.UpdateEnvironmentInput{Name: new("environment"), MinWorkers: new(int64(3))},
		)
		errs <- e
	})
	wg.Go(func() {
		_, e := s.UpdateEnvironment(
			local(),
			&api.UpdateEnvironmentInput{Name: new("environment"), Schedulers: new(int64(4))},
		)
		errs <- e
	})
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	env := get(t, s)
	if len(env.Tags) != 31 || *env.MinWorkers != 3 || *env.Schedulers != 4 {
		t.Fatal("lost concurrent write", env)
	}
}

func TestListPagesBoundsScopeAndDeletedCursor(t *testing.T) {
	s := New(memory.New(nil, nil), netRecords(), &source{})
	for _, name := range []string{"C", "A", "B"} {
		in := input()
		in.Name = new(name)
		create(t, s, in)
	}
	one, e := s.ListEnvironments(local(), &api.ListEnvironmentsInput{MaxResults: new(int64(1))})
	if e != nil || !reflect.DeepEqual(one.Environments, []string{"A"}) || one.NextToken == nil {
		t.Fatal(one, e)
	}
	_, e = s.DeleteEnvironment(local(), &api.DeleteEnvironmentInput{Name: new("A")})
	if e != nil {
		t.Fatal(e)
	}
	two, e := s.ListEnvironments(
		local(),
		&api.ListEnvironmentsInput{MaxResults: new(int64(2)), NextToken: one.NextToken},
	)
	if e != nil || !reflect.DeepEqual(two.Environments, []string{"B", "C"}) || two.NextToken != nil {
		t.Fatal(two, e)
	}
	for _, n := range []int64{0, 26, -1} {
		_, e = s.ListEnvironments(local(), &api.ListEnvironmentsInput{MaxResults: new(n)})
		wantError(t, e, "ValidationException", 400)
	}
	for _, token := range []string{"", "bad", "!!!!"} {
		_, e = s.ListEnvironments(local(), &api.ListEnvironmentsInput{NextToken: new(token)})
		wantError(t, e, "ValidationException", 400)
	}
	_, e = s.ListEnvironments(scoped("111111111111", "us-east-1"), &api.ListEnvironmentsInput{NextToken: one.NextToken})
	wantError(t, e, "ValidationException", 400)
	inv, e := s.Resources(local())
	if e != nil || len(inv) != 2 || inv[0].Details.(map[string]any)["RecordsOnly"] != true ||
		inv[0].Details.(map[string]any)["ExecutionReady"] != false {
		t.Fatal(inv, e)
	}
	if !maps.Equal(getTagsForName(t, s, "B"), map[string]string{"owner": "local"}) {
		t.Fatal("stored tags")
	}
}

func getTagsForName(t *testing.T, s *Service, name string) map[string]string {
	t.Helper()
	o, e := s.GetEnvironment(local(), &api.GetEnvironmentInput{Name: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	return o.Environment.Tags
}

func TestScalarUpdatesRetainCapturedReferencesAndFreshCompensationRevision(t *testing.T) {
	s := New(memory.New(nil, nil), netRecords(), &source{})
	create(t, s, input())
	before := get(t, s)
	s.network = nil
	s.source = &source{bad: true}
	_, e := s.UpdateEnvironment(
		local(),
		&api.UpdateEnvironmentInput{Name: before.Name, DagS3Path: new("retained/references")},
	)
	if e != nil {
		t.Fatal(e)
	}
	after := get(t, s)
	if !reflect.DeepEqual(before.NetworkConfiguration, after.NetworkConfiguration) ||
		*before.SourceBucketArn != *after.SourceBucketArn {
		t.Fatal("captured references changed")
	}
	_, e = s.UpdateEnvironment(
		local(),
		&api.UpdateEnvironmentInput{
			Name:            before.Name,
			SourceBucketArn: new("arn:aws:s3:::other-source"),
			MaxWorkers:      new(int64(24)),
		},
	)
	wantError(t, e, "ValidationException", 400)
	if !reflect.DeepEqual(after, get(t, s)) {
		t.Fatal("invalid reference update wrote")
	}
	snapshot, committed, rev, e := s.CloudFormationUpdate(
		local(),
		&api.UpdateEnvironmentInput{Name: before.Name, MaxWorkers: new(int64(21))},
		nil,
		map[string]string{},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.TagResource(
		local(),
		&api.TagResourceInput{ResourceArn: after.Arn, Tags: map[string]string{"external": "later"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.CloudFormationRestore(local(), snapshot, rev)
	wantError(t, e, "ValidationException", 400)
	live := get(t, s)
	if *live.MaxWorkers != *committed.MaxWorkers || live.Tags["external"] != "later" {
		t.Fatal("stale compensation wrote")
	}
}
