package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/networkfirewall"
	"github.com/aws/aws-sdk-go-v2/service/networkfirewall/types"
)

func TestNetworkFirewallDisabledTLSHolding(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	c := networkFirewallClient(s, "test", "us-east-1")
	// Cross-account lookups cannot see the policy created below.
	other := networkFirewallClient(s, "team-b", "us-east-1")
	if _, err := other.DescribeFirewallPolicy(ctx, &networkfirewall.DescribeFirewallPolicyInput{
		FirewallPolicyName: new("no-tls"),
	}); err == nil {
		t.Fatal("missing cross-account policy accepted")
	}

	otherRegion := networkFirewallClient(s, "test", "eu-west-1")
	if _, err := otherRegion.DescribeFirewallPolicy(ctx, &networkfirewall.DescribeFirewallPolicyInput{
		FirewallPolicyName: new("no-tls"),
	}); err == nil {
		t.Fatal("missing cross-region policy accepted")
	}

	in := &networkfirewall.CreateFirewallPolicyInput{
		FirewallPolicyName: new("no-tls"),
		FirewallPolicy: &types.FirewallPolicy{
			StatelessDefaultActions: []string{
				"aws:forward_to_sfe",
			},
			StatelessFragmentDefaultActions: []string{"aws:forward_to_sfe"},
			EnableTLSSessionHolding:         new(false),
		},
	}
	out, err := c.CreateFirewallPolicy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = networkFirewallClient(s, "test", "us-east-1")
	got, err := c.DescribeFirewallPolicy(ctx, &networkfirewall.DescribeFirewallPolicyInput{
		FirewallPolicyArn: out.FirewallPolicyResponse.FirewallPolicyArn,
	})
	if err != nil || got.FirewallPolicy.EnableTLSSessionHolding == nil ||
		aws.ToBool(got.FirewallPolicy.EnableTLSSessionHolding) {
		t.Fatalf("policy: %+v %v", got, err)
	}

	in.FirewallPolicy.EnableTLSSessionHolding = new(true)
	if _, err = c.UpdateFirewallPolicy(ctx, &networkfirewall.UpdateFirewallPolicyInput{
		FirewallPolicyArn: out.FirewallPolicyResponse.FirewallPolicyArn,
		UpdateToken:       got.UpdateToken, FirewallPolicy: in.FirewallPolicy,
	}); err == nil {
		t.Fatal("enabled TLS session holding accepted")
	}
}
