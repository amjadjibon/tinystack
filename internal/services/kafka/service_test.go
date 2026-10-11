package kafka

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
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
		"subnet-00000003": {
			VpcId:              new("vpc-00000001"),
			AvailabilityZone:   new("us-east-1d"),
			AvailabilityZoneId: new("use1-az4"),
		},
	}}
}

func input() *api.CreateClusterRequest {
	return &api.CreateClusterRequest{
		ClusterName:         new("cluster"),
		KafkaVersion:        new("3.8.x"),
		NumberOfBrokerNodes: new(int64(2)),
		BrokerNodeGroupInfo: &api.BrokerNodeGroupInfo{
			ClientSubnets: []string{
				"subnet-00000001",
				"subnet-00000002",
			},
			InstanceType:   new("kafka.m5.large"),
			SecurityGroups: []string{"sg-00000001"},
			StorageInfo:    &api.StorageInfo{EbsStorageInfo: &api.EBSStorageInfo{VolumeSize: new(int64(123))}},
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

func describe(t *testing.T, s *Service, arn *string) *api.ClusterInfo {
	t.Helper()
	o, e := s.DescribeCluster(local(), &api.DescribeClusterRequest{ClusterArn: arn})
	if e != nil {
		t.Fatal(e)
	}
	return o.ClusterInfo
}

func create(t *testing.T, s *Service, in *api.CreateClusterRequest) *string {
	t.Helper()
	o, e := s.CreateCluster(local(), in)
	if e != nil {
		t.Fatal(e)
	}
	return o.ClusterArn
}

func TestCRUDDefaultsOwnershipRevisionAndErrors(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	ctx := local()
	in := input()
	arn := create(t, s, in)
	b := describe(t, s, arn)
	if !arnPattern.MatchString(*arn) ||
		!strings.HasPrefix(*arn, "arn:aws:kafka:us-east-1:000000000000:cluster/cluster/") ||
		b.CreationTime == nil ||
		common.Value(b.CurrentVersion) == "" ||
		*b.EnhancedMonitoring != "DEFAULT" ||
		*b.BrokerNodeGroupInfo.BrokerAZDistribution != "DEFAULT" ||
		*b.CurrentBrokerSoftwareInfo.KafkaVersion != "3.8.x" {
		t.Fatal("identity/defaults", b)
	}
	if in.EnhancedMonitoring != nil || in.BrokerNodeGroupInfo.BrokerAZDistribution != nil {
		t.Fatal("input mutated")
	}
	in.Tags["owner"] = "caller"
	in.BrokerNodeGroupInfo.ClientSubnets[0] = "changed"
	b.Tags["owner"] = "output"
	*b.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize = 456
	before := describe(t, s, arn)
	if before.Tags["owner"] != "local" || *before.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize != 123 {
		t.Fatal("ownership")
	}
	_, e := s.CreateCluster(ctx, input())
	wantError(t, e, "ConflictException", 409)
	_, e = s.UpdateMonitoring(
		ctx,
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     before.CurrentVersion,
			EnhancedMonitoring: new("PER_TOPIC_PER_PARTITION"),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	after := describe(t, s, arn)
	if *after.CurrentVersion == *before.CurrentVersion || *after.EnhancedMonitoring != "PER_TOPIC_PER_PARTITION" ||
		!after.CreationTime.Equal(*before.CreationTime) ||
		*after.ClusterArn != *before.ClusterArn {
		t.Fatal("update")
	}
	for _, in := range []*api.UpdateMonitoringRequest{
		{ClusterArn: arn, CurrentVersion: before.CurrentVersion, EnhancedMonitoring: new("DEFAULT")},
		{ClusterArn: arn, EnhancedMonitoring: new("DEFAULT")},
		{ClusterArn: arn, CurrentVersion: after.CurrentVersion},
		{ClusterArn: arn, CurrentVersion: after.CurrentVersion, EnhancedMonitoring: new("BAD")},
	} {
		_, e = s.UpdateMonitoring(ctx, in)
		wantError(t, e, "BadRequestException", 400)
		if !reflect.DeepEqual(after, describe(t, s, arn)) {
			t.Fatal("failed update wrote")
		}
	}
	_, e = s.UpdateMonitoring(
		ctx,
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     after.CurrentVersion,
			EnhancedMonitoring: after.EnhancedMonitoring,
		},
	)
	if e != nil || !reflect.DeepEqual(after, describe(t, s, arn)) {
		t.Fatal("explicit no-op semantics", e)
	}
	for _, other := range []context.Context{scoped("111111111111", "us-east-1"), scoped("000000000000", "us-west-2")} {
		_, e = s.DescribeCluster(other, &api.DescribeClusterRequest{ClusterArn: arn})
		wantError(t, e, "NotFoundException", 404)
		_, e = s.UpdateMonitoring(
			other,
			&api.UpdateMonitoringRequest{
				ClusterArn:         arn,
				CurrentVersion:     after.CurrentVersion,
				EnhancedMonitoring: new("DEFAULT"),
			},
		)
		wantError(t, e, "NotFoundException", 404)
		_, e = s.TagResource(other, &api.TagResourceRequest{ResourceArn: arn, Tags: map[string]string{"x": "y"}})
		wantError(t, e, "NotFoundException", 404)
		_, e = s.DeleteCluster(other, &api.DeleteClusterRequest{ClusterArn: arn})
		wantError(t, e, "NotFoundException", 404)
	}
	for _, bad := range []string{
		"cluster",
		strings.Split(*arn,
			"/")[2],
		strings.Replace(*arn,
			"arn:aws:",
			"arn:aws-cn:",
			1),
	} {
		_, e = s.DeleteCluster(ctx, &api.DeleteClusterRequest{ClusterArn: new(bad)})
		wantError(t, e, "BadRequestException", 400)
	}
	wrongName := strings.Replace(*arn, "/cluster/", "/another/", 1)
	_, e = s.DeleteCluster(ctx, &api.DeleteClusterRequest{ClusterArn: new(wrongName)})
	wantError(t, e, "NotFoundException", 404)
	_, e = s.DeleteCluster(ctx, &api.DeleteClusterRequest{ClusterArn: arn, CurrentVersion: before.CurrentVersion})
	wantError(t, e, "BadRequestException", 400)
	if !reflect.DeepEqual(after, describe(t, s, arn)) {
		t.Fatal("error mutated record")
	}
	records, e := s.Resources(ctx)
	if e != nil || len(records) != 1 || records[0].Details.(map[string]any)["RecordsOnly"] != true ||
		records[0].Details.(map[string]any)["ExecutionReady"] != false {
		t.Fatal("inventory", e)
	}
	_, e = s.DeleteCluster(ctx, &api.DeleteClusterRequest{ClusterArn: arn, CurrentVersion: after.CurrentVersion})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteCluster(ctx, &api.DeleteClusterRequest{ClusterArn: arn})
	wantError(t, e, "NotFoundException", 404)
	fresh := create(t, s, input())
	if *fresh == *arn {
		t.Fatal("recreated incarnation reused identity")
	}
}

func TestFiniteCreationMatrixAndInvalidCandidates(t *testing.T) {
	for _, region := range []string{"us-east-1", "us-west-1", "us-west-2"} {
		for _, version := range []string{"3.8.x", "3.9.x"} {
			for _, host := range []string{"kafka.m5.large", "kafka.m5.xlarge"} {
				for _, count := range []int{2, 3} {
					n := netRecords()
					in := input()
					in.KafkaVersion = new(version)
					in.BrokerNodeGroupInfo.InstanceType = new(host)
					if count == 3 {
						in.BrokerNodeGroupInfo.ClientSubnets = append(
							in.BrokerNodeGroupInfo.ClientSubnets,
							"subnet-00000003",
						)
					}
					in.NumberOfBrokerNodes = new(int64(count * 5))
					for _, sn := range n.subnets {
						*sn.AvailabilityZone = strings.Replace(*sn.AvailabilityZone, "us-east-1", region, 1)
					}
					_, e := New(&memory.Store{}, n).CreateCluster(scoped("000000000000", region), in)
					if region == "us-west-1" && count == 3 {
						wantError(t, e, "BadRequestException", 400)
					} else if e != nil {
						t.Fatal(region, version, host, count, e)
					}
				}
			}
		}
	}
	changes := map[string]func(*api.CreateClusterRequest){
		"name":        func(in *api.CreateClusterRequest) { in.ClusterName = new("") },
		"name-format": func(in *api.CreateClusterRequest) { in.ClusterName = new("bad/name") },
		"name-long":   func(in *api.CreateClusterRequest) { in.ClusterName = new(strings.Repeat("a", 65)) },
		"version":     func(in *api.CreateClusterRequest) { in.KafkaVersion = nil },
		"old-version": func(in *api.CreateClusterRequest) { in.KafkaVersion = new("3.7.x") },
		"kraft":       func(in *api.CreateClusterRequest) { in.KafkaVersion = new("3.9.x.kraft") },
		"nodes":       func(in *api.CreateClusterRequest) { in.NumberOfBrokerNodes = nil },
		"too-many":    func(in *api.CreateClusterRequest) { in.NumberOfBrokerNodes = new(int64(16)) },
		"uneven":      func(in *api.CreateClusterRequest) { in.NumberOfBrokerNodes = new(int64(3)) },
		"group":       func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo = nil },
		"host":        func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo.InstanceType = new("express.m7g.large") },
		"subnets":     func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo.ClientSubnets = nil },
		"duplicate-subnet": func(in *api.CreateClusterRequest) {
			in.BrokerNodeGroupInfo.ClientSubnets[1] = in.BrokerNodeGroupInfo.ClientSubnets[0]
		},
		"missing-subnet": func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo.ClientSubnets[1] = "subnet-000000ff" },
		"groups":         func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo.SecurityGroups = nil },
		"missing-group": func(in *api.CreateClusterRequest) {
			in.BrokerNodeGroupInfo.SecurityGroups = []string{"sg-000000ff"}
		},
		"duplicate-group": func(in *api.CreateClusterRequest) {
			in.BrokerNodeGroupInfo.SecurityGroups = []string{"sg-00000001", "sg-00000001"}
		},
		"storage": func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo.StorageInfo = nil },
		"ebs":     func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo = nil },
		"size":    func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize = nil },
		"low-volume": func(in *api.CreateClusterRequest) {
			in.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize = new(int64(0))
		},
		"high-volume": func(in *api.CreateClusterRequest) {
			in.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize = new(int64(16385))
		},
		"monitor":      func(in *api.CreateClusterRequest) { in.EnhancedMonitoring = new("bad") },
		"distribution": func(in *api.CreateClusterRequest) { in.BrokerNodeGroupInfo.BrokerAZDistribution = new("bad") },
		"reserved":     func(in *api.CreateClusterRequest) { in.Tags = map[string]string{"AWS:key": "x"} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s := New(&memory.Store{}, netRecords())
			in := input()
			change(in)
			_, e := s.CreateCluster(local(), in)
			wantError(t, e, "BadRequestException", 400)
			list, e := s.ListClusters(local(), &api.ListClustersRequest{})
			if e != nil || len(list.ClusterInfoList) != 0 {
				t.Fatal("invalid creation wrote")
			}
		})
	}
	for _, size := range []int64{1, 16384} {
		in := input()
		in.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize = new(size)
		create(t, New(&memory.Store{}, netRecords()), in)
	}
	for name, change := range map[string]func(*network){
		"cross-vpc":       func(n *network) { n.subnets["subnet-00000002"].VpcId = new("vpc-00000002") },
		"same-az":         func(n *network) { n.subnets["subnet-00000002"].AvailabilityZone = new("us-east-1a") },
		"wrong-region":    func(n *network) { n.subnets["subnet-00000002"].AvailabilityZone = new("us-west-2a") },
		"local-zone":      func(n *network) { n.subnets["subnet-00000002"].AvailabilityZone = new("us-east-1-lax-1a") },
		"forbidden-zone":  func(n *network) { n.subnets["subnet-00000002"].AvailabilityZoneId = new("use1-az3") },
		"wrong-group-vpc": func(n *network) { n.badGroups = true },
	} {
		t.Run(name, func(t *testing.T) {
			n := netRecords()
			change(n)
			_, e := New(&memory.Store{}, n).CreateCluster(local(), input())
			wantError(t, e, "BadRequestException", 400)
		})
	}
	_, e := New(&memory.Store{}, netRecords()).CreateCluster(scoped("000000000000", "eu-west-1"), input())
	wantError(t, e, "BadRequestException", 400)
	_, e = New(&memory.Store{}, netRecords()).CreateCluster(scoped("111111111111", "us-east-1"), input())
	wantError(t, e, "BadRequestException", 400)
}

