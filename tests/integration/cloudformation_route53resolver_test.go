package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/route53resolver"
)

func resolverTemplate(vpc, subnet, group, name, associationName string, mutate func(map[string]any)) string {
	endpoint := map[string]any{
		"Name": name, "Direction": "OUTBOUND", "SecurityGroupIds": []any{group},
		"IpAddresses": []any{map[string]any{"SubnetId": subnet}, map[string]any{"SubnetId": subnet, "Ip": "10.0.1.20"}},
		"Tags":        []any{map[string]any{"Key": "env", "Value": "cfn"}},
	}

	rule := map[string]any{
		"Name":               name,
		"RuleType":           "FORWARD",
		"DomainName":         "example.com",
		"ResolverEndpointId": map[string]any{"Fn::GetAtt": []any{"Endpoint", "ResolverEndpointId"}},
		"TargetIps": []any{
			map[string]any{"Ip": "192.0.2.1"},
		},
		"Tags": []any{map[string]any{"Key": "name", "Value": name}},
	}

	resources := map[string]any{
		"Endpoint": map[string]any{"Type": "AWS::Route53Resolver::ResolverEndpoint", "Properties": endpoint},
		"Rule":     map[string]any{"Type": "AWS::Route53Resolver::ResolverRule", "Properties": rule},
		"Association": map[string]any{
			"Type": "AWS::Route53Resolver::ResolverRuleAssociation",
			"Properties": map[string]any{
				"Name":           associationName,
				"VPCId":          vpc,
				"ResolverRuleId": map[string]any{"Fn::GetAtt": []any{"Rule", "ResolverRuleId"}},
			},
		},
	}

	if mutate != nil {
		mutate(resources)
	}

	raw, _ := json.Marshal(map[string]any{"Resources": resources, "Outputs": map[string]any{
		"EndpointName":    map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Endpoint", "Name"}}},
		"RuleName":        map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Rule", "Name"}}},
		"AssociationName": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Association", "Name"}}},
		"EndpointRef":     map[string]any{"Value": map[string]any{"Ref": "Endpoint"}},
		"EndpointId": map[string]any{
			"Value": map[string]any{"Fn::GetAtt": []any{"Endpoint", "ResolverEndpointId"}},
		},
		"EndpointArn":    map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Endpoint", "Arn"}}},
		"RuleRef":        map[string]any{"Value": map[string]any{"Ref": "Rule"}},
		"RuleId":         map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Rule", "ResolverRuleId"}}},
		"AssociationRef": map[string]any{"Value": map[string]any{"Ref": "Association"}},
		"AssociationId": map[string]any{
			"Value": map[string]any{"Fn::GetAtt": []any{"Association", "ResolverRuleAssociationId"}},
		},
	}})
	return string(raw)
}

func TestCloudFormationRoute53ResolverLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := resolverClient(s)
	vpc, subnet, group := resolverNetwork(t, s)
	template := func(name, association string, mutate func(map[string]any)) *string {
		return new(resolverTemplate(*vpc, *subnet, *group, name, association, mutate))
	}

	_, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("resolver-stack"), TemplateBody: template("initial", "association", nil)},
	)
	if err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, cf, "resolver-stack", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)
	for _, prefix := range []string{"Endpoint", "Rule", "Association"} {
		if outputs[prefix+"Ref"] == "" || outputs[prefix+"Ref"] != outputs[prefix+"Id"] {
			t.Fatal(outputs)
		}
	}

	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("resolver-stack"), TemplateBody: template("updated", "association", nil)},
	)
	if err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, cf, "resolver-stack", cfntypes.StackStatusUpdateComplete)
	updated := outputsOf(&st)
	if updated["EndpointRef"] != outputs["EndpointRef"] || updated["RuleRef"] != outputs["RuleRef"] {
		t.Fatal(updated)
	}

	ep, err := c.GetResolverEndpoint(ctx, &sdk.GetResolverEndpointInput{ResolverEndpointId: new(outputs["EndpointId"])})
	if err != nil || *ep.ResolverEndpoint.Name != "updated" {
		t.Fatal(ep, err)
	}

	rule, err := c.GetResolverRule(ctx, &sdk.GetResolverRuleInput{ResolverRuleId: new(outputs["RuleId"])})
	if err != nil || *rule.ResolverRule.Name != "updated" {
		t.Fatal(rule, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: rule.ResolverRule.Arn})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Value != "updated" {
		t.Fatal(tags, err)
	}

	// Each declared unsupported change must fail explicitly and roll the stack back.
	for _, mutation := range []func(map[string]any){
		func(r map[string]any) {
			r["Endpoint"].(map[string]any)["Properties"].(map[string]any)["IpAddresses"] = []any{
				map[string]any{"SubnetId": *subnet, "Ip": "10.0.1.30"}, map[string]any{"SubnetId": *subnet, "Ip": "10.0.1.31"},
			}
		},
		func(r map[string]any) {
			r["Endpoint"].(map[string]any)["Properties"].(map[string]any)["Tags"] = []any{}
		},
		func(r map[string]any) { delete(r["Endpoint"].(map[string]any)["Properties"].(map[string]any), "Name") },
		func(r map[string]any) { delete(r["Rule"].(map[string]any)["Properties"].(map[string]any), "Name") },
		func(r map[string]any) { delete(r["Rule"].(map[string]any)["Properties"].(map[string]any), "TargetIps") },
		func(r map[string]any) {
			delete(r["Rule"].(map[string]any)["Properties"].(map[string]any), "ResolverEndpointId")
		},
	} {
		_, err = cf.UpdateStack(
			ctx,
			&cfn.UpdateStackInput{
				StackName:    new("resolver-stack"),
				TemplateBody: template("updated", "association", mutation),
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		wantStatus(ctx, t, cf, "resolver-stack", cfntypes.StackStatusUpdateRollbackComplete)
	}

	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("resolver-stack"), TemplateBody: template("updated", "replacement", nil)},
	)
	if err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, cf, "resolver-stack", cfntypes.StackStatusUpdateComplete)
	if outputsOf(&st)["AssociationId"] == outputs["AssociationId"] {
		t.Fatal("association Name change did not replace")
	}

	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("resolver-stack")})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	endpoints, err := c.ListResolverEndpoints(ctx, &sdk.ListResolverEndpointsInput{})
	if err != nil || len(endpoints.ResolverEndpoints) != 0 {
		t.Fatal(endpoints, err)
	}

	rules, err := c.ListResolverRules(ctx, &sdk.ListResolverRulesInput{})
	if err != nil || len(rules.ResolverRules) != 0 {
		t.Fatal(rules, err)
	}

	associations, err := c.ListResolverRuleAssociations(ctx, &sdk.ListResolverRuleAssociationsInput{})
	if err != nil || len(associations.ResolverRuleAssociations) != 0 {
		t.Fatal(associations, err)
	}
}

func TestCloudFormationRoute53ResolverOptionalNames(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	vpc, subnet, group := resolverNetwork(t, s)
	body := resolverTemplate(*vpc, *subnet, *group, "ignored", "ignored", func(resources map[string]any) {
		for _, resource := range resources {
			delete(resource.(map[string]any)["Properties"].(map[string]any), "Name")
		}
	})
	_, err := cf.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("resolver-optional"), TemplateBody: new(body)})
	if err != nil {
		t.Fatal(err)
	}
	st := wantStatus(ctx, t, cf, "resolver-optional", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)
	for _, key := range []string{"EndpointName", "RuleName", "AssociationName"} {
		if value, ok := outputs[key]; !ok || value != "" {
			t.Fatal(outputs)
		}
	}

	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("resolver-optional")})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
}
