package mq

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
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func mqContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}
func localContext() context.Context { return mqContext("000000000000", "us-east-1") }
func brokerInput() *api.CreateBrokerRequest {
	return &api.CreateBrokerRequest{
		BrokerName: new("broker"), EngineType: new(activeMQ), EngineVersion: new("5.18"),
		HostInstanceType: new(micro), DeploymentMode: new(single), PubliclyAccessible: new(true),
		Users: []api.User{{Username: new("admin"), Password: new("sample-valid-password")}},
		Logs:  &api.Logs{General: new(true), Audit: new(false)}, Tags: map[string]string{"owner": "local"},
		MaintenanceWindowStartTime: &api.WeeklyStartTime{DayOfWeek: new("FRIDAY"), TimeOfDay: new("12:34")},
	}
}

func wantError(t *testing.T, e error, code string, status int) {
	t.Helper()
	var aws *awserror.Error
	if !errors.As(e, &aws) || aws.Code != code || aws.StatusCode != status {
		t.Fatalf("want %s/%d, got %v", code, status, e)
	}
}

func describe(t *testing.T, s *Service, id *string) *api.DescribeBrokerResponse {
	t.Helper()
	b, e := s.DescribeBroker(localContext(), &api.DescribeBrokerRequest{BrokerId: id})
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestBrokerLifecycleDefaultsSecrecyAndAtomicUpdate(t *testing.T) {
	ctx := localContext()
	store := &memory.Store{}
	s := New(store, nil)
	in := brokerInput()
	created, e := s.CreateBroker(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	b := describe(t, s, created.BrokerId)
	if !idPattern.MatchString(*b.BrokerId) ||
		*b.BrokerArn != "arn:aws:mq:us-east-1:000000000000:broker:broker:"+*b.BrokerId ||
		b.Created == nil || !*b.AutoMinorVersionUpgrade || !*b.EncryptionOptions.UseAwsOwnedKey ||
		*b.Logs.Audit || *b.MaintenanceWindowStartTime.TimeZone != "UTC" ||
		len(b.Users) != 1 ||
		*b.Users[0].Username != "admin" {
		t.Fatal("metadata/defaults")
	}
	if in.AutoMinorVersionUpgrade != nil || in.EncryptionOptions != nil ||
		in.MaintenanceWindowStartTime.TimeZone != nil {
		t.Fatal("caller input mutated")
	}
	store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			raw, _ := tx.Get(key)
			if strings.Contains(string(raw), *in.Users[0].Password) || strings.Contains(string(raw), "password") {
				t.Fatal("raw password stored")
			}
		}
		return nil
	})
	inventory, e := s.Resources(ctx)
	if e != nil || len(inventory) != 1 || inventory[0].Details.(map[string]any)["RecordsOnly"] != true ||
		inventory[0].Details.(map[string]any)["ExecutionReady"] != false {
		t.Fatal("inventory boundary")
	}
	out, e := s.UpdateBroker(ctx, &api.UpdateBrokerRequest{
		BrokerId: created.BrokerId, HostInstanceType: new(large),
		EngineVersion: new("5.19"), Logs: &api.Logs{General: new(false)},
	})
	if e != nil || *out.HostInstanceType != large || *out.Logs.General || *out.Logs.Audit {
		t.Fatal("update", e)
	}
	updated := describe(t, s, created.BrokerId)
	if !b.Created.Equal(*updated.Created) || *b.BrokerArn != *updated.BrokerArn ||
		*updated.MaintenanceWindowStartTime.TimeOfDay != "12:34" {
		t.Fatal("stable identity or retention")
	}
	for _, bad := range []*api.UpdateBrokerRequest{
		{BrokerId: created.BrokerId, AutoMinorVersionUpgrade: new(false)},
		{BrokerId: created.BrokerId, EngineVersion: new("5.18")},
		{BrokerId: created.BrokerId, HostInstanceType: new("mq.m7g.large")},
		{BrokerId: created.BrokerId, SecurityGroups: []string{}},
		{BrokerId: created.BrokerId, MaintenanceWindowStartTime: &api.WeeklyStartTime{
			DayOfWeek: new("MONDAY"), TimeOfDay: new("24:00"),
		}},
	} {
		_, e = s.UpdateBroker(ctx, bad)
		wantError(t, e, "BadRequestException", 400)
		if !reflect.DeepEqual(updated, describe(t, s, created.BrokerId)) {
			t.Fatal("failed update changed record")
		}
	}
	_, e = s.CreateBroker(ctx, in)
	wantError(t, e, "ConflictException", 409)
	_, e = s.DescribeBroker(ctx, &api.DescribeBrokerRequest{BrokerId: created.BrokerArn})
	wantError(t, e, "BadRequestException", 400)
	deleted, e := s.DeleteBroker(ctx, &api.DeleteBrokerRequest{BrokerId: created.BrokerId})
	if e != nil || *deleted.BrokerId != *created.BrokerId {
		t.Fatal("delete", e)
	}
	_, e = s.DeleteBroker(ctx, &api.DeleteBrokerRequest{BrokerId: created.BrokerId})
	wantError(t, e, "NotFoundException", 404)
	_, e = s.DescribeBroker(ctx, &api.DescribeBrokerRequest{BrokerId: created.BrokerId})
	wantError(t, e, "NotFoundException", 404)
}

