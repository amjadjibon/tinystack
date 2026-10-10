package integration_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/wafv2"
	"github.com/aws/aws-sdk-go-v2/service/wafv2/types"
)

const wafTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Resources:
  IP:
    Type: AWS::WAFv2::IPSet
    Properties:
      Name: stack-ip
      Scope: REGIONAL
      IPAddressVersion: IPV4
      Addresses: ["192.0.2.44/32"]
      Description: Stack IP addresses
      Tags: [{Key: Owner, Value: first}]
  Group:
    Type: AWS::WAFv2::RuleGroup
    Properties:
      Name: stack-group
      Scope: REGIONAL
      Capacity: 10
      Description: Stack group rules
      VisibilityConfig: {SampledRequestsEnabled: false, CloudWatchMetricsEnabled: true, MetricName: group}
      Rules:
        - Name: clients
          Priority: 0
          Statement: {IPSetReferenceStatement: {ARN: !GetAtt IP.Arn}}
          Action: {Block: {}}
          VisibilityConfig: {SampledRequestsEnabled: false, CloudWatchMetricsEnabled: true, MetricName: clients}
      Tags: [{Key: Owner, Value: first}]
  ACL:
    Type: AWS::WAFv2::WebACL
    Properties:
      Name: stack-acl
      Scope: REGIONAL
      Description: Stack ACL configuration
      DefaultAction: {Allow: {}}
      VisibilityConfig: {SampledRequestsEnabled: true, CloudWatchMetricsEnabled: false, MetricName: acl}
      Rules:
        - Name: group-reference
          Priority: 0
          Statement: {RuleGroupReferenceStatement: {ARN: !GetAtt Group.Arn}}
          OverrideAction: {None: {}}
          VisibilityConfig: {SampledRequestsEnabled: false, CloudWatchMetricsEnabled: true, MetricName: group_reference}
      Tags: [{Key: Owner, Value: first}]
  Association:
    Type: AWS::WAFv2::WebACLAssociation
    Properties:
      WebACLArn: !GetAtt ACL.Arn
      ResourceArn: arn:aws:elasticloadbalancing:us-east-1:000000000000:loadbalancer/app/frontend/0123456789abcdef
Outputs:
  IPRef: {Value: !Ref IP}
  IPArn: {Value: !GetAtt IP.Arn}
  IPID: {Value: !GetAtt IP.Id}
  GroupRef: {Value: !Ref Group}
  GroupArn: {Value: !GetAtt Group.Arn}
  ACLRef: {Value: !Ref ACL}
  ACLArn: {Value: !GetAtt ACL.Arn}
  AssociationRef: {Value: !Ref Association}
