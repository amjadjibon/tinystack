package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/wafv2"
	"github.com/aws/aws-sdk-go-v2/service/wafv2/types"
)

func wafClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wafVisibility(name string) *types.VisibilityConfig {
	return &types.VisibilityConfig{MetricName: new(name), SampledRequestsEnabled: false, CloudWatchMetricsEnabled: true}
}

func wafIPRule(arn *string) types.Rule {
	return types.Rule{
		Name:             new("ip-rule"),
		Priority:         0,
		Statement:        &types.Statement{IPSetReferenceStatement: &types.IPSetReferenceStatement{ARN: arn}},
		Action:           &types.RuleAction{Block: &types.BlockAction{}},
		VisibilityConfig: wafVisibility("ip-metric"),
	}
}

func wafGroupRule(arn *string) types.Rule {
	return types.Rule{
		Name:             new("group-rule"),
		Priority:         10,
		Statement:        &types.Statement{RuleGroupReferenceStatement: &types.RuleGroupReferenceStatement{ARN: arn}},
		OverrideAction:   &types.OverrideAction{None: &types.NoneAction{}},
		VisibilityConfig: wafVisibility("group-metric"),
	}
}

func wafFixture(
	t *testing.T,
	c *sdk.Client,
	scope types.Scope,
) (*types.IPSetSummary, *types.RuleGroupSummary, *types.WebACLSummary) {
	t.Helper()
	ctx := context.Background()
	ip, err := c.CreateIPSet(
		ctx,
		&sdk.CreateIPSetInput{
			Name:             new("clients"),
			Scope:            scope,
			Description:      new("Client addresses"),
			IPAddressVersion: types.IPAddressVersionIpv4,
			Addresses:        []string{"192.0.2.44/32", "192.0.2.0/24"},
			Tags:             []types.Tag{{Key: new("owner"), Value: new("ip")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	group, err := c.CreateRuleGroup(
		ctx,
		&sdk.CreateRuleGroupInput{
			Name:             new("clients-group"),
			Scope:            scope,
			Capacity:         new(int64(10)),
			Description:      new("Client rules"),
			Rules:            []types.Rule{wafIPRule(ip.Summary.ARN)},
			VisibilityConfig: wafVisibility("group"),
			Tags:             []types.Tag{{Key: new("owner"), Value: new("group")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := c.CreateWebACL(
		ctx,
		&sdk.CreateWebACLInput{
			Name:             new("clients-acl"),
			Scope:            scope,
			Description:      new("Client ACL"),
			DefaultAction:    &types.DefaultAction{Allow: &types.AllowAction{}},
			Rules:            []types.Rule{wafGroupRule(group.Summary.ARN)},
			VisibilityConfig: wafVisibility("acl"),
			Tags:             []types.Tag{{Key: new("owner"), Value: new("acl")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return ip.Summary, group.Summary, acl.Summary
}

const (
	wafTarget = "arn:aws:elasticloadbalancing:us-east-1:000000000000:loadbalancer/app/frontend/0123456789abcdef"
	wafStage  = "arn:aws:apigateway:us-east-1::/restapis/abcdefghij/stages/prod"
)

func TestWAFSDKWorkflowAndLocks(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := wafClient(s, "test", "us-east-1")
	ip, g, a := wafFixture(t, c, types.ScopeRegional)
	_, err := c.CreateIPSet(
		ctx,
		&sdk.CreateIPSetInput{
			Name:             ip.Name,
			Scope:            types.ScopeRegional,
			IPAddressVersion: types.IPAddressVersionIpv4,
			Addresses:        []string{},
		},
	)
	code(t, err, "WAFDuplicateItemException")
	got, err := c.GetWebACL(ctx, &sdk.GetWebACLInput{Name: a.Name, Id: a.Id, Scope: types.ScopeRegional})
	if err != nil || got.WebACL.Rules[0].OverrideAction.None == nil ||
		aws.ToString(got.WebACL.ARN) != aws.ToString(a.ARN) {
		t.Fatalf("ACL %+v %v", got, err)
	}
	_, err = c.GetWebACL(ctx, &sdk.GetWebACLInput{Name: new("wrong"), Id: a.Id, Scope: types.ScopeRegional})
	code(t, err, "WAFNonexistentItemException")
	_, err = c.UpdateWebACL(
		ctx,
		&sdk.UpdateWebACLInput{
			Name:             a.Name,
			Id:               a.Id,
			Scope:            types.ScopeRegional,
			LockToken:        new("00000000-0000-0000-0000-000000000000"),
			DefaultAction:    got.WebACL.DefaultAction,
			VisibilityConfig: got.WebACL.VisibilityConfig,
		},
	)
	code(t, err, "WAFOptimisticLockException")
	// Exactly one concurrent caller can replace configuration with the old token.
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			_, e := c.UpdateWebACL(
				ctx,
				&sdk.UpdateWebACLInput{
					Name:             a.Name,
					Id:               a.Id,
					Scope:            types.ScopeRegional,
					LockToken:        got.LockToken,
					Description:      new("Updated ACL"),
					DefaultAction:    &types.DefaultAction{Block: &types.BlockAction{}},
					VisibilityConfig: wafVisibility("updated"),
					Rules: []types.Rule{
						wafGroupRule(g.ARN),
						{
							Name:     new("direct"),
							Priority: 20,
							Statement: &types.Statement{
								IPSetReferenceStatement: &types.IPSetReferenceStatement{ARN: ip.ARN},
							},
							Action:           &types.RuleAction{Count: &types.CountAction{}},
							VisibilityConfig: wafVisibility("direct"),
						},
					},
				},
			)
			results <- e
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else {
			code(t, e, "WAFOptimisticLockException")
		}
	}
	if wins != 1 {
		t.Fatalf("winners %d", wins)
	}
	now, err := c.GetWebACL(ctx, &sdk.GetWebACLInput{Name: a.Name, Id: a.Id, Scope: types.ScopeRegional})
	if err != nil || now.WebACL.DefaultAction.Block == nil || len(now.WebACL.Rules) != 2 ||
		aws.ToString(now.LockToken) == aws.ToString(got.LockToken) {
		t.Fatalf("update %+v %v", now, err)
	}
	for _, target := range []string{wafTarget, wafStage} {
		if _, err = c.AssociateWebACL(
			ctx,
			&sdk.AssociateWebACLInput{WebACLArn: a.ARN, ResourceArn: new(target)},
		); err != nil {
			t.Fatal(err)
		}
	}
	resources, err := c.ListResourcesForWebACL(ctx, &sdk.ListResourcesForWebACLInput{WebACLArn: a.ARN})
	if err != nil || !reflect.DeepEqual(resources.ResourceArns, []string{wafTarget}) {
		t.Fatalf("ALBs %+v %v", resources, err)
	}
	stages, err := c.ListResourcesForWebACL(
		ctx,
		&sdk.ListResourcesForWebACLInput{WebACLArn: a.ARN, ResourceType: types.ResourceTypeApiGateway},
	)
	if err != nil || !reflect.DeepEqual(stages.ResourceArns, []string{wafStage}) {
		t.Fatalf("stages %+v %v", stages, err)
	}
	associated, err := c.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafTarget)})
	if err != nil || !reflect.DeepEqual(now.WebACL, associated.WebACL) {
		t.Fatalf("association %+v %v", associated, err)
	}
	_, err = c.DeleteWebACL(
		ctx,
		&sdk.DeleteWebACLInput{Name: a.Name, Id: a.Id, Scope: types.ScopeRegional, LockToken: now.LockToken},
	)
	code(t, err, "WAFAssociatedItemException")
	_, err = c.DeleteRuleGroup(
		ctx,
		&sdk.DeleteRuleGroupInput{Name: g.Name, Id: g.Id, Scope: types.ScopeRegional, LockToken: g.LockToken},
	)
	code(t, err, "WAFAssociatedItemException")
	_, err = c.DeleteIPSet(
		ctx,
		&sdk.DeleteIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional, LockToken: ip.LockToken},
	)
	code(t, err, "WAFAssociatedItemException")
	// Replacement associations leave the old ACL free for deletion.
	replacement, err := c.CreateWebACL(
		ctx,
		&sdk.CreateWebACLInput{
			Name:             new("replacement"),
			Scope:            types.ScopeRegional,
			DefaultAction:    &types.DefaultAction{Allow: &types.AllowAction{}},
			VisibilityConfig: wafVisibility("replacement"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.AssociateWebACL(
		ctx,
		&sdk.AssociateWebACLInput{WebACLArn: replacement.Summary.ARN, ResourceArn: new(wafTarget)},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DisassociateWebACL(ctx, &sdk.DisassociateWebACLInput{ResourceArn: new(wafStage)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DisassociateWebACL(ctx, &sdk.DisassociateWebACLInput{ResourceArn: new(wafStage)})
	if err != nil {
		t.Fatal(err)
	}
	none, err := c.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafStage)})
	if err != nil || none.WebACL != nil {
		t.Fatalf("disassociation %+v %v", none, err)
	}
	_, err = c.DeleteWebACL(
		ctx,
		&sdk.DeleteWebACLInput{Name: a.Name, Id: a.Id, Scope: types.ScopeRegional, LockToken: now.LockToken},
	)
	if err != nil {
		t.Fatal(err)
	}
	group, err := c.GetRuleGroup(ctx, &sdk.GetRuleGroupInput{Name: g.Name, Id: g.Id, Scope: types.ScopeRegional})
	if err != nil || aws.ToInt64(group.RuleGroup.Capacity) != 10 || len(group.RuleGroup.Rules) != 1 {
		t.Fatalf("group %+v %v", group, err)
	}
	update, err := c.UpdateRuleGroup(
		ctx,
		&sdk.UpdateRuleGroupInput{
			Name:             g.Name,
			Id:               g.Id,
			Scope:            types.ScopeRegional,
			LockToken:        group.LockToken,
			Description:      new("Empty group"),
			VisibilityConfig: wafVisibility("empty"),
			Rules:            []types.Rule{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteRuleGroup(
		ctx,
		&sdk.DeleteRuleGroupInput{Name: g.Name, Id: g.Id, Scope: types.ScopeRegional, LockToken: group.LockToken},
	)
	code(t, err, "WAFOptimisticLockException")
	_, err = c.DeleteRuleGroup(
		ctx,
		&sdk.DeleteRuleGroupInput{Name: g.Name, Id: g.Id, Scope: types.ScopeRegional, LockToken: update.NextLockToken},
	)
	if err != nil {
		t.Fatal(err)
	}
	ips, err := c.GetIPSet(ctx, &sdk.GetIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional})
	if err != nil || len(ips.IPSet.Addresses) != 2 {
		t.Fatalf("IP %+v %v", ips, err)
	}
	upd, err := c.UpdateIPSet(
		ctx,
		&sdk.UpdateIPSetInput{
			Name:        ip.Name,
			Id:          ip.Id,
			Scope:       types.ScopeRegional,
			LockToken:   ips.LockToken,
			Addresses:   []string{},
			Description: new("Cleared addresses"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteIPSet(
		ctx,
		&sdk.DeleteIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional, LockToken: upd.NextLockToken},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetIPSet(ctx, &sdk.GetIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional})
	code(t, err, "WAFNonexistentItemException")
}

func TestWAFScopesPaginationAndTags(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := wafClient(s, "test", "us-east-1")
	ip, g, a := wafFixture(t, c, types.ScopeRegional)
	globalIP, _, globalACL := wafFixture(t, c, types.ScopeCloudfront)
	if !strings.Contains(aws.ToString(globalIP.ARN), ":global/ipset/") ||
		!strings.Contains(aws.ToString(ip.ARN), ":regional/ipset/") {
		t.Fatal("scope ARNs")
	}
	_, err := c.CreateWebACL(
		ctx,
		&sdk.CreateWebACLInput{
			Name:             new("cross-scope"),
			Scope:            types.ScopeCloudfront,
			DefaultAction:    &types.DefaultAction{Allow: &types.AllowAction{}},
			VisibilityConfig: wafVisibility("cross"),
			Rules:            []types.Rule{wafGroupRule(g.ARN)},
		},
	)
	code(t, err, "WAFNonexistentItemException")
	for _, foreign := range []*sdk.Client{wafClient(s, "team-b", "us-east-1"), wafClient(s, "test", "eu-west-1")} {
		_, err = foreign.GetIPSet(ctx, &sdk.GetIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional})
		code(t, err, "WAFNonexistentItemException")
		_, err = foreign.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceARN: a.ARN, Tags: []types.Tag{{Key: new("foreign"), Value: new("yes")}}},
		)
		code(t, err, "WAFNonexistentItemException")
	}
	_, err = wafClient(s, "test", "eu-west-1").ListIPSets(ctx, &sdk.ListIPSetsInput{Scope: types.ScopeCloudfront})
	code(t, err, "WAFInvalidParameterException")
	_, err = wafClient(s, "test", "not-a-region").ListIPSets(ctx, &sdk.ListIPSetsInput{Scope: types.ScopeRegional})
	code(t, err, "WAFInvalidParameterException")
	_, err = c.AssociateWebACL(ctx, &sdk.AssociateWebACLInput{WebACLArn: globalACL.ARN, ResourceArn: new(wafTarget)})
	code(t, err, "WAFNonexistentItemException")
	_, err = c.AssociateWebACL(
		ctx,
		&sdk.AssociateWebACLInput{
			WebACLArn:   a.ARN,
			ResourceArn: new(strings.Replace(wafTarget, "000000000000", "111111111111", 1)),
		},
	)
	code(t, err, "WAFNonexistentItemException")
	_, err = c.AssociateWebACL(
		ctx,
		&sdk.AssociateWebACLInput{
			WebACLArn:   a.ARN,
			ResourceArn: new("arn:aws:cloudfront::000000000000:distribution/ABC"),
		},
	)
	code(t, err, "WAFInvalidParameterException")
	for _, name := range []string{"alpha", "zeta"} {
		_, err = c.CreateIPSet(
			ctx,
			&sdk.CreateIPSetInput{
				Name:             new(name),
				Scope:            types.ScopeRegional,
				IPAddressVersion: types.IPAddressVersionIpv6,
				Addresses:        []string{"2001:db8::1/128", "2001:db8::/64"},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.ListIPSets(ctx, &sdk.ListIPSetsInput{Scope: types.ScopeRegional, Limit: new(int32(1))})
	if err != nil || len(first.IPSets) != 1 || aws.ToString(first.IPSets[0].Name) != "alpha" ||
		first.NextMarker == nil {
		t.Fatalf("page %+v %v", first, err)
	}
	second, err := c.ListIPSets(
		ctx,
		&sdk.ListIPSetsInput{Scope: types.ScopeRegional, Limit: new(int32(1)), NextMarker: first.NextMarker},
	)
	if err != nil || aws.ToString(second.IPSets[0].Name) != "clients" {
		t.Fatalf("second %+v %v", second, err)
	}
	last, err := c.ListIPSets(
		ctx,
		&sdk.ListIPSetsInput{Scope: types.ScopeRegional, Limit: new(int32(1)), NextMarker: second.NextMarker},
	)
	if err != nil || len(last.IPSets) != 1 || aws.ToString(last.IPSets[0].Name) != "zeta" || last.NextMarker != nil {
		t.Fatalf("last page %+v %v", last, err)
	}
	alpha := first.IPSets[0]
	_, err = c.DeleteIPSet(
		ctx,
		&sdk.DeleteIPSetInput{Name: alpha.Name, Id: alpha.Id, Scope: types.ScopeRegional, LockToken: alpha.LockToken},
	)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.ListIPSets(
		ctx,
		&sdk.ListIPSetsInput{Scope: types.ScopeRegional, Limit: new(int32(1)), NextMarker: first.NextMarker},
	)
	if err != nil || len(resumed.IPSets) != 1 || aws.ToString(resumed.IPSets[0].Name) != "clients" {
		t.Fatalf("resume after deletion %+v %v", resumed, err)
	}
	for _, input := range []*sdk.ListIPSetsInput{
		{Scope: types.ScopeCloudfront, NextMarker: first.NextMarker},
		{Scope: types.ScopeRegional, Limit: new(int32(0))},
		{Scope: types.ScopeRegional, Limit: new(int32(101))},
		{Scope: types.ScopeRegional, NextMarker: new("broken")},
	} {
		_, err = c.ListIPSets(ctx, input)
		code(t, err, "WAFInvalidParameterException")
	}
	_, err = wafClient(
		s,
		"team-b",
		"us-east-1",
	).ListIPSets(ctx, &sdk.ListIPSetsInput{Scope: types.ScopeRegional, NextMarker: first.NextMarker})
	code(t, err, "WAFInvalidParameterException")
	_, err = c.ListWebACLs(ctx, &sdk.ListWebACLsInput{Scope: types.ScopeRegional, NextMarker: first.NextMarker})
	code(t, err, "WAFInvalidParameterException")
	groups, err := c.ListRuleGroups(ctx, &sdk.ListRuleGroupsInput{Scope: types.ScopeRegional})
	if err != nil || len(groups.RuleGroups) != 1 ||
		aws.ToString(groups.RuleGroups[0].LockToken) != aws.ToString(g.LockToken) {
		t.Fatalf("groups %+v %v", groups, err)
	}
	acls, err := c.ListWebACLs(ctx, &sdk.ListWebACLsInput{Scope: types.ScopeCloudfront})
	if err != nil || len(acls.WebACLs) != 1 {
		t.Fatalf("ACL list %+v %v", acls, err)
	}
	tags := []types.Tag{}
	for i := range 49 {
		tags = append(tags, types.Tag{Key: new(fmt.Sprintf("k%02d", i)), Value: new("value")})
	}
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ResourceARN: ip.ARN, Tags: tags})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceARN: ip.ARN,
			Tags: []types.Tag{
				{Key: new("owner"), Value: new("changed")},
				{Key: new("extra"), Value: new("overflow")},
			},
		},
	)
	code(t, err, "WAFLimitsExceededException")
	saved, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: ip.ARN})
	if err != nil || len(saved.TagInfoForResource.TagList) != 50 {
		t.Fatalf("tags %+v %v", saved, err)
	}
	for _, tag := range saved.TagInfoForResource.TagList {
		if aws.ToString(tag.Key) == "owner" && aws.ToString(tag.Value) != "ip" {
			t.Fatal("failed tag merge changed old value")
		}
	}
	page, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: ip.ARN, Limit: new(int32(1))})
	if err != nil || page.NextMarker == nil {
		t.Fatalf("tag page %+v %v", page, err)
	}
	_, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: a.ARN, NextMarker: page.NextMarker})
	code(t, err, "WAFInvalidParameterException")
	_, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceARN: ip.ARN, TagKeys: []string{"owner", "aws:reserved"}},
	)
	code(t, err, "WAFInvalidParameterException")
	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceARN: ip.ARN, TagKeys: []string{"k00", "missing"}})
	if err != nil {
		t.Fatal(err)
	}
	// Configuration tokens survive tagging and untagging.
	token, err := c.GetIPSet(ctx, &sdk.GetIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional})
	if err != nil || aws.ToString(token.LockToken) != aws.ToString(ip.LockToken) {
		t.Fatalf("token %+v %v", token, err)
	}
	globalTag, err := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceARN: globalIP.ARN, Tags: []types.Tag{{Key: new("global"), Value: new("yes")}}},
	)
	if err != nil {
		t.Fatalf("global tags %+v %v", globalTag, err)
	}
	tagging := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	_, err = tagging.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{aws.ToString(a.ARN)},
			Tags:            map[string]string{"generic": "yes"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := tagging.GetResources(
		ctx,
		&resourcegroupstaggingapi.GetResourcesInput{ResourceTypeFilters: []string{"wafv2:webacl"}},
	)
	if err != nil || len(discovered.ResourceTagMappingList) != 2 {
		t.Fatalf("inventory %+v %v", discovered, err)
	}
}

