package integration_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/wafv2"
	"github.com/aws/aws-sdk-go-v2/service/wafv2/types"
)

func TestWAFSQLiteRestartAndReset(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	s, stop := persistent(t, path, &fakeRuntime{})
	c := wafClient(s, "test", "us-east-1")
	ip, g, a := wafFixture(t, c, types.ScopeRegional)
	v6, err := c.CreateIPSet(
		ctx,
		&sdk.CreateIPSetInput{
			Name:             new("persist-v6"),
			Scope:            types.ScopeCloudfront,
			IPAddressVersion: types.IPAddressVersionIpv6,
			Addresses:        []string{"2001:db8:1234::/48", "2001:db8::1/128"},
			Description:      new("Global IPv6 addresses"),
		},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	ipUpdate, err := c.UpdateIPSet(
		ctx,
		&sdk.UpdateIPSetInput{
			Name:        ip.Name,
			Id:          ip.Id,
			Scope:       types.ScopeRegional,
			Addresses:   []string{"198.51.100.42/32", "198.51.100.0/24"},
			Description: new("Persistent IPv4 configuration"),
			LockToken:   ip.LockToken,
		},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	for _, arn := range []*string{ip.ARN, g.ARN, a.ARN, v6.Summary.ARN} {
		if _, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{
				ResourceARN: arn,
				Tags:        []types.Tag{{Key: new("persist"), Value: new("after-change")}},
			},
		); err != nil {
			stop()
			t.Fatal(err)
		}
	}
	_, err = c.AssociateWebACL(ctx, &sdk.AssociateWebACLInput{WebACLArn: a.ARN, ResourceArn: new(wafStage)})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	aclBefore, err := c.GetWebACL(ctx, &sdk.GetWebACLInput{Name: a.Name, Id: a.Id, Scope: types.ScopeRegional})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	groupBefore, err := c.GetRuleGroup(ctx, &sdk.GetRuleGroupInput{Name: g.Name, Id: g.Id, Scope: types.ScopeRegional})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	ipBefore, err := c.GetIPSet(ctx, &sdk.GetIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	v6Before, err := c.GetIPSet(
		ctx,
		&sdk.GetIPSetInput{Name: v6.Summary.Name, Id: v6.Summary.Id, Scope: types.ScopeCloudfront},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	tagsBefore, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: a.ARN})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	stop()
	s, stop = persistent(t, path, &fakeRuntime{})
	defer stop()
	c = wafClient(s, "test", "us-east-1")
	aclAfter, err := c.GetWebACL(ctx, &sdk.GetWebACLInput{Name: a.Name, Id: a.Id, Scope: types.ScopeRegional})
	if err != nil || !reflect.DeepEqual(aclBefore.WebACL, aclAfter.WebACL) ||
		aws.ToString(aclBefore.LockToken) != aws.ToString(aclAfter.LockToken) {
		t.Fatalf("ACL restart %+v %v", aclAfter, err)
	}
	groupAfter, err := c.GetRuleGroup(ctx, &sdk.GetRuleGroupInput{Name: g.Name, Id: g.Id, Scope: types.ScopeRegional})
	if err != nil || !reflect.DeepEqual(groupBefore.RuleGroup, groupAfter.RuleGroup) ||
		aws.ToString(groupBefore.LockToken) != aws.ToString(groupAfter.LockToken) {
		t.Fatalf("group restart %+v %v", groupAfter, err)
	}
	ipAfter, err := c.GetIPSet(ctx, &sdk.GetIPSetInput{Name: ip.Name, Id: ip.Id, Scope: types.ScopeRegional})
	if err != nil || !reflect.DeepEqual(ipBefore.IPSet, ipAfter.IPSet) ||
		aws.ToString(ipUpdate.NextLockToken) != aws.ToString(ipAfter.LockToken) {
		t.Fatalf("IP restart %+v %v", ipAfter, err)
	}
	v6After, err := c.GetIPSet(
		ctx,
		&sdk.GetIPSetInput{Name: v6.Summary.Name, Id: v6.Summary.Id, Scope: types.ScopeCloudfront},
	)
	if err != nil || !reflect.DeepEqual(v6Before.IPSet, v6After.IPSet) {
		t.Fatalf("global IPv6 restart %+v %v", v6After, err)
	}
	tagsAfter, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: a.ARN})
	if err != nil || !reflect.DeepEqual(tagsBefore.TagInfoForResource, tagsAfter.TagInfoForResource) {
		t.Fatalf("tags restart %+v %v", tagsAfter, err)
	}
	associated, err := c.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafStage)})
	if err != nil || !reflect.DeepEqual(aclAfter.WebACL, associated.WebACL) {
		t.Fatalf("association restart %+v %v", associated, err)
	}
	// The caller's pre-restart token works, and the original stale token fails.
	_, err = c.UpdateIPSet(
		ctx,
		&sdk.UpdateIPSetInput{
			Name:      ip.Name,
			Id:        ip.Id,
			Scope:     types.ScopeRegional,
			Addresses: []string{},
			LockToken: ip.LockToken,
		},
	)
	code(t, err, "WAFOptimisticLockException")
	_, err = c.UpdateIPSet(
		ctx,
		&sdk.UpdateIPSetInput{
			Name:      ip.Name,
			Id:        ip.Id,
			Scope:     types.ScopeRegional,
			Addresses: []string{},
			LockToken: ipUpdate.NextLockToken,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	status, body := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatalf("reset %d %s", status, body)
	}
	for _, scope := range []types.Scope{types.ScopeRegional, types.ScopeCloudfront} {
		rows, e := c.ListIPSets(ctx, &sdk.ListIPSetsInput{Scope: scope})
		if e != nil || len(rows.IPSets) != 0 {
			t.Fatalf("reset IP %+v %v", rows, e)
		}
	}
	rows, err := c.ListRuleGroups(ctx, &sdk.ListRuleGroupsInput{Scope: types.ScopeRegional})
	if err != nil || len(rows.RuleGroups) != 0 {
		t.Fatalf("reset groups %+v %v", rows, err)
	}
	acls, err := c.ListWebACLs(ctx, &sdk.ListWebACLsInput{Scope: types.ScopeRegional})
	if err != nil || len(acls.WebACLs) != 0 {
		t.Fatalf("reset ACL %+v %v", acls, err)
	}
	none, err := c.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafStage)})
	if err != nil || none.WebACL != nil {
		t.Fatalf("reset association %+v %v", none, err)
	}
}

func TestWAFSigV4VerifyRouting(t *testing.T) {
	s := verifyingServer(t)
	c := wafClient(s, "test", "us-east-1")
	_, _, a := wafFixture(t, c, types.ScopeRegional)
	ctx := context.Background()
	_, err := c.AssociateWebACL(ctx, &sdk.AssociateWebACLInput{WebACLArn: a.ARN, ResourceArn: new(wafTarget)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafTarget)})
	if err != nil || aws.ToString(got.WebACL.Id) != aws.ToString(a.Id) {
		t.Fatalf("signed route %+v %v", got, err)
	}
	_, err = wafClient(s, "wrong", "us-east-1").ListWebACLs(ctx, &sdk.ListWebACLsInput{Scope: types.ScopeRegional})
	code(t, err, "InvalidSignatureException")
}
