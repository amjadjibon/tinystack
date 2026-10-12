package route53resolver_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/route53resolver"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func setup(t *testing.T) (*route53resolver.Service, context.Context, *api.CreateResolverEndpointRequest, *string) {
	t.Helper()
	ctx := testContext("123456789012", "us-east-1")
	store := memory.New(nil, nil)
	network := ec2.New(store)
	vpc, err := network.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(
		ctx,
		&ec2api.CreateSubnetRequest{VpcId: vpc.Vpc.VpcId, CidrBlock: new("10.0.1.0/24")},
	)
	if err != nil {
		t.Fatal(err)
	}

	group, err := network.CreateSecurityGroup(ctx, &ec2api.CreateSecurityGroupRequest{
		VpcId: vpc.Vpc.VpcId, GroupName: new("resolver"), Description: new("resolver"),
	})
	if err != nil {
		t.Fatal(err)
	}

	input := &api.CreateResolverEndpointRequest{
		CreatorRequestId: new("endpoint-token"), Name: new("outbound"),
		Direction: new("OUTBOUND"), SecurityGroupIds: []string{*group.GroupId},
		IpAddresses: []api.IpAddressRequest{
			{SubnetId: subnet.Subnet.SubnetId, Ip: new("10.0.1.10")},
			{SubnetId: subnet.Subnet.SubnetId, Ip: new("10.0.1.11")},
		}, Tags: []api.Tag{{Key: new("env"), Value: new("test")}},
	}

	return route53resolver.New(store, network), ctx, input, vpc.Vpc.VpcId
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestLifecycleAtomicityTagsAndScope(t *testing.T) {
	s, ctx, input, vpcID := setup(t)
	created, err := s.CreateResolverEndpoint(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	ep := created.ResolverEndpoint
	if *ep.Status != "OPERATIONAL" || *ep.IpAddressCount != 2 || *ep.HostVPCId != *vpcID {
		t.Fatal(ep)
	}

	retry, err := s.CreateResolverEndpoint(ctx, input)
	if err != nil || !reflect.DeepEqual(created, retry) {
		t.Fatal(retry, err)
	}

	changed := common.Clone(input)
	changed.Name = new("other")
	_, err = s.CreateResolverEndpoint(ctx, changed)
	wantCode(t, err, "ResourceExistsException")
	_, err = s.UpdateResolverEndpoint(
		ctx,
		&api.UpdateResolverEndpointRequest{ResolverEndpointId: ep.Id, Name: new("123")},
	)
	wantCode(t, err, "InvalidParameterException")
	after, err := s.GetResolverEndpoint(ctx, &api.GetResolverEndpointRequest{ResolverEndpointId: ep.Id})
	if err != nil || !reflect.DeepEqual(ep, after.ResolverEndpoint) {
		t.Fatal("rejected update changed endpoint", after, err)
	}

	updated, err := s.UpdateResolverEndpoint(
		ctx,
		&api.UpdateResolverEndpointRequest{ResolverEndpointId: ep.Id, Name: new("updated")},
	)
	if err != nil || *updated.ResolverEndpoint.Name != "updated" {
		t.Fatal(updated, err)
	}

	retry, err = s.CreateResolverEndpoint(ctx, input)
	if err != nil || *retry.ResolverEndpoint.Name != "updated" {
		t.Fatal(retry, err)
	}

	for _, other := range []context.Context{
		testContext("000000000001", "us-east-1"), testContext("123456789012", "us-west-2"),
	} {
		_, err = s.GetResolverEndpoint(other, &api.GetResolverEndpointRequest{ResolverEndpointId: ep.Id})
		wantCode(t, err, "ResourceNotFoundException")
		_, err = s.CreateResolverEndpoint(other, input)
		wantCode(t, err, "InvalidParameterException")
	}

	ruleInput := &api.CreateResolverRuleRequest{
		CreatorRequestId: new("rule-token"), RuleType: new("FORWARD"),
		DomainName: new("example.com"), Name: new("forward"), ResolverEndpointId: ep.Id,
		TargetIps: []api.TargetAddress{{Ip: new("192.0.2.10")}},
	}

	createdRule, err := s.CreateResolverRule(ctx, ruleInput)
	if err != nil {
		t.Fatal(err)
	}

	rule := createdRule.ResolverRule
	if *rule.DomainName != "example.com." || *rule.TargetIps[0].Port != 53 {
		t.Fatal(rule)
	}

	ruleRetry, err := s.CreateResolverRule(ctx, ruleInput)
	if err != nil || !reflect.DeepEqual(createdRule, ruleRetry) {
		t.Fatal(ruleRetry, err)
	}

	_, err = s.UpdateResolverRule(ctx, &api.UpdateResolverRuleRequest{
		ResolverRuleId: rule.Id,
		Config: &api.ResolverRuleConfig{
			Name:      new("bad-update"),
			TargetIps: []api.TargetAddress{{Ip: new("192.0.2.20"), Port: new(int64(65536))}},
		},
	})
	wantCode(t, err, "InvalidParameterException")
	afterRule, err := s.GetResolverRule(ctx, &api.GetResolverRuleRequest{ResolverRuleId: rule.Id})
	if err != nil || !reflect.DeepEqual(rule, afterRule.ResolverRule) {
		t.Fatal("rejected update changed rule", afterRule, err)
	}

	_, err = s.UpdateResolverRule(ctx, &api.UpdateResolverRuleRequest{
		ResolverRuleId: rule.Id,
		Config:         &api.ResolverRuleConfig{TargetIps: []api.TargetAddress{}},
	})
	wantCode(t, err, "InvalidParameterException")
	updatedRule, err := s.UpdateResolverRule(ctx, &api.UpdateResolverRuleRequest{
		ResolverRuleId: rule.Id,
		Config: &api.ResolverRuleConfig{
			Name:      new("renamed"),
			TargetIps: []api.TargetAddress{{Ip: new("192.0.2.20")}},
		},
	})
	if err != nil || *updatedRule.ResolverRule.Name != "renamed" {
		t.Fatal(updatedRule, err)
	}

	assocInput := &api.AssociateResolverRuleRequest{ResolverRuleId: rule.Id, VPCId: vpcID, Name: new("association")}
	associated, err := s.AssociateResolverRule(ctx, assocInput)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.AssociateResolverRule(ctx, assocInput)
	wantCode(t, err, "ResourceExistsException")
	_, err = s.DeleteResolverRule(ctx, &api.DeleteResolverRuleRequest{ResolverRuleId: rule.Id})
	wantCode(t, err, "ResourceInUseException")
	_, err = s.DeleteResolverEndpoint(ctx, &api.DeleteResolverEndpointRequest{ResolverEndpointId: ep.Id})
	wantCode(t, err, "InvalidRequestException")
	association, err := s.GetResolverRuleAssociation(ctx, &api.GetResolverRuleAssociationRequest{
		ResolverRuleAssociationId: associated.ResolverRuleAssociation.Id,
	})
	if err != nil || *association.ResolverRuleAssociation.Status != "COMPLETE" {
		t.Fatal(association, err)
	}

	system, err := s.CreateResolverRule(ctx, &api.CreateResolverRuleRequest{
		CreatorRequestId: new("system"),
		RuleType:         new("SYSTEM"), DomainName: new("EXAMPLE.COM."),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.AssociateResolverRule(
		ctx,
		&api.AssociateResolverRuleRequest{ResolverRuleId: system.ResolverRule.Id, VPCId: vpcID},
	)
	wantCode(t, err, "ResourceExistsException")
	for _, arn := range []*string{ep.Arn, rule.Arn} {
		_, err = s.TagResource(
			ctx,
			&api.TagResourceRequest{ResourceArn: arn, Tags: []api.Tag{{Key: new("owner"), Value: new("team")}}},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, e := s.ListTagsForResource(
			ctx,
			&api.ListTagsForResourceRequest{ResourceArn: arn, MaxResults: new(int64(1))},
		)
		if e != nil || len(tags.Tags) != 1 {
			t.Fatal(tags, e)
		}

		_, err = s.UntagResource(ctx, &api.UntagResourceRequest{ResourceArn: arn, TagKeys: []string{"owner"}})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = s.TagResource(ctx, &api.TagResourceRequest{ResourceArn: ep.Arn})
	wantCode(t, err, "InvalidParameterException")
	_, err = s.UntagResource(ctx, &api.UntagResourceRequest{ResourceArn: ep.Arn})
	wantCode(t, err, "InvalidParameterException")

	resources, err := s.Resources(ctx)
	if err != nil || len(resources) != 4 {
		t.Fatal(resources, err)
	}

	disassociated, err := s.DisassociateResolverRule(
		ctx,
		&api.DisassociateResolverRuleRequest{ResolverRuleId: rule.Id, VPCId: vpcID},
	)
	if err != nil || *disassociated.ResolverRuleAssociation.Status != "DELETING" {
		t.Fatal(disassociated, err)
	}

	for _, id := range []*string{rule.Id, system.ResolverRule.Id} {
		deleted, e := s.DeleteResolverRule(ctx, &api.DeleteResolverRuleRequest{ResolverRuleId: id})
		if e != nil || *deleted.ResolverRule.Status != "DELETING" {
			t.Fatal(deleted, e)
		}
	}

	deleted, err := s.DeleteResolverEndpoint(ctx, &api.DeleteResolverEndpointRequest{ResolverEndpointId: ep.Id})
	if err != nil || *deleted.ResolverEndpoint.Status != "DELETING" {
		t.Fatal(deleted, err)
	}

	_, err = s.GetResolverEndpoint(ctx, &api.GetResolverEndpointRequest{ResolverEndpointId: ep.Id})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestValidationAndPagination(t *testing.T) {
	s, ctx, input, _ := setup(t)
	for _, mutate := range []func(*api.CreateResolverEndpointRequest){
		func(in *api.CreateResolverEndpointRequest) { in.Direction = new("INBOUND_DELEGATION") },
		func(in *api.CreateResolverEndpointRequest) { in.IpAddresses = in.IpAddresses[:1] },
		func(in *api.CreateResolverEndpointRequest) { in.IpAddresses[0].Ip = new("not-an-ip") },
		func(in *api.CreateResolverEndpointRequest) { in.IpAddresses[0].Ip = new("192.0.2.1") },
		func(in *api.CreateResolverEndpointRequest) { in.SecurityGroupIds = []string{"sg-missing"} },
		func(in *api.CreateResolverEndpointRequest) { in.CreatorRequestId = nil },
	} {
		bad := common.Clone(input)
		mutate(bad)
		_, err := s.CreateResolverEndpoint(ctx, bad)
		wantCode(t, err, "InvalidParameterException")
	}

	for i := range 3 {
		in := common.Clone(input)
		in.CreatorRequestId = new(string(rune('a' + i)))
		if _, err := s.CreateResolverEndpoint(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.ListResolverEndpoints(ctx, &api.ListResolverEndpointsRequest{MaxResults: new(int64(1))})
	if err != nil || len(first.ResolverEndpoints) != 1 || first.NextToken == nil {
		t.Fatal(first, err)
	}

	_, err = s.DeleteResolverEndpoint(
		ctx,
		&api.DeleteResolverEndpointRequest{ResolverEndpointId: first.ResolverEndpoints[0].Id},
	)
	if err != nil {
		t.Fatal(err)
	}

	rest, err := s.ListResolverEndpoints(ctx, &api.ListResolverEndpointsRequest{NextToken: first.NextToken})
	if err != nil || len(rest.ResolverEndpoints) != 2 {
		t.Fatal(rest, err)
	}

	_, err = s.ListResolverEndpoints(ctx, &api.ListResolverEndpointsRequest{NextToken: new("!")})
	wantCode(t, err, "InvalidNextTokenException")
	for _, typ := range []string{"RECURSIVE", "DELEGATE", "invalid"} {
		_, err = s.CreateResolverRule(ctx, &api.CreateResolverRuleRequest{
			CreatorRequestId: new(typ), RuleType: new(typ), DomainName: new("example.com"),
		})
		wantCode(t, err, "InvalidParameterException")
	}

	_, err = s.CreateResolverRule(ctx, &api.CreateResolverRuleRequest{
		CreatorRequestId: new("bad-system"),
		RuleType: new(
			"SYSTEM",
		),
		DomainName: new("example.com"),
		TargetIps:  []api.TargetAddress{{Ip: new("192.0.2.1")}},
	})
	wantCode(t, err, "InvalidParameterException")
}