func wafWire(t *testing.T, s *httptest.Server, operation, body string) (status int, result map[string]any) {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	r.Header.Set("X-Amz-Target", "AWSWAF_20190729."+operation)
	r.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/wafv2/aws4_request, SignedHeaders=host, Signature=dummy",
	)
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatalf("wire %s %v", data, err)
	}
	if resp.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
		t.Fatalf("content type %v", resp.Header)
	}
	return resp.StatusCode, out
}

func TestWAFRawWireValidationAndAtomicity(t *testing.T) {
	s := server(t)
	c := wafClient(s, "test", "us-east-1")
	ip, g, a := wafFixture(t, c, types.ScopeRegional)
	ctx := context.Background()
	before, err := c.GetWebACL(ctx, &sdk.GetWebACLInput{Name: a.Name, Id: a.Id, Scope: types.ScopeRegional})
	if err != nil {
		t.Fatal(err)
	}
	acl := fmt.Sprintf(
		`{"Name":%q,"Id":%q,"Scope":"REGIONAL","LockToken":%q,"Description":"Invalid update",`+
			`"DefaultAction":{"Allow":{}},"VisibilityConfig":{"SampledRequestsEnabled":false,`+
			`"CloudWatchMetricsEnabled":true,"MetricName":"acl"},"Rules":[{"Name":"group-rule","Priority":10,`+
			`"Statement":{"RuleGroupReferenceStatement":{"ARN":%q}},"OverrideAction":{"None":{}},`+
			`"VisibilityConfig":{"SampledRequestsEnabled":false,"CloudWatchMetricsEnabled":true,`+
			`"MetricName":"group"}}]}`,
		*a.Name,
		*a.Id,
		*a.LockToken,
		*g.ARN,
	)
	cases := []string{
		strings.Replace(acl, `"Description":"Invalid update"`, `"CaptchaConfig":null`, 1),
		strings.Replace(acl, `"None":{}`, `"None":{},"Count":{}`, 1),
		strings.Replace(acl, `"Allow":{}`, `"Allow":{"CustomRequestHandling":null}`, 1),
		strings.Replace(acl, `"Allow":{}`, `"Allow":{},"Block":{}`, 1),
		strings.Replace(acl, `"RuleGroupReferenceStatement":`, `"ManagedRuleGroupStatement":`, 1),
		strings.Replace(acl, `"RuleGroupReferenceStatement":`, `"RateBasedStatement":`, 1),
		strings.Replace(acl, `"RuleGroupReferenceStatement":`, `"AndStatement":`, 1),
		strings.Replace(acl, fmt.Sprintf(`"ARN":%q`, *g.ARN), fmt.Sprintf(`"ARN":%q,"ExcludedRules":[]`, *g.ARN), 1),
		strings.Replace(acl, `"Statement":{`, `"Statement":{"ByteMatchStatement":false,`, 1),
		strings.Replace(acl, `"DefaultAction":{"Allow":{}}`, `"DefaultAction":{}`, 1),
		strings.Replace(acl, `"OverrideAction":{"None":{}}`, `"OverrideAction":{}`, 1),
		strings.Replace(acl, `"Priority":10`, `"Priority":-1`, 1),
		strings.Replace(acl, `"Priority":10`, `"Priority":1.5`, 1),
		strings.Replace(acl, `"MetricName":"acl"`, `"MetricName":"All"`, 1),
		strings.Replace(acl, `"SampledRequestsEnabled":false,`, ``, 1),
		strings.Replace(
			acl,
			`"VisibilityConfig":{"SampledRequestsEnabled":false,"CloudWatchMetricsEnabled":true,"MetricName":"acl"}`,
			`"VisibilityConfig":{}`,
			1,
		),
		strings.Replace(acl, `"Name":"group-rule"`, `"Name":"bad name"`, 1),
	}
	for _, body := range cases {
		status, out := wafWire(t, s, "UpdateWebACL", body)
		if status != 400 || out["__type"] != "WAFInvalidParameterException" {
			t.Fatalf("body %s status %d %+v", body, status, out)
		}
	}
	missingToken := strings.Replace(acl, fmt.Sprintf(`"LockToken":%q,`, *a.LockToken), "", 1)
	status, out := wafWire(t, s, "UpdateWebACL", missingToken)
	if status != 400 || out["__type"] != "WAFOptimisticLockException" {
		t.Fatalf("missing lock %d %+v", status, out)
	}
	after, err := c.GetWebACL(ctx, &sdk.GetWebACLInput{Name: a.Name, Id: a.Id, Scope: types.ScopeRegional})
	if err != nil || !reflect.DeepEqual(before.WebACL, after.WebACL) ||
		aws.ToString(before.LockToken) != aws.ToString(after.LockToken) {
		t.Fatalf("invalid writes published %+v %v", after, err)
	}
	// Successful raw Create/Get also exercise target routing and error casing.
	status, out = wafWire(
		t,
		s,
		"CreateIPSet",
		`{"Name":"raw-v6","Scope":"REGIONAL","IPAddressVersion":"IPV6","Addresses":["2001:db8::/64"],`+
			`"Description":"Raw IPv6 addresses"}`,
	)
	if status != 200 {
		t.Fatalf("raw create %d %+v", status, out)
	}
	summary := out["Summary"].(map[string]any)
	status, out = wafWire(t, s, "GetIPSet", fmt.Sprintf(`{"Name":"raw-v6","Scope":"REGIONAL","Id":%q}`, summary["Id"]))
	if status != 200 || out["LockToken"] != summary["LockToken"] {
		t.Fatalf("raw get %d %+v", status, out)
	}
	for _, address := range []string{
		"0.0.0.0/0",
		"192.0.2.0/33",
		"192.0.2.1",
		"192.0.2.1/24 ",
		"::/0",
		"::ffff:192.0.2.1/128",
		"2001:db8::/129",
		"",
	} {
		_, err = c.UpdateIPSet(
			ctx,
			&sdk.UpdateIPSetInput{
				Name:      ip.Name,
				Id:        ip.Id,
				Scope:     types.ScopeRegional,
				LockToken: ip.LockToken,
				Addresses: []string{address},
			},
		)
		code(t, err, "WAFInvalidParameterException")
	}
	original, err := c.GetIPSet(ctx, &sdk.GetIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional})
	if err != nil || aws.ToString(original.LockToken) != aws.ToString(ip.LockToken) ||
		len(original.IPSet.Addresses) != 2 {
		t.Fatalf("CIDR atomicity %+v %v", original, err)
	}
	for _, body := range []string{
		`{"Name":"x","Scope":"REGIONAL","IPAddressVersion":"IPV4"}`,
		`{"Name":"x","Scope":"REGIONAL","IPAddressVersion":"IPV4","Addresses":null}`,
		`{"Name":"x","Scope":"regional","IPAddressVersion":"IPV4","Addresses":[]}`,
		`{"Name":"x","Scope":"REGIONAL","IPAddressVersion":"IPV4","Addresses":[],"Tags":[]}`,
	} {
		status, out = wafWire(t, s, "CreateIPSet", body)
		if status != 400 || out["__type"] != "WAFInvalidParameterException" {
			t.Fatalf("invalid input %d %+v", status, out)
		}
	}
}

