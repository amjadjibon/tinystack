package batch

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func batchContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
}

func clientError(t *testing.T, e error) {
	t.Helper()
	var aws *awserror.Error
	if !errors.As(e, &aws) || aws.Code != "ClientException" || aws.StatusCode != 400 {
		t.Fatalf("want ClientException/400, got %v", e)
	}
}

func managedFixture(t *testing.T) (*Service, *api.CreateComputeEnvironmentRequest) {
	t.Helper()
	ctx := batchContext()
	store := &memory.Store{}
	net := ec2.New(store)
	s := New(store, net)
	v, e := net.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.90.0.0/16")})
	if e != nil {
		t.Fatal(e)
	}
	sn, e := net.CreateSubnet(ctx, &ec2api.CreateSubnetRequest{VpcId: v.Vpc.VpcId, CidrBlock: new("10.90.1.0/24")})
	if e != nil {
		t.Fatal(e)
	}
	g, e := net.CreateSecurityGroup(
		ctx,
		&ec2api.CreateSecurityGroupRequest{
			VpcId:       v.Vpc.VpcId,
			GroupName:   new("batch"),
			Description: new("local metadata"),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	return s, &api.CreateComputeEnvironmentRequest{
		ComputeEnvironmentName: new("compute"),
		Type:                   new(managed),
		Tags:                   map[string]string{"owner": "infra"},
		ComputeResources: &api.ComputeResource{
			Type:               new(ec2Platform),
			MinvCpus:           new(int64(2)),
			MaxvCpus:           new(int64(16)),
			DesiredvCpus:       new(int64(4)),
			AllocationStrategy: new("BEST_FIT_PROGRESSIVE"),
			InstanceTypes:      []string{"c5.large", "m5"},
			Subnets:            []string{*sn.Subnet.SubnetId},
			SecurityGroupIds:   []string{*g.GroupId},
			InstanceRole:       new("arn:aws:iam::000000000000:instance-profile/worker"),
		},
	}
}

func definitionInput() *api.RegisterJobDefinitionRequest {
	return &api.RegisterJobDefinitionRequest{
		JobDefinitionName: new("worker"),
		Type:              new("container"),
		Parameters:        map[string]string{"input": "local"},
		Tags:              map[string]string{"owner": "worker"},
		ContainerProperties: &api.ContainerProperties{
			Image:   new("example/worker:v2"),
			Command: []string{"run", "Ref::input"},
			ResourceRequirements: []api.ResourceRequirement{
				{Type: new("VCPU"), Value: new("2")},
				{Type: new("MEMORY"), Value: new("128")},
			},
			Environment:      []api.KeyValuePair{{Name: new("LOCAL"), Value: new("yes")}},
			JobRoleArn:       new("arn:aws:iam::000000000000:role/job"),
			ExecutionRoleArn: new("arn:aws:iam::000000000000:role/pull"),
		},
	}
}

func TestManagedQueueLifecycleAtomicity(t *testing.T) {
	ctx := batchContext()
	s, in := managedFixture(t)
	ce, e := s.CreateComputeEnvironment(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.CreateComputeEnvironment(ctx, in)
	clientError(t, e)
	q, e := s.CreateJobQueue(
		ctx,
		&api.CreateJobQueueRequest{
			JobQueueName: new("queue"),
			Priority:     new(int64(7)),
			ComputeEnvironmentOrder: []api.ComputeEnvironmentOrder{
				{Order: new(int64(2)), ComputeEnvironment: ce.ComputeEnvironmentName},
			},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	before, e := s.DescribeComputeEnvironments(ctx, &api.DescribeComputeEnvironmentsRequest{})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before.ComputeEnvironments[0].ComputeResources, in.ComputeResources) ||
		*before.ComputeEnvironments[0].Status != valid {
		t.Fatalf("nested retention %+v", before)
	}
	_, e = s.UpdateComputeEnvironment(
		ctx,
		&api.UpdateComputeEnvironmentRequest{
			ComputeEnvironment: ce.ComputeEnvironmentArn,
			State:              new(disabled),
			ComputeResources:   &api.ComputeResourceUpdate{MaxvCpus: new(int64(1))},
		},
	)
	clientError(t, e)
	after, _ := s.DescribeComputeEnvironments(ctx, &api.DescribeComputeEnvironmentsRequest{})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed update changed environment")
	}
	_, e = s.DeleteComputeEnvironment(
		ctx,
		&api.DeleteComputeEnvironmentRequest{ComputeEnvironment: ce.ComputeEnvironmentArn},
	)
	clientError(t, e)
	_, e = s.UpdateComputeEnvironment(
		ctx,
		&api.UpdateComputeEnvironmentRequest{
			ComputeEnvironment: ce.ComputeEnvironmentArn,
			State:              new(disabled),
			ComputeResources:   &api.ComputeResourceUpdate{DesiredvCpus: new(int64(8))},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteComputeEnvironment(
		ctx,
		&api.DeleteComputeEnvironmentRequest{ComputeEnvironment: ce.ComputeEnvironmentName},
	)
	clientError(t, e)
	qb, _ := s.DescribeJobQueues(ctx, &api.DescribeJobQueuesRequest{})
	_, e = s.UpdateJobQueue(
		ctx,
		&api.UpdateJobQueueRequest{
			JobQueue: q.JobQueueArn,
			Priority: new(int64(9)),
			ComputeEnvironmentOrder: []api.ComputeEnvironmentOrder{
				{Order: new(int64(1)), ComputeEnvironment: new("absent")},
			},
		},
	)
	clientError(t, e)
	qa, _ := s.DescribeJobQueues(ctx, &api.DescribeJobQueuesRequest{})
	if !reflect.DeepEqual(qb, qa) {
		t.Fatal("failed queue update changed priority or relations")
	}
	if *qa.JobQueues[0].ComputeEnvironmentOrder[0].ComputeEnvironment != *ce.ComputeEnvironmentArn {
		t.Fatal("association was not normalized")
	}
	_, e = s.UpdateJobQueue(
		ctx,
		&api.UpdateJobQueueRequest{
			JobQueue:                q.JobQueueName,
			State:                   new(disabled),
			ComputeEnvironmentOrder: []api.ComputeEnvironmentOrder{},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteComputeEnvironment(
		ctx,
		&api.DeleteComputeEnvironmentRequest{ComputeEnvironment: ce.ComputeEnvironmentArn},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteJobQueue(ctx, &api.DeleteJobQueueRequest{JobQueue: q.JobQueueArn})
	if e != nil {
		t.Fatal(e)
	}
}

func TestComputeAndContainerValidation(t *testing.T) {
	ctx := batchContext()
	s, in := managedFixture(t)
	for _, mutate := range []func(*api.CreateComputeEnvironmentRequest){
		func(p *api.CreateComputeEnvironmentRequest) { p.Type = new("FARGATE") },
		func(p *api.CreateComputeEnvironmentRequest) { p.ComputeResources.Type = new("SPOT") },
		func(p *api.CreateComputeEnvironmentRequest) { p.State = new("INVALID") },
		func(p *api.CreateComputeEnvironmentRequest) { p.ComputeResources.MaxvCpus = new(int64(0)) },
		func(p *api.CreateComputeEnvironmentRequest) { p.ComputeResources.DesiredvCpus = new(int64(17)) },
		func(p *api.CreateComputeEnvironmentRequest) { p.ComputeResources.InstanceTypes = []string{"c5", "c5"} },
		func(p *api.CreateComputeEnvironmentRequest) { p.ComputeResources.InstanceTypes = []string{"m6g"} },
		func(p *api.CreateComputeEnvironmentRequest) { p.ComputeResources.Subnets = []string{"subnet-12345678"} },
		func(p *api.CreateComputeEnvironmentRequest) {
			p.ComputeResources.SecurityGroupIds = []string{"sg-12345678"}
		},
		func(p *api.CreateComputeEnvironmentRequest) {
			p.ServiceRole = new("arn:aws:iam::111111111111:role/foreign")
		},
		func(p *api.CreateComputeEnvironmentRequest) { p.Tags = map[string]string{"AWS:system": "x"} },
	} {
		p := common.Clone(in)
		mutate(p)
		_, e := s.CreateComputeEnvironment(ctx, p)
		clientError(t, e)
	}
	for _, mutate := range []func(*api.RegisterJobDefinitionRequest){
		func(p *api.RegisterJobDefinitionRequest) { p.Type = new("multinode") },
		func(p *api.RegisterJobDefinitionRequest) { p.ContainerProperties.Image = new("") },
		func(p *api.RegisterJobDefinitionRequest) { p.PlatformCapabilities = []string{"FARGATE"} },
		func(p *api.RegisterJobDefinitionRequest) { p.ContainerProperties.Vcpus = new(int64(1)) },
		func(p *api.RegisterJobDefinitionRequest) {
			p.ContainerProperties.ResourceRequirements[0].Value = new("1.5")
		},
		func(p *api.RegisterJobDefinitionRequest) {
			p.ContainerProperties.ResourceRequirements[1].Type = new("VCPU")
		},
		func(p *api.RegisterJobDefinitionRequest) {
			p.ContainerProperties.Environment = append(p.ContainerProperties.Environment, p.ContainerProperties.Environment[0])
		},
		func(p *api.RegisterJobDefinitionRequest) {
			p.ContainerProperties.Environment[0].Name = new("AWS_BATCH_SECRET")
		},
		func(p *api.RegisterJobDefinitionRequest) {
			p.ContainerProperties.JobRoleArn = new("arn:aws:iam::111111111111:role/foreign")
		},
	} {
		p := definitionInput()
		mutate(p)
		_, e := s.RegisterJobDefinition(ctx, p)
		clientError(t, e)
	}
	all, _ := s.Resources(ctx)
	if len(all) != 0 {
		t.Fatal("invalid creates left records")
	}
}

func TestVersionsTagsScopePaginationAndConcurrency(t *testing.T) {
	ctx := batchContext()
	s := New(&memory.Store{}, nil)
	first, e := s.RegisterJobDefinition(ctx, definitionInput())
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.RegisterJobDefinition(ctx, definitionInput())
	if e != nil || *second.Revision != 2 {
		t.Fatalf("second revision %v %v", second, e)
	}
	_, e = s.DeregisterJobDefinition(ctx, &api.DeregisterJobDefinitionRequest{JobDefinition: first.JobDefinitionArn})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeregisterJobDefinition(ctx, &api.DeregisterJobDefinitionRequest{JobDefinition: new("worker:1")})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeregisterJobDefinition(ctx, &api.DeregisterJobDefinitionRequest{JobDefinition: new("worker")})
	clientError(t, e)
	_, e = s.TagResource(
		ctx,
		&api.TagResourceRequest{ResourceArn: first.JobDefinitionArn, Tags: map[string]string{"one": "revision"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	tags, _ := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceArn: second.JobDefinitionArn})
	if tags.Tags["one"] != "" {
		t.Fatal("tags bled between revisions")
	}
	full := map[string]string{}
	for i := range 49 {
		full[fmt.Sprintf("key-%02d", i)] = "v"
	}
	_, e = s.TagResource(ctx, &api.TagResourceRequest{ResourceArn: first.JobDefinitionArn, Tags: full})
	clientError(t, e)
	tags, _ = s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceArn: first.JobDefinitionArn})
	if len(tags.Tags) != 2 {
		t.Fatal("invalid tags were partially merged")
	}
	_, e = s.UntagResource(
		ctx,
		&api.UntagResourceRequest{ResourceArn: first.JobDefinitionArn, TagKeys: []string{"one", "one"}},
	)
	clientError(t, e)
	_, e = s.UntagResource(
		ctx,
		&api.UntagResourceRequest{ResourceArn: first.JobDefinitionArn, TagKeys: []string{"one", "absent"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	page1, e := s.DescribeJobDefinitions(
		ctx,
		&api.DescribeJobDefinitionsRequest{JobDefinitionName: new("worker"), MaxResults: new(int64(1))},
	)
	if e != nil || len(page1.JobDefinitions) != 1 || *page1.JobDefinitions[0].Status != inactive ||
		page1.NextToken == nil {
		t.Fatalf("first page %v %v", page1, e)
	}
	page2, e := s.DescribeJobDefinitions(
		ctx,
		&api.DescribeJobDefinitionsRequest{
			JobDefinitionName: new("worker"),
			MaxResults:        new(int64(1)),
			NextToken:         page1.NextToken,
		},
	)
	if e != nil || *page2.JobDefinitions[0].Revision != 2 {
		t.Fatalf("second page %v %v", page2, e)
	}
	_, e = s.DescribeJobDefinitions(
		ctx,
		&api.DescribeJobDefinitionsRequest{Status: new(active), NextToken: page1.NextToken},
	)
	clientError(t, e)
	other := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "111111111111", Region: "us-west-2"}},
	)
	_, e = s.TagResource(
		other,
		&api.TagResourceRequest{ResourceArn: first.JobDefinitionArn, Tags: map[string]string{"foreign": "no"}},
	)
	clientError(t, e)
	input := definitionInput()
	input.ContainerProperties.JobRoleArn = nil
	input.ContainerProperties.ExecutionRoleArn = nil
	one, e := s.RegisterJobDefinition(other, input)
	if e != nil || *one.Revision != 1 {
		t.Fatalf("independent revision %v %v", one, e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() { _, registerErr := s.RegisterJobDefinition(ctx, definitionInput()); errs <- registerErr })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	all, e := s.DescribeJobDefinitions(ctx, &api.DescribeJobDefinitionsRequest{JobDefinitionName: new("worker")})
	if e != nil || len(all.JobDefinitions) != 22 {
		t.Fatalf("concurrent revisions %v %v", all, e)
	}
	for i, r := range all.JobDefinitions {
		if *r.Revision != int64(i+1) {
			t.Fatalf("revision skipped or duplicated %d", *r.Revision)
		}
	}
	for _, r := range all.JobDefinitions {
		_, e = s.DeregisterJobDefinition(ctx, &api.DeregisterJobDefinitionRequest{JobDefinition: r.JobDefinitionArn})
		if e != nil {
			t.Fatal(e)
		}
	}
	last, e := s.RegisterJobDefinition(ctx, definitionInput())
	if e != nil || *last.Revision != 23 {
		t.Fatalf("inactive history lost counter %v %v", last, e)
	}
}

func TestComputeUniquenessAndDeletedPaginationCursor(t *testing.T) {
	ctx := batchContext()
	s := New(&memory.Store{}, nil)
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			_, e := s.CreateComputeEnvironment(
				ctx,
				&api.CreateComputeEnvironmentRequest{
					ComputeEnvironmentName: new("a"),
					Type:                   new(unmanaged),
					ServiceRole:            new("batch-role"),
				},
			)
			results <- e
		})
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else {
			clientError(t, e)
		}
	}
	if success != 1 {
		t.Fatalf("same name successes %d", success)
	}
	for _, name := range []string{"b", "c"} {
		_, e := s.CreateComputeEnvironment(
			ctx,
			&api.CreateComputeEnvironmentRequest{
				ComputeEnvironmentName: new(name),
				Type:                   new(unmanaged),
				ServiceRole:            new("batch-role"),
			},
		)
		if e != nil {
			t.Fatal(e)
		}
	}
	one, e := s.DescribeComputeEnvironments(ctx, &api.DescribeComputeEnvironmentsRequest{MaxResults: new(int64(1))})
	if e != nil || one.NextToken == nil {
		t.Fatal(e)
	}
	_, e = s.UpdateComputeEnvironment(
		ctx,
		&api.UpdateComputeEnvironmentRequest{ComputeEnvironment: new("a"), State: new(disabled)},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteComputeEnvironment(ctx, &api.DeleteComputeEnvironmentRequest{ComputeEnvironment: new("a")})
	if e != nil {
		t.Fatal(e)
	}
	two, e := s.DescribeComputeEnvironments(ctx, &api.DescribeComputeEnvironmentsRequest{NextToken: one.NextToken})
	if e != nil || len(two.ComputeEnvironments) != 2 || *two.ComputeEnvironments[0].ComputeEnvironmentName != "b" {
		t.Fatalf("deleted cursor %v %v", two, e)
	}
	_, e = s.DescribeJobQueues(ctx, &api.DescribeJobQueuesRequest{NextToken: one.NextToken})
	clientError(t, e)
	_, e = s.DescribeComputeEnvironments(ctx, &api.DescribeComputeEnvironmentsRequest{MaxResults: new(int64(101))})
	clientError(t, e)
}

func TestQueueAssociationValidationAndIndependentScopes(t *testing.T) {
	ctx := batchContext()
	s := New(&memory.Store{}, nil)
	refs := []api.ComputeEnvironmentOrder{}
	for i := range 4 {
		name := fmt.Sprintf("environment-%d", i)
		r, err := s.CreateComputeEnvironment(ctx, &api.CreateComputeEnvironmentRequest{
			ComputeEnvironmentName: new(name), Type: new(unmanaged), ServiceRole: new("/path/batch"),
		})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(
			refs,
			api.ComputeEnvironmentOrder{Order: new(int64(i)), ComputeEnvironment: r.ComputeEnvironmentArn},
		)
	}
	for _, list := range [][]api.ComputeEnvironmentOrder{
		{},
		refs,
		{refs[0], refs[0]},
		{
			{Order: new(int64(1)), ComputeEnvironment: refs[0].ComputeEnvironment},
			{Order: new(int64(1)), ComputeEnvironment: refs[1].ComputeEnvironment},
		},
		{{Order: new(int64(-1)), ComputeEnvironment: refs[0].ComputeEnvironment}},
		{{Order: new(int64(1)), ComputeEnvironment: new("absent")}},
	} {
		_, err := s.CreateJobQueue(ctx, &api.CreateJobQueueRequest{
			JobQueueName: new("invalid"), Priority: new(int64(1)), ComputeEnvironmentOrder: list,
		})
		clientError(t, err)
	}
	q, err := s.CreateJobQueue(ctx, &api.CreateJobQueueRequest{
		JobQueueName: new("valid-queue"), Priority: new(int64(2147483647)),
		ComputeEnvironmentOrder: []api.ComputeEnvironmentOrder{refs[2], refs[0], refs[1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	described, err := s.DescribeJobQueues(ctx, &api.DescribeJobQueuesRequest{JobQueues: []string{*q.JobQueueArn}})
	if err != nil || len(described.JobQueues[0].ComputeEnvironmentOrder) != 3 ||
		*described.JobQueues[0].ComputeEnvironmentOrder[0].Order != 0 {
		t.Fatalf("ordered associations %v %v", described, err)
	}
	for _, scope := range []request.Scope{
		{AccountID: "111111111111", Region: "us-east-1"},
		{AccountID: "000000000000", Region: "us-west-2"},
	} {
		other := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
		all, scopeErr := s.DescribeJobQueues(other, &api.DescribeJobQueuesRequest{})
		if scopeErr != nil || len(all.JobQueues) != 0 {
			t.Fatal("queue scope leaked")
		}
		input := definitionInput()
		input.ContainerProperties.JobRoleArn = nil
		input.ContainerProperties.ExecutionRoleArn = nil
		revision, scopeErr := s.RegisterJobDefinition(other, input)
		if scopeErr != nil || *revision.Revision != 1 {
			t.Fatalf("independent revision %v %v", revision, scopeErr)
		}
		_, scopeErr = s.CreateJobQueue(other, &api.CreateJobQueueRequest{
			JobQueueName: new("foreign"), Priority: new(int64(1)), ComputeEnvironmentOrder: refs[:1],
		})
		clientError(t, scopeErr)
	}
	_, err = s.UpdateComputeEnvironment(ctx, &api.UpdateComputeEnvironmentRequest{
		ComputeEnvironment: refs[0].ComputeEnvironment, ServiceRole: new("replacement-role"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateComputeEnvironment(ctx, &api.UpdateComputeEnvironmentRequest{
		ComputeEnvironment: refs[0].ComputeEnvironment,
		ServiceRole: new(
			"arn:aws:iam::000000000000:role/aws-service-role/batch.amazonaws.com/AWSServiceRoleForBatch",
		),
	})
	clientError(t, err)
	all, err := s.DescribeComputeEnvironments(ctx, &api.DescribeComputeEnvironmentsRequest{})
	if err != nil || *all.ComputeEnvironments[0].ServiceRole != "arn:aws:iam::000000000000:role/replacement-role" {
		t.Fatalf("role update atomicity %v %v", all, err)
	}
}
