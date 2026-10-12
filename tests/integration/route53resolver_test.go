package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/route53resolver"
	"github.com/aws/aws-sdk-go-v2/service/route53resolver/types"
	"github.com/aws/smithy-go"
)

func resolverClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func resolverNetwork(t *testing.T, s *httptest.Server) (vpc, subnet, group *string) {
	t.Helper()
	ctx := context.Background()
	network := ec2Client(s, "test", "us-east-1")
	v, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	sn, err := network.CreateSubnet(ctx, &ec2sdk.CreateSubnetInput{VpcId: v.Vpc.VpcId, CidrBlock: new("10.0.1.0/24")})
	if err != nil {
		t.Fatal(err)
	}

	sg, err := network.CreateSecurityGroup(ctx, &ec2sdk.CreateSecurityGroupInput{
		VpcId: v.Vpc.VpcId, GroupName: new("resolver"), Description: new("Resolver records"),
	})
	if err != nil {
		t.Fatal(err)
	}

	return v.Vpc.VpcId, sn.Subnet.SubnetId, sg.GroupId
}

func resolverError(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestRoute53ResolverSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := resolverClient(s)
	vpc, subnet, group := resolverNetwork(t, s)
	input := &sdk.CreateResolverEndpointInput{
		CreatorRequestId: new("sdk-endpoint"), Name: new("outbound"),
		Direction: types.ResolverEndpointDirectionOutbound, SecurityGroupIds: []string{*group},
		IpAddresses: []types.IpAddressRequest{{SubnetId: subnet}, {SubnetId: subnet, Ip: new("10.0.1.20")}},
		Tags:        []types.Tag{{Key: new("env"), Value: new("sdk")}},
	}

	created, err := c.CreateResolverEndpoint(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	ep := created.ResolverEndpoint
	if ep.Status != types.ResolverEndpointStatusOperational || *ep.IpAddressCount != 2 || *ep.HostVPCId != *vpc {
		t.Fatal(ep)
	}

	retry, err := c.CreateResolverEndpoint(ctx, input)
	if err != nil || *retry.ResolverEndpoint.Id != *ep.Id {
		t.Fatal(retry, err)
	}

	input.Name = new("conflict")
	_, err = c.CreateResolverEndpoint(ctx, input)
	var duplicate *types.ResourceExistsException
	if !errors.As(err, &duplicate) {
		t.Fatalf("expected modeled ResourceExistsException, got %v", err)
	}

	_, err = c.UpdateResolverEndpoint(
		ctx,
		&sdk.UpdateResolverEndpointInput{ResolverEndpointId: ep.Id, Name: new("renamed")},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetResolverEndpoint(ctx, &sdk.GetResolverEndpointInput{ResolverEndpointId: ep.Id})
	if err != nil || *got.ResolverEndpoint.Name != "renamed" {
		t.Fatal(got, err)
	}

	endpoints, err := c.ListResolverEndpoints(ctx, &sdk.ListResolverEndpointsInput{MaxResults: new(int32(1))})
	if err != nil || len(endpoints.ResolverEndpoints) != 1 {
		t.Fatal(endpoints, err)
	}

	ruleCreated, err := c.CreateResolverRule(ctx, &sdk.CreateResolverRuleInput{
		CreatorRequestId: new("sdk-rule"),
		Name:             new("forward"), RuleType: types.RuleTypeOptionForward, DomainName: new("example.com"),
		ResolverEndpointId: ep.Id, TargetIps: []types.TargetAddress{{Ip: new("192.0.2.1")}},
		Tags: []types.Tag{{Key: new("env"), Value: new("sdk")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	rule := ruleCreated.ResolverRule
	if *rule.DomainName != "example.com." || *rule.TargetIps[0].Port != 53 {
		t.Fatal(rule)
	}

	_, err = c.UpdateResolverRule(ctx, &sdk.UpdateResolverRuleInput{
		ResolverRuleId: rule.Id,
		Config: &types.ResolverRuleConfig{
			Name:      new("updated"),
			TargetIps: []types.TargetAddress{{Ip: new("192.0.2.2"), Port: new(int32(5353))}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	gotRule, err := c.GetResolverRule(ctx, &sdk.GetResolverRuleInput{ResolverRuleId: rule.Id})
	if err != nil || *gotRule.ResolverRule.Name != "updated" || *gotRule.ResolverRule.TargetIps[0].Port != 5353 {
		t.Fatal(gotRule, err)
	}

	rules, err := c.ListResolverRules(ctx, &sdk.ListResolverRulesInput{})
	if err != nil || len(rules.ResolverRules) != 1 {
		t.Fatal(rules, err)
	}

	assoc, err := c.AssociateResolverRule(
		ctx,
		&sdk.AssociateResolverRuleInput{ResolverRuleId: rule.Id, VPCId: vpc, Name: new("association")},
	)
	if err != nil {
		t.Fatal(err)
	}

	gotAssoc, err := c.GetResolverRuleAssociation(ctx, &sdk.GetResolverRuleAssociationInput{
		ResolverRuleAssociationId: assoc.ResolverRuleAssociation.Id,
	})
	if err != nil || gotAssoc.ResolverRuleAssociation.Status != types.ResolverRuleAssociationStatusComplete {
		t.Fatal(gotAssoc, err)
	}

	associations, err := c.ListResolverRuleAssociations(ctx, &sdk.ListResolverRuleAssociationsInput{})
	if err != nil || len(associations.ResolverRuleAssociations) != 1 {
		t.Fatal(associations, err)
	}

	_, err = c.DeleteResolverRule(ctx, &sdk.DeleteResolverRuleInput{ResolverRuleId: rule.Id})
	resolverError(t, err, "ResourceInUseException")
	_, err = c.DeleteResolverEndpoint(ctx, &sdk.DeleteResolverEndpointInput{ResolverEndpointId: ep.Id})
	resolverError(t, err, "InvalidRequestException")
	for _, arn := range []*string{ep.Arn, rule.Arn} {
		_, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: arn, Tags: []types.Tag{{Key: new("owner"), Value: new("sdk")}}},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(
			ctx,
			&sdk.ListTagsForResourceInput{ResourceArn: arn, MaxResults: new(int32(1))},
		)
		if e != nil || len(tags.Tags) != 1 || tags.NextToken == nil {
			t.Fatal(tags, e)
		}

		rest, e := c.ListTagsForResource(
			ctx,
			&sdk.ListTagsForResourceInput{ResourceArn: arn, NextToken: tags.NextToken},
		)
		if e != nil || len(rest.Tags) != 1 || *rest.Tags[0].Key != "owner" {
			t.Fatal(rest, e)
		}

		_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"owner"}})
		if err != nil {
			t.Fatal(err)
		}
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*ep.Arn, *rule.Arn}, Tags: map[string]string{"rgta": "yes"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	for _, arn := range []*string{ep.Arn, rule.Arn} {
		tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		if e != nil || len(tags.Tags) != 2 || *tags.Tags[1].Key != "rgta" {
			t.Fatal(tags, e)
		}
	}

	resources, err := tagging.GetResources(
		ctx,
		&rgta.GetResourcesInput{ResourceTypeFilters: []string{"route53resolver"}},
	)
	if err != nil || len(resources.ResourceTagMappingList) != 2 {
		t.Fatal(resources, err)
	}

	disassociated, err := c.DisassociateResolverRule(
		ctx,
		&sdk.DisassociateResolverRuleInput{ResolverRuleId: rule.Id, VPCId: vpc},
	)
	if err != nil || disassociated.ResolverRuleAssociation.Status != types.ResolverRuleAssociationStatusDeleting {
		t.Fatal(disassociated, err)
	}

	deletedRule, err := c.DeleteResolverRule(ctx, &sdk.DeleteResolverRuleInput{ResolverRuleId: rule.Id})
	if err != nil || deletedRule.ResolverRule.Status != types.ResolverRuleStatusDeleting {
		t.Fatal(deletedRule, err)
	}

	deleted, err := c.DeleteResolverEndpoint(ctx, &sdk.DeleteResolverEndpointInput{ResolverEndpointId: ep.Id})
	if err != nil || deleted.ResolverEndpoint.Status != types.ResolverEndpointStatusDeleting {
		t.Fatal(deleted, err)
	}

	_, err = c.GetResolverEndpoint(ctx, &sdk.GetResolverEndpointInput{ResolverEndpointId: ep.Id})
	resolverError(t, err, "ResourceNotFoundException")
	input.CreatorRequestId = new("unsupported")
	input.IpAddresses[0].Ipv6 = new("2001:db8::1")
	_, err = c.CreateResolverEndpoint(ctx, input)
	resolverError(t, err, "ValidationException")
}