func TestBrokerCreationValidationAndEngineCombinations(t *testing.T) {
	for _, engine := range []string{activeMQ, rabbitMQ} {
		for _, host := range []string{micro, large} {
			for _, mode := range []string{single, standby, cluster} {
				t.Run(engine+"/"+host+"/"+mode, func(t *testing.T) {
					in := brokerInput()
					in.EngineType = new(engine)
					in.HostInstanceType = new(host)
					in.DeploymentMode = new(mode)
					if engine == rabbitMQ {
						in.EngineVersion = new("3.13")
						in.Logs.Audit = nil
					}
					_, e := New(&memory.Store{}, nil).CreateBroker(localContext(), in)
					valid := mode == single || host == large &&
						(engine == activeMQ && mode == standby || engine == rabbitMQ && mode == cluster)
					valid = valid && (engine != rabbitMQ || host != micro)
					if valid {
						if e != nil {
							t.Fatal(e)
						}
					} else {
						wantError(t, e, "BadRequestException", 400)
					}
				})
			}
		}
	}
	mutations := []struct {
		change func(*api.CreateBrokerRequest)
		name   string
	}{
		{name: "name empty", change: func(in *api.CreateBrokerRequest) { in.BrokerName = new("") }},
		{name: "name chars", change: func(in *api.CreateBrokerRequest) { in.BrokerName = new("a.b") }},
		{
			name:   "name length",
			change: func(in *api.CreateBrokerRequest) { in.BrokerName = new(strings.Repeat("a", 51)) },
		},
		{name: "missing public", change: func(in *api.CreateBrokerRequest) { in.PubliclyAccessible = nil }},
		{name: "missing version", change: func(in *api.CreateBrokerRequest) { in.EngineVersion = nil }},
		{name: "old version", change: func(in *api.CreateBrokerRequest) { in.EngineVersion = new("5.17") }},
		{name: "false upgrade", change: func(in *api.CreateBrokerRequest) { in.AutoMinorVersionUpgrade = new(false) }},
		{name: "encryption false", change: func(in *api.CreateBrokerRequest) {
			in.EncryptionOptions = &api.EncryptionOptions{UseAwsOwnedKey: new(false)}
		}},
		{
			name:   "encryption empty",
			change: func(in *api.CreateBrokerRequest) { in.EncryptionOptions = &api.EncryptionOptions{} },
		},
		{name: "missing users", change: func(in *api.CreateBrokerRequest) { in.Users = nil }},
		{
			name:   "duplicate users",
			change: func(in *api.CreateBrokerRequest) { in.Users = append(in.Users, in.Users[0]) },
		},
		{name: "username one", change: func(in *api.CreateBrokerRequest) { in.Users[0].Username = new("a") }},
		{name: "username space", change: func(in *api.CreateBrokerRequest) { in.Users[0].Username = new("a b") }},
		{name: "password short", change: func(in *api.CreateBrokerRequest) { in.Users[0].Password = new("short") }},
		{
			name:   "password unique",
			change: func(in *api.CreateBrokerRequest) { in.Users[0].Password = new("aaaaaaaaaaaa") },
		},
		{
			name:   "password excluded",
			change: func(in *api.CreateBrokerRequest) { in.Users[0].Password = new("sample:password") },
		},
		{
			name:   "password max",
			change: func(in *api.CreateBrokerRequest) { in.Users[0].Password = new(strings.Repeat("abcd", 63)) },
		},
		{
			name:   "password unicode",
			change: func(in *api.CreateBrokerRequest) { in.Users[0].Password = new("sample-pässword") },
		},
		{
			name:   "timezone excluded",
			change: func(in *api.CreateBrokerRequest) { in.MaintenanceWindowStartTime.TimeZone = new("Asia/Kuala_Lumpur") },
		},
		{name: "token empty", change: func(in *api.CreateBrokerRequest) { in.CreatorRequestId = new("") }},
		{name: "public subnet empty", change: func(in *api.CreateBrokerRequest) { in.SubnetIds = []string{} }},
		{
			name:   "tag reserved",
			change: func(in *api.CreateBrokerRequest) { in.Tags = map[string]string{"AWS:owner": ""} },
		},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			s := New(&memory.Store{}, nil)
			in := brokerInput()
			m.change(in)
			_, e := s.CreateBroker(localContext(), in)
			wantError(t, e, "BadRequestException", 400)
			out, _ := s.ListBrokers(localContext(), &api.ListBrokersRequest{})
			if len(out.BrokerSummaries) != 0 {
				t.Fatal("invalid creation persisted")
			}
		})
	}
	in := brokerInput()
	in.Users[0].Username = new("a~b")
	in.EngineType = new(rabbitMQ)
	in.EngineVersion = new("3.13")
	in.HostInstanceType = new(large)
	in.Logs.Audit = nil
	for _, name := range []string{"guest", "a~b"} {
		in.Users[0].Username = new(name)
		_, e := New(&memory.Store{}, nil).CreateBroker(localContext(), in)
		wantError(t, e, "BadRequestException", 400)
	}
	in.Users[0].Username = new("admin")
	in.Logs.Audit = new(false)
	_, e := New(&memory.Store{}, nil).CreateBroker(localContext(), in)
	wantError(t, e, "BadRequestException", 400)
	in.Logs.Audit = nil
	s := New(&memory.Store{}, nil)
	created, e := s.CreateBroker(localContext(), in)
	if e != nil {
		t.Fatal(e)
	}
	if len(describe(t, s, created.BrokerId).Users) != 0 {
		t.Fatal("RabbitMQ UserSummary returned despite modeled exclusion")
	}
}