`

func TestCloudFormationWAFRecords(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	w := wafClient(s, "test", "us-east-1")
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("waf-records"), TemplateBody: new(wafTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, c, "waf-records", cfntypes.StackStatusCreateComplete)
	outputs := map[string]string{}
	for _, o := range stack.Outputs {
		outputs[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	if outputs["IPRef"] != "stack-ip|"+outputs["IPID"]+"|REGIONAL" || outputs["AssociationRef"] != outputs["ACLRef"] ||
		!strings.Contains(outputs["IPArn"], ":regional/ipset/stack-ip/"+outputs["IPID"]) {
		t.Fatalf("Ref/GetAtt %+v", outputs)
	}
	ipID := outputs["IPID"]
	extra := []types.Tag{}
	for i := range 49 {
		extra = append(extra, types.Tag{Key: new(fmt.Sprintf("external%02d", i)), Value: new("keep")})
	}
	_, err = w.TagResource(ctx, &sdk.TagResourceInput{ResourceARN: new(outputs["IPArn"]), Tags: extra})
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.ReplaceAll(wafTemplate, "first", "second")
	updated = strings.Replace(updated, "{Key: Owner, Value: second}", "{Key: Replacement, Value: second}", 1)
	updated = strings.Replace(updated, "192.0.2.44/32", "198.51.100.42/32", 1)
	updated = strings.Replace(updated, "ResourceArn: "+wafTarget, "ResourceArn: "+wafStage, 1)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("waf-records"), TemplateBody: new(updated)})
	if err != nil {
		t.Fatal(err)
	}
	stack = wantStatus(ctx, t, c, "waf-records", cfntypes.StackStatusUpdateComplete)
	for _, o := range stack.Outputs {
		if aws.ToString(o.OutputKey) == "IPID" && aws.ToString(o.OutputValue) != ipID {
			t.Fatal("IP update replaced immutable identity")
		}
	}
	tags, err := w.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: new(outputs["IPArn"])})
	if err != nil || len(tags.TagInfoForResource.TagList) != 50 {
		t.Fatalf("50 tags replacement %+v %v", tags, err)
	}
	for _, tag := range tags.TagInfoForResource.TagList {
		if aws.ToString(tag.Key) == "Owner" {
			t.Fatal("obsolete template tag retained")
		}
	}
	assoc, err := w.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafTarget)})
	if err != nil || assoc.WebACL != nil {
		t.Fatalf("old target retained %+v %v", assoc, err)
	}
	assoc, err = w.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafStage)})
	if err != nil || aws.ToString(assoc.WebACL.ARN) != outputs["ACLArn"] {
		t.Fatalf("new target %+v %v", assoc, err)
	}
	before, err := w.GetIPSet(ctx, &sdk.GetIPSetInput{Name: new("stack-ip"), Id: new(ipID), Scope: types.ScopeRegional})
	if err != nil {
		t.Fatal(err)
	}
	// IP changes first, then a dependent ACL fails; rollback restores both its
	// configuration and template tags at the limit without disturbing outsiders.
	bad := strings.Replace(updated, "198.51.100.42/32", "203.0.113.7/32", 1)
	bad = strings.Replace(bad, "{Key: Replacement, Value: second}", "{Key: Third, Value: third}", 1)
	bad = strings.Replace(bad, "OverrideAction: {None: {}}", "OverrideAction: {None: {}, Count: {}}", 1)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("waf-records"), TemplateBody: new(bad)})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "waf-records", cfntypes.StackStatusUpdateRollbackComplete)
	after, err := w.GetIPSet(ctx, &sdk.GetIPSetInput{Name: new("stack-ip"), Id: new(ipID), Scope: types.ScopeRegional})
	if err != nil || !reflect.DeepEqual(before.IPSet, after.IPSet) {
		t.Fatalf("rollback configuration %+v %v", after, err)
	}
	afterTags, err := w.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: new(outputs["IPArn"])})
	if err != nil || !reflect.DeepEqual(tags.TagInfoForResource, afterTags.TagInfoForResource) {
		t.Fatalf("rollback tags %+v %v", afterTags, err)
	}
	for _, body := range []string{
		strings.Replace(updated, "Capacity: 10", "Capacity: 10\n      CustomResponseBodies: {}", 1),
		strings.Replace(updated,
			"Statement: {IPSetReferenceStatement: {ARN: !GetAtt IP.Arn}}",
			"Statement: {IPSetReferenceStatement: {ARN: !GetAtt IP.Arn, IPSetForwardedIPConfig: null}}", 1),
	} {
		_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("waf-records"), TemplateBody: new(body)})
		if err == nil {
			wantStatus(ctx, t, c, "waf-records", cfntypes.StackStatusUpdateRollbackComplete)
		} else {
			code(t, err, "ValidationError")
		}
	}
	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("waf-records")})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)
	ips, err := w.ListIPSets(ctx, &sdk.ListIPSetsInput{Scope: types.ScopeRegional})
	if err != nil || len(ips.IPSets) != 0 {
		t.Fatalf("delete IP %+v %v", ips, err)
	}
	groups, err := w.ListRuleGroups(ctx, &sdk.ListRuleGroupsInput{Scope: types.ScopeRegional})
	if err != nil || len(groups.RuleGroups) != 0 {
		t.Fatalf("delete group %+v %v", groups, err)
	}
	acls, err := w.ListWebACLs(ctx, &sdk.ListWebACLsInput{Scope: types.ScopeRegional})
	if err != nil || len(acls.WebACLs) != 0 {
		t.Fatalf("delete ACL %+v %v", acls, err)
	}
}

func TestCloudFormationWAFCreateRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	bad := strings.Replace(wafTemplate, "DefaultAction: {Allow: {}}", "DefaultAction: {}", 1)
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("waf-rollback"), TemplateBody: new(bad)})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "waf-rollback", cfntypes.StackStatusRollbackComplete)
	w := wafClient(s, "test", "us-east-1")
	ips, err := w.ListIPSets(ctx, &sdk.ListIPSetsInput{Scope: types.ScopeRegional})
	if err != nil || len(ips.IPSets) != 0 {
		t.Fatalf("create rollback IP %+v %v", ips, err)
	}
	groups, err := w.ListRuleGroups(ctx, &sdk.ListRuleGroupsInput{Scope: types.ScopeRegional})
	if err != nil || len(groups.RuleGroups) != 0 {
		t.Fatalf("create rollback group %+v %v", groups, err)
	}
}

func TestCloudFormationWAFAssociationReplacementAndRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	w := wafClient(s, "test", "us-east-1")
	// Two ACLs share the same rule group; replace only the association's ACL.
	second := `  SecondACL:
    Type: AWS::WAFv2::WebACL
    Properties:
      Name: second-acl
      Scope: REGIONAL
      DefaultAction: {Block: {}}
      VisibilityConfig: {SampledRequestsEnabled: false, CloudWatchMetricsEnabled: false, MetricName: second}