func TestConcurrentUniquenessCASAndTagMerges(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	var wg sync.WaitGroup
	var creates atomic.Int32
	for range 24 {
		wg.Go(func() {
			_, e := s.CreateCluster(local(), input())
			if e == nil {
				creates.Add(1)
			} else {
				wantError(t, e, "ConflictException", 409)
			}
		})
	}
	wg.Wait()
	if creates.Load() != 1 {
		t.Fatal("uniqueness", creates.Load())
	}
	rows, _ := s.ListClusters(local(), &api.ListClustersRequest{})
	arn := rows.ClusterInfoList[0].ClusterArn
	b := describe(t, s, arn)
	var wins atomic.Int32
	start := make(chan struct{})
	for range 24 {
		wg.Go(func() {
			<-start
			_, e := s.UpdateMonitoring(
				local(),
				&api.UpdateMonitoringRequest{
					ClusterArn:         arn,
					CurrentVersion:     b.CurrentVersion,
					EnhancedMonitoring: new("PER_BROKER"),
				},
			)
			if e == nil {
				wins.Add(1)
			} else {
				wantError(t, e, "BadRequestException", 400)
			}
		})
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("CAS committed", wins.Load())
	}
	for i := range 24 {
		wg.Go(func() {
			_, e := s.TagResource(
				local(),
				&api.TagResourceRequest{ResourceArn: arn, Tags: map[string]string{fmt.Sprintf("k%02d", i): "v"}},
			)
			if e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	after := describe(t, s, arn)
	if len(after.Tags) != 25 {
		t.Fatal("lost merge", after.Tags)
	}
}

func TestTagsBoundariesAtomicCFAndCompensation(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	arn := create(t, s, input())
	b := describe(t, s, arn)
	for _, tags := range []map[string]string{
		nil,
		{"": "v"},
		{strings.Repeat("a",
			129): "v"},
		{"a": strings.Repeat("a",
			256)},
		{"aws:a": "v"},
		{"a": "!"},
		{"a:b": "v"},
	} {
		_, e := s.TagResource(local(), &api.TagResourceRequest{ResourceArn: arn, Tags: tags})
		wantError(t, e, "BadRequestException", 400)
		if !reflect.DeepEqual(b, describe(t, s, arn)) {
			t.Fatal("failed tags wrote")
		}
	}
	_, e := s.TagResource(local(), &api.TagResourceRequest{ResourceArn: arn, Tags: map[string]string{}})
	if e != nil {
		t.Fatal("empty map no-op", e)
	}
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{ResourceArn: arn, Tags: map[string]string{"owner": "replaced", "é whitespace": ""}},
	)
	if e != nil {
		t.Fatal(e)
	}
	if *b.CurrentVersion != *describe(t, s, arn).CurrentVersion {
		t.Fatal("tag endpoint changed configuration revision")
	}
	_, e = s.UntagResource(
		local(),
		&api.UntagResourceRequest{ResourceArn: arn, TagKeys: []string{"é whitespace", "absent"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	for _, keys := range [][]string{nil, {"owner", "owner"}, {"aws:key"}, {""}} {
		_, e = s.UntagResource(local(), &api.UntagResourceRequest{ResourceArn: arn, TagKeys: keys})
		wantError(t, e, "BadRequestException", 400)
	}
	full := map[string]string{"owner": "replaced"}
	for i := range 49 {
		full[fmt.Sprintf("k%02d", i)] = "v"
	}
	_, e = s.TagResource(local(), &api.TagResourceRequest{ResourceArn: arn, Tags: full})
	if e != nil {
		t.Fatal(e)
	}
	before := describe(t, s, arn)
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{ResourceArn: arn, Tags: map[string]string{"owner": "changed", "overflow": "v"}},
	)
	wantError(t, e, "BadRequestException", 400)
	_, _, e = s.CloudFormationUpdate(
		local(),
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     before.CurrentVersion,
			EnhancedMonitoring: new("PER_BROKER"),
		},
		nil,
		map[string]string{"overflow": "v"},
	)
	wantError(t, e, "BadRequestException", 400)
	if !reflect.DeepEqual(before, describe(t, s, arn)) {
		t.Fatal("overflow partial write")
	}
	desired := map[string]string{}
	for i := range 50 {
		desired[fmt.Sprintf("new%02d", i)] = ""
	}
	snapshot, after, e := s.CloudFormationUpdate(
		local(),
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     before.CurrentVersion,
			EnhancedMonitoring: new("PER_BROKER"),
		},
		full,
		desired,
	)
	if e != nil || len(after.Tags) != 50 {
		t.Fatal("final set replacement", e)
	}
	restored, e := s.CloudFormationRestore(local(), snapshot, *after.CurrentVersion)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(restored.Tags, before.Tags) || *restored.EnhancedMonitoring != *before.EnhancedMonitoring ||
		*restored.CurrentVersion == *before.CurrentVersion ||
		*restored.CurrentVersion == *after.CurrentVersion {
		t.Fatal("compensation CAS/metadata")
	}
	_, e = s.CloudFormationRestore(local(), snapshot, *after.CurrentVersion)
	wantError(t, e, "BadRequestException", 400)
	// External tags present in live state survive ownership shrink/removal.
	_, _, e = s.CloudFormationUpdate(
		local(),
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     restored.CurrentVersion,
			EnhancedMonitoring: new("DEFAULT"),
		},
		map[string]string{"owner": "old"},
		map[string]string{},
	)
	if e != nil {
		t.Fatal(e)
	}
	tags, _ := s.ListTagsForResource(local(), &api.ListTagsForResourceRequest{ResourceArn: arn})
	if len(tags.Tags) != 49 {
		t.Fatal("owned tag removal", tags)
	}
	tags.Tags["new"] = "copy"
	if len(describe(t, s, arn).Tags) != 49 {
		t.Fatal("output ownership")
	}
}

func TestListPaginationFilterScopeAndDeletedCursor(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	for i := range 8 {
		in := input()
		in.ClusterName = new(fmt.Sprintf("page-%02d", i))
		create(t, s, in)
	}
	in := input()
	in.ClusterName = new("other")
	create(t, s, in)
	first, e := s.ListClusters(
		local(),
		&api.ListClustersRequest{ClusterNameFilter: new("page-"), MaxResults: new(int64(3))},
	)
	if e != nil || len(first.ClusterInfoList) != 3 || first.NextToken == nil {
		t.Fatal(e)
	}
	_, e = s.DeleteCluster(local(), &api.DeleteClusterRequest{ClusterArn: first.ClusterInfoList[2].ClusterArn})
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.ListClusters(
		local(),
		&api.ListClustersRequest{
			ClusterNameFilter: new("page-"),
			MaxResults:        new(int64(3)),
			NextToken:         first.NextToken,
		},
	)
	if e != nil || len(second.ClusterInfoList) != 3 || *second.ClusterInfoList[0].ClusterName != "page-03" ||
		second.NextToken == nil {
		t.Fatal("deleted cursor", e)
	}
	last, e := s.ListClusters(
		local(),
		&api.ListClustersRequest{ClusterNameFilter: new("page-"), NextToken: second.NextToken},
	)
	if e != nil || len(last.ClusterInfoList) != 2 || last.NextToken != nil {
		t.Fatal("terminal page", e)
	}
	for _, ctx := range []context.Context{scoped("111111111111", "us-east-1"), scoped("000000000000", "us-west-2")} {
		_, e = s.ListClusters(
			ctx,
			&api.ListClustersRequest{ClusterNameFilter: new("page-"), NextToken: first.NextToken},
		)
		wantError(t, e, "BadRequestException", 400)
	}
	for _, in := range []*api.ListClustersRequest{
		{NextToken: first.NextToken},
		{NextToken: new("bad")},
		{NextToken: new("")},
		{MaxResults: new(int64(0))},
		{MaxResults: new(int64(101))},
	} {
		_, e = s.ListClusters(local(), in)
		wantError(t, e, "BadRequestException", 400)
	}
	empty, e := s.ListClusters(local(), &api.ListClustersRequest{ClusterNameFilter: new("not-matching")})
	if e != nil || len(empty.ClusterInfoList) != 0 {
		t.Fatal(e)
	}
	// A page exactly at the limit has no spurious continuation token.
	page, e := s.ListClusters(local(), &api.ListClustersRequest{MaxResults: new(int64(8))})
	if e != nil || len(page.ClusterInfoList) != 8 || page.NextToken != nil {
		t.Fatal("boundary", e)
	}
}

func TestTagBoundaryAcceptanceStaleCFAndDefaultListLimit(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	arn := create(t, s, input())
	b := describe(t, s, arn)
	accepted := map[string]string{
		strings.Repeat("é", 128): strings.Repeat("字", 255),
		"Case":                   "one",
		"case":                   "two",
		"_./=+-@ space":          "",
	}
	if _, e := s.TagResource(local(), &api.TagResourceRequest{ResourceArn: arn, Tags: accepted}); e != nil {
		t.Fatal(e)
	}
	tags, _ := s.ListTagsForResource(local(), &api.ListTagsForResourceRequest{ResourceArn: arn})
	if len(tags.Tags) != 5 || tags.Tags["Case"] != "one" || tags.Tags["case"] != "two" {
		t.Fatal("Unicode/case boundaries")
	}
	if _, e := s.UntagResource(local(), &api.UntagResourceRequest{ResourceArn: arn, TagKeys: []string{}}); e != nil {
		t.Fatal("explicit empty list", e)
	}
	if _, e := s.UpdateMonitoring(
		local(),
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     b.CurrentVersion,
			EnhancedMonitoring: new("PER_BROKER"),
		},
	); e != nil {
		t.Fatal(e)
	}
	before := describe(t, s, arn)
	_, _, e := s.CloudFormationUpdate(
		local(),
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     b.CurrentVersion,
			EnhancedMonitoring: new("DEFAULT"),
		},
		map[string]string{"owner": "local"},
		map[string]string{"replacement": "must-not-write"},
	)
	wantError(t, e, "BadRequestException", 400)
	if !reflect.DeepEqual(before, describe(t, s, arn)) {
		t.Fatal("stale CF changed config/tags/version")
	}
	for i := range 101 {
		in := input()
		in.ClusterName = new(fmt.Sprintf("default-%03d", i))
		create(t, s, in)
	}
	first, e := s.ListClusters(local(), &api.ListClustersRequest{})
	if e != nil || len(first.ClusterInfoList) != 100 || first.NextToken == nil {
		t.Fatal("default limit", e)
	}
	last, e := s.ListClusters(local(), &api.ListClustersRequest{NextToken: first.NextToken})
	if e != nil || len(last.ClusterInfoList) != 2 || last.NextToken != nil {
		t.Fatal("default continuation", e)
	}
}