func TestBrokerIdempotencyConcurrencyAndScope(t *testing.T) {
	s := New(&memory.Store{}, nil)
	ctx := localContext()
	in := brokerInput()
	in.CreatorRequestId = new("same-token")
	var wg sync.WaitGroup
	var successes atomic.Int32
	ids := make(chan string, 24)
	for range 24 {
		wg.Go(func() {
			out, e := s.CreateBroker(ctx, in)
			if e != nil {
				t.Error(e)
				return
			}
			successes.Add(1)
			ids <- *out.BrokerId
		})
	}
	wg.Wait()
	close(ids)
	id := ""
	for value := range ids {
		if id == "" {
			id = value
		}
		if id != value {
			t.Fatal("replay duplicated")
		}
	}
	if successes.Load() != 24 {
		t.Fatal("concurrent replay failed")
	}
	changed := common.Clone(in)
	changed.Users[0].Password = new("different-password")
	_, e := s.CreateBroker(ctx, changed)
	wantError(t, e, "ConflictException", 409)
	for _, other := range []context.Context{
		mqContext("111111111111", "us-east-1"), mqContext("000000000000", "us-west-2"),
	} {
		_, e = s.DescribeBroker(other, &api.DescribeBrokerRequest{BrokerId: new(id)})
		wantError(t, e, "NotFoundException", 404)
		_, e = s.UpdateBroker(other, &api.UpdateBrokerRequest{BrokerId: new(id)})
		wantError(t, e, "NotFoundException", 404)
		_, e = s.DeleteBroker(other, &api.DeleteBrokerRequest{BrokerId: new(id)})
		wantError(t, e, "NotFoundException", 404)
		_, e = s.CreateBroker(other, in)
		if e != nil {
			t.Fatal("scoped name/token", e)
		}
	}
	_, e = s.DeleteBroker(ctx, &api.DeleteBrokerRequest{BrokerId: new(id)})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.CreateBroker(ctx, in)
	wantError(t, e, "ConflictException", 409)
	noToken := brokerInput()
	noToken.CreatorRequestId = nil
	successes.Store(0)
	for range 20 {
		wg.Go(func() {
			_, e := s.CreateBroker(ctx, noToken)
			if e == nil {
				successes.Add(1)
			} else {
				wantError(t, e, "ConflictException", 409)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("uniqueness was not atomic")
	}
}

func TestBrokerTagsAtomicLimitsAndConcurrentMerge(t *testing.T) {
	s := New(&memory.Store{}, nil)
	ctx := localContext()
	in := brokerInput()
	created, e := s.CreateBroker(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []map[string]string{
		{"": ""}, {strings.Repeat("a", 129): ""}, {"k": strings.Repeat("v", 257)}, {"aws:key": ""},
	} {
		_, e = s.CreateTags(ctx, &api.CreateTagsRequest{ResourceArn: created.BrokerArn, Tags: bad})
		wantError(t, e, "BadRequestException", 400)
	}
	var wg sync.WaitGroup
	for i := range 49 {
		wg.Go(func() {
			_, tagErr := s.CreateTags(ctx, &api.CreateTagsRequest{
				ResourceArn: created.BrokerArn,
				Tags:        map[string]string{fmt.Sprintf("k%02d", i): ""},
			})
			if tagErr != nil {
				t.Error(tagErr)
			}
		})
	}
	wg.Wait()
	before := describe(t, s, created.BrokerId)
	if len(before.Tags) != 50 {
		t.Fatal("concurrent merge lost tags")
	}
	_, e = s.CreateTags(
		ctx,
		&api.CreateTagsRequest{
			ResourceArn: created.BrokerArn,
			Tags:        map[string]string{"overflow": "", "owner": "changed"},
		},
	)
	wantError(t, e, "BadRequestException", 400)
	if !reflect.DeepEqual(before, describe(t, s, created.BrokerId)) {
		t.Fatal("partial tag write")
	}
	_, e = s.CreateTags(
		ctx,
		&api.CreateTagsRequest{ResourceArn: created.BrokerArn, Tags: map[string]string{"owner": "changed"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteTags(
		ctx,
		&api.DeleteTagsRequest{ResourceArn: created.BrokerArn, TagKeys: []string{"owner", "owner"}},
	)
	wantError(t, e, "BadRequestException", 400)
	_, e = s.DeleteTags(ctx, &api.DeleteTagsRequest{ResourceArn: created.BrokerArn, TagKeys: []string{}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteTags(
		ctx,
		&api.DeleteTagsRequest{ResourceArn: created.BrokerArn, TagKeys: []string{"owner", "absent"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	tags, e := s.ListTags(ctx, &api.ListTagsRequest{ResourceArn: created.BrokerArn})
	if e != nil || len(tags.Tags) != 49 {
		t.Fatal("delete/list tags", e)
	}
	for _, foreign := range []context.Context{
		mqContext("111111111111", "us-east-1"), mqContext("000000000000", "us-west-2"),
	} {
		_, e = s.ListTags(foreign, &api.ListTagsRequest{ResourceArn: created.BrokerArn})
		wantError(t, e, "NotFoundException", 404)
		_, e = s.CreateTags(foreign, &api.CreateTagsRequest{ResourceArn: created.BrokerArn})
		wantError(t, e, "NotFoundException", 404)
		_, e = s.DeleteTags(foreign, &api.DeleteTagsRequest{ResourceArn: created.BrokerArn, TagKeys: []string{}})
		wantError(t, e, "NotFoundException", 404)
	}
	_, e = s.ListTags(
		ctx,
		&api.ListTagsRequest{
			ResourceArn: new(strings.Replace(*created.BrokerArn, ":broker:broker:", ":broker:wrong:", 1)),
		},
	)
	wantError(t, e, "NotFoundException", 404)
	desired := map[string]string{}
	for i := range 50 {
		desired[fmt.Sprintf("new%d", i)] = ""
	}
	before = describe(t, s, created.BrokerId)
	_, _, e = s.CloudFormationUpdate(
		ctx,
		&api.UpdateBrokerRequest{BrokerId: created.BrokerId, HostInstanceType: new(large)},
		maps.Clone(before.Tags),
		desired,
	)
	if e != nil || len(describe(t, s, created.BrokerId).Tags) != 50 {
		t.Fatal("atomic disjoint 50-tag replacement", e)
	}
	restored, e := s.CloudFormationRestore(ctx, before)
	if e != nil || !reflect.DeepEqual(before, restored) {
		t.Fatal("restore exact live snapshot", e)
	}
}

func TestBrokerPaginationTokensAndDeletedCursor(t *testing.T) {
	s := New(&memory.Store{}, nil)
	ctx := localContext()
	for i := range 26 {
		in := brokerInput()
		in.BrokerName = new(fmt.Sprintf("broker-%02d", i))
		_, e := s.CreateBroker(ctx, in)
		if e != nil {
			t.Fatal(e)
		}
	}
	first, e := s.ListBrokers(ctx, &api.ListBrokersRequest{})
	if e != nil || len(first.BrokerSummaries) != 20 || first.NextToken == nil {
		t.Fatal("default page", e)
	}
	for _, n := range []int64{4, 101} {
		_, e = s.ListBrokers(ctx, &api.ListBrokersRequest{MaxResults: new(n)})
		wantError(t, e, "BadRequestException", 400)
	}
	for _, token := range []string{"bad", "", *first.NextToken} {
		other := ctx
		if token == *first.NextToken {
			other = mqContext("111111111111", "us-east-1")
		}
		_, e = s.ListBrokers(other, &api.ListBrokersRequest{NextToken: new(token)})
		if token == "" {
			if e != nil {
				t.Fatal("empty first-page token", e)
			}
		} else {
			wantError(t, e, "BadRequestException", 400)
		}
	}
	cursor := first.BrokerSummaries[19].BrokerId
	_, e = s.DeleteBroker(ctx, &api.DeleteBrokerRequest{BrokerId: cursor})
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.ListBrokers(ctx, &api.ListBrokersRequest{NextToken: first.NextToken, MaxResults: new(int64(5))})
	if e != nil || len(second.BrokerSummaries) != 5 || second.NextToken == nil {
		t.Fatal("deleted cursor resume", e)
	}
	third, e := s.ListBrokers(ctx, &api.ListBrokersRequest{NextToken: second.NextToken})
	if e != nil || len(third.BrokerSummaries) != 1 || third.NextToken != nil {
		t.Fatal("terminal page", e)
	}
}

func TestBrokerPrivateNetworkMetadataValidation(t *testing.T) {
	ctx := localContext()
	store := &memory.Store{}
	network := ec2.New(store)
	s := New(store, network)
	v, e := network.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.91.0.0/16")})
	if e != nil {
		t.Fatal(e)
	}
	group, e := network.CreateSecurityGroup(ctx, &ec2api.CreateSecurityGroupRequest{
		VpcId:     v.Vpc.VpcId,
		GroupName: new("mq-records"), Description: new("local"),
	})
	if e != nil {
		t.Fatal(e)
	}
	subnets := []string{}
	for i, zone := range []string{"us-east-1a", "us-east-1b", "us-east-1c"} {
		sn, subnetErr := network.CreateSubnet(ctx, &ec2api.CreateSubnetRequest{
			VpcId:     v.Vpc.VpcId,
			CidrBlock: new(fmt.Sprintf("10.91.%d.0/24", i)), AvailabilityZone: new(zone),
		})
		if subnetErr != nil {
			t.Fatal(subnetErr)
		}
		subnets = append(subnets, *sn.Subnet.SubnetId)
	}
	for _, combo := range []struct {
		engine, version, mode string
		count                 int
	}{
		{activeMQ, "5.18", single, 1},
		{activeMQ, "5.19", standby, 2},
		{rabbitMQ, "3.13", single, 1},
		{rabbitMQ, "3.13", cluster, 3},
	} {
		in := brokerInput()
		in.BrokerName = new(combo.engine + combo.mode)
		in.EngineType = new(combo.engine)
		in.EngineVersion = new(combo.version)
		in.HostInstanceType = new(large)
		in.DeploymentMode = new(combo.mode)
		in.PubliclyAccessible = new(false)
		in.SubnetIds = subnets[:combo.count]
		in.SecurityGroups = []string{*group.GroupId}
		if combo.engine == rabbitMQ {
			in.Logs.Audit = nil
		}
		out, createErr := s.CreateBroker(ctx, in)
		if createErr != nil {
			t.Fatal(createErr)
		}
		b := describe(t, s, out.BrokerId)
		if *b.PubliclyAccessible || !reflect.DeepEqual(b.SubnetIds, in.SubnetIds) {
			t.Fatal("private references not retained")
		}
	}
	base := brokerInput()
	base.PubliclyAccessible = new(false)
	base.SubnetIds = subnets[:1]
	base.SecurityGroups = []string{*group.GroupId}
	for _, change := range []func(*api.CreateBrokerRequest){
		func(in *api.CreateBrokerRequest) { in.SubnetIds = nil },
		func(in *api.CreateBrokerRequest) { in.SecurityGroups = nil },
		func(in *api.CreateBrokerRequest) {
			in.SubnetIds = []string{subnets[0], subnets[0]}
			in.DeploymentMode = new(standby)
			in.HostInstanceType = new(large)
		},
		func(in *api.CreateBrokerRequest) { in.SecurityGroups = []string{*group.GroupId, *group.GroupId} },
		func(in *api.CreateBrokerRequest) { in.SubnetIds = []string{"subnet-12345678"} },
		func(in *api.CreateBrokerRequest) { in.SecurityGroups = []string{"sg-12345678"} },
	} {
		in := common.Clone(base)
		change(in)
		_, e = s.CreateBroker(ctx, in)
		wantError(t, e, "BadRequestException", 400)
	}
	_, e = s.CreateBroker(mqContext("111111111111", "us-east-1"), base)
	wantError(t, e, "BadRequestException", 400)
	wrong, e := network.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.92.0.0/16")})
	if e != nil {
		t.Fatal(e)
	}
	other, e := network.CreateSecurityGroup(ctx, &ec2api.CreateSecurityGroupRequest{
		VpcId:     wrong.Vpc.VpcId,
		GroupName: new("mq-wrong"), Description: new("local"),
	})
	if e != nil {
		t.Fatal(e)
	}
	sameAZ, sameErr := network.CreateSubnet(ctx, &ec2api.CreateSubnetRequest{
		VpcId:     v.Vpc.VpcId,
		CidrBlock: new("10.91.8.0/24"), AvailabilityZone: new("us-east-1a"),
	})
	if sameErr != nil {
		t.Fatal(sameErr)
	}
	badAZ := common.Clone(base)
	badAZ.BrokerName = new("same-az")
	badAZ.HostInstanceType = new(large)
	badAZ.DeploymentMode = new(standby)
	badAZ.SubnetIds = []string{subnets[0], *sameAZ.Subnet.SubnetId}
	_, e = s.CreateBroker(ctx, badAZ)
	wantError(t, e, "BadRequestException", 400)
	differentVPC, subnetErr := network.CreateSubnet(ctx, &ec2api.CreateSubnetRequest{
		VpcId:     wrong.Vpc.VpcId,
		CidrBlock: new("10.92.1.0/24"), AvailabilityZone: new("us-east-1b"),
	})
	if subnetErr != nil {
		t.Fatal(subnetErr)
	}
	badVPC := common.Clone(badAZ)
	badVPC.BrokerName = new("different-vpc")
	badVPC.SubnetIds = []string{subnets[0], *differentVPC.Subnet.SubnetId}
	_, e = s.CreateBroker(ctx, badVPC)
	wantError(t, e, "BadRequestException", 400)
	base.SecurityGroups = []string{*other.GroupId}
	_, e = s.CreateBroker(ctx, base)
	wantError(t, e, "BadRequestException", 400)
}

func TestBrokerConcurrentNestedUpdatesAndReplayCredentialDiscard(t *testing.T) {
	ctx := localContext()
	store := &memory.Store{}
	s := New(store, nil)
	in := brokerInput()
	in.CreatorRequestId = new("credential-check")
	created, e := s.CreateBroker(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for _, logs := range []*api.Logs{{General: new(false)}, {Audit: new(true)}} {
		wg.Go(func() {
			_, err := s.UpdateBroker(ctx, &api.UpdateBrokerRequest{BrokerId: created.BrokerId, Logs: logs})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	b := describe(t, s, created.BrokerId)
	if *b.Logs.General || !*b.Logs.Audit {
		t.Fatal("concurrent nested update lost a flag")
	}
	store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			raw, _ := tx.Get(key)
			if strings.Contains(string(raw), *in.Users[0].Password) {
				t.Fatal("replay retained raw credential")
			}
		}
		return nil
	})
	d := ProtectCredentials(api.Descriptor(s))
	if !d.Operations["CreateBroker"].Sensitive {
		t.Fatal("credential diagnostics are not redacted")
	}
}