func TestWAFMissingResourceErrors(t *testing.T) {
	ctx := context.Background()
	c := wafClient(server(t), "test", "us-east-1")
	name, identifier, token := new(
		"missing",
	), new(
		"00000000-0000-0000-0000-000000000000",
	), new(
		"00000000-0000-0000-0000-000000000001",
	)
	arn := new("arn:aws:wafv2:us-east-1:000000000000:regional/webacl/missing/" + *identifier)
	cases := []struct {
		call func() error
		name string
	}{
		{name: "GetWebACL", call: func() error {
			_, e := c.GetWebACL(ctx, &sdk.GetWebACLInput{Name: name, Id: identifier, Scope: types.ScopeRegional})
			return e
		}},
		{name: "GetRuleGroup", call: func() error {
			_, e := c.GetRuleGroup(ctx, &sdk.GetRuleGroupInput{Name: name, Id: identifier, Scope: types.ScopeRegional})
			return e
		}},
		{name: "GetIPSet", call: func() error {
			_, e := c.GetIPSet(ctx, &sdk.GetIPSetInput{Name: name, Id: identifier, Scope: types.ScopeRegional})
			return e
		}},
		{name: "UpdateWebACL", call: func() error {
			_, e := c.UpdateWebACL(
				ctx,
				&sdk.UpdateWebACLInput{
					Name:             name,
					Id:               identifier,
					Scope:            types.ScopeRegional,
					LockToken:        token,
					DefaultAction:    &types.DefaultAction{Allow: &types.AllowAction{}},
					VisibilityConfig: wafVisibility("valid"),
				},
			)
			return e
		}},
		{name: "UpdateRuleGroup", call: func() error {
			_, e := c.UpdateRuleGroup(
				ctx,
				&sdk.UpdateRuleGroupInput{
					Name:             name,
					Id:               identifier,
					Scope:            types.ScopeRegional,
					LockToken:        token,
					VisibilityConfig: wafVisibility("valid"),
				},
			)
			return e
		}},
		{name: "UpdateIPSet", call: func() error {
			_, e := c.UpdateIPSet(
				ctx,
				&sdk.UpdateIPSetInput{
					Name:      name,
					Id:        identifier,
					Scope:     types.ScopeRegional,
					LockToken: token,
					Addresses: []string{},
				},
			)
			return e
		}},
		{name: "DeleteWebACL", call: func() error {
			_, e := c.DeleteWebACL(
				ctx,
				&sdk.DeleteWebACLInput{Name: name, Id: identifier, Scope: types.ScopeRegional, LockToken: token},
			)
			return e
		}},
		{name: "DeleteRuleGroup", call: func() error {
			_, e := c.DeleteRuleGroup(
				ctx,
				&sdk.DeleteRuleGroupInput{Name: name, Id: identifier, Scope: types.ScopeRegional, LockToken: token},
			)
			return e
		}},
		{name: "DeleteIPSet", call: func() error {
			_, e := c.DeleteIPSet(
				ctx,
				&sdk.DeleteIPSetInput{Name: name, Id: identifier, Scope: types.ScopeRegional, LockToken: token},
			)
			return e
		}},
		{name: "TagResource", call: func() error {
			_, e := c.TagResource(
				ctx,
				&sdk.TagResourceInput{ResourceARN: arn, Tags: []types.Tag{{Key: new("valid"), Value: new("yes")}}},
			)
			return e
		}},
		{name: "UntagResource", call: func() error {
			_, e := c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceARN: arn, TagKeys: []string{"valid"}})
			return e
		}},
		{name: "ListTagsForResource", call: func() error {
			_, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: arn})
			return e
		}},
		{name: "AssociateWebACL", call: func() error {
			_, e := c.AssociateWebACL(ctx, &sdk.AssociateWebACLInput{WebACLArn: arn, ResourceArn: new(wafTarget)})
			return e
		}},
		{name: "ListResourcesForWebACL", call: func() error {
			_, e := c.ListResourcesForWebACL(ctx, &sdk.ListResourcesForWebACLInput{WebACLArn: arn})
			return e
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { code(t, test.call(), "WAFNonexistentItemException") })
	}
}