func TestCloudFormationOwnedTagsFromPersistedEmptySet(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	in := input()
	in.Tags = nil
	arn := create(t, s, in)
	before := describe(t, s, arn)
	// Empty maps are omitted by record serialization. Re-adding owned tags must
	// allocate a final set and cannot assume a non-nil map from the storage read.
	snapshot, after, e := s.CloudFormationUpdate(
		local(),
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     before.CurrentVersion,
			EnhancedMonitoring: new("DEFAULT"),
		},
		map[string]string{},
		map[string]string{"owned": "added"},
	)
	if e != nil || after.Tags["owned"] != "added" || *after.CurrentVersion == *before.CurrentVersion {
		t.Fatal("tags after empty persisted set", e)
	}
	restored, e := s.CloudFormationRestore(local(), snapshot, *after.CurrentVersion)
	if e != nil || len(restored.Tags) != 0 || *restored.CurrentVersion == *after.CurrentVersion {
		t.Fatal("restore empty tags", e)
	}
	stable := describe(t, s, arn)
	_, after, e = s.CloudFormationUpdate(
		local(),
		&api.UpdateMonitoringRequest{
			ClusterArn:         arn,
			CurrentVersion:     stable.CurrentVersion,
			EnhancedMonitoring: new("DEFAULT"),
		},
		map[string]string{},
		map[string]string{},
	)
	if e != nil || *after.CurrentVersion != *stable.CurrentVersion {
		t.Fatal("empty final set changed no-op revision", e)
	}
}