`
	body := strings.Replace(wafTemplate, "  Association:\n", second+"  Association:\n", 1)
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("waf-associations"), TemplateBody: new(body)})
	if err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, c, "waf-associations", cfntypes.StackStatusCreateComplete)
	first, err := w.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafTarget)})
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(body, "WebACLArn: !GetAtt ACL.Arn", "WebACLArn: !GetAtt SecondACL.Arn", 1)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("waf-associations"), TemplateBody: new(updated)})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "waf-associations", cfntypes.StackStatusUpdateComplete)
	got, err := w.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafTarget)})
	if err != nil || got.WebACL == nil || aws.ToString(got.WebACL.Name) != "second-acl" {
		t.Fatalf("replacement cleanup removed new association %+v %v", got, err)
	}
	// Replacing the association succeeds, then a dependent invalid resource
	// forces rollback. The previous association must be restored with its ACL.
	badResource := `  ZFailure:
    Type: AWS::WAFv2::IPSet
    DependsOn: Association
    Properties:
      Scope: REGIONAL
      IPAddressVersion: IPV4
      Addresses: ["0.0.0.0/0"]
`
	bad := strings.Replace(body, "Outputs:\n", badResource+"Outputs:\n", 1)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("waf-associations"), TemplateBody: new(bad)})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "waf-associations", cfntypes.StackStatusUpdateRollbackComplete)
	got, err = w.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafTarget)})
	if err != nil || got.WebACL == nil || aws.ToString(got.WebACL.Name) != "second-acl" {
		t.Fatalf("association rollback %+v %v", got, err)
	}
	if aws.ToString(got.WebACL.Id) == aws.ToString(first.WebACL.Id) {
		t.Fatal("rollback retained failed requested association")
	}
	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("waf-associations")})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)
	got, err = w.GetWebACLForResource(ctx, &sdk.GetWebACLForResourceInput{ResourceArn: new(wafTarget)})
	if err != nil || got.WebACL != nil {
		t.Fatalf("association delete %+v %v", got, err)
	}
}
