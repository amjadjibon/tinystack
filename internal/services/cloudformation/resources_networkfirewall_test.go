package cloudformation

import (
	"testing"
)

func TestNetworkFirewallResourceTypesRegistered(t *testing.T) {
	expectedTypes := []struct {
		typ        string
		attributes []string
	}{
		{
			typ: firewallType,
			attributes: []string{
				memberEndpointIds,
				memberFirewallArn,
				memberFirewallId,
			},
		},
		{
			typ: firewallPolicyType,
			attributes: []string{
				memberFirewallPolicyArn,
				memberFirewallPolicyId,
			},
		},
		{
			typ: ruleGroupType,
			attributes: []string{
				memberRuleGroupArn,
				memberRuleGroupId,
			},
		},
		{
			typ:        loggingConfigurationType,
			attributes: []string{},
		},
	}

	for _, tt := range expectedTypes {
		rt := resourceTypeOf(tt.typ)
		if rt == nil {
			t.Fatalf("expected resource type %s to be registered", tt.typ)
		}

		for _, attr := range tt.attributes {
			if !supportsAttribute(tt.typ, attr) {
				t.Errorf("resource type %s should support attribute %s", tt.typ, attr)
			}
		}
	}
}

func TestFirewallEndpointIDs(t *testing.T) {
	ep1 := "vpce-0123456789abcdef0"
	ep2 := "vpce-0fedcba9876543210"
	out := map[string]any{
		"FirewallStatus": map[string]any{
			"SyncStates": map[string]any{
				"us-east-1b": map[string]any{
					"Attachment": map[string]any{
						"EndpointId": ep2,
					},
				},
				"us-east-1a": map[string]any{
					"Attachment": map[string]any{
						"EndpointId": ep1,
					},
				},
			},
		},
	}

	endpoints := firewallEndpointIDs(out)
	if len(endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(endpoints))
	}

	if endpoints[0] != "us-east-1a:vpce-0123456789abcdef0" {
		t.Errorf("expected first endpoint in sorted order, got %v", endpoints[0])
	}

	if endpoints[1] != "us-east-1b:vpce-0fedcba9876543210" {
		t.Errorf("expected second endpoint in sorted order, got %v", endpoints[1])
	}
}
