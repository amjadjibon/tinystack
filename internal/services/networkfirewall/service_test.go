package networkfirewall

import (
	"context"
	"fmt"
	"testing"

	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/networkfirewall"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testCtx() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
}

// testService returns a service whose EC2 network has a VPC with subnets in two Availability Zones.
func testService(t *testing.T, ctx context.Context) (s *Service, vpcID string, subnets []string) {
	t.Helper()
	store := &memory.Store{}
	network := ec2.New(store)
	vpc, err := network.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.92.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	for i, zone := range []string{"us-east-1a", "us-east-1b"} {
		sn, subnetErr := network.CreateSubnet(ctx, &ec2api.CreateSubnetRequest{
			VpcId:            vpc.Vpc.VpcId,
			CidrBlock:        new(fmt.Sprintf("10.92.%d.0/24", i)),
			AvailabilityZone: new(zone),
		})
		if subnetErr != nil {
			t.Fatal(subnetErr)
		}

		subnets = append(subnets, *sn.Subnet.SubnetId)
	}

	return New(store, network), *vpc.Vpc.VpcId, subnets
}

func TestRuleGroupValidation(t *testing.T) {
	ctx := testCtx()
	s, _, _ := testService(t, ctx)

	// Missing capacity: model constraints are checked by the generated Invoke.
	_, err := api.Descriptor(s).Operations["CreateRuleGroup"].Invoke(ctx, &api.CreateRuleGroupRequest{
		RuleGroupName: new("test-rg-no-cap"),
		Type:          new("STATELESS"),
	})
	if err == nil {
		t.Fatal("expected error with missing capacity")
	}

	// Dry run does not persist
	dryResp, err := s.CreateRuleGroup(ctx, &api.CreateRuleGroupRequest{
		RuleGroupName: new("test-rg-dry"),
		Type:          new("STATELESS"),
		Capacity:      new(int64(100)),
		DryRun:        new(true),
		RuleGroup: &api.RuleGroup{
			RulesSource: &api.RulesSource{
				RulesString: new("pass"),
			},
		},
	})
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}

	if dryResp.RuleGroupResponse == nil {
		t.Fatal("expected response for dry run")
	}

	// Verify not stored
	_, err = s.DescribeRuleGroup(ctx, &api.DescribeRuleGroupRequest{
		RuleGroupName: new("test-rg-dry"),
	})
	if err == nil {
		t.Fatal("expected ResourceNotFoundException for dry run rule group")
	}

	// Multiple rules sources
	_, err = s.CreateRuleGroup(ctx, &api.CreateRuleGroupRequest{
		RuleGroupName: new("test-rg-multi"),
		Type:          new("STATELESS"),
		Capacity:      new(int64(100)),
		RuleGroup: &api.RuleGroup{
			RulesSource: &api.RulesSource{
				RulesString:                    new("pass"),
				StatelessRulesAndCustomActions: &api.StatelessRulesAndCustomActions{},
			},
		},
	})
	if err == nil {
		t.Fatal("expected error with multiple rules sources")
	}
}

func TestFirewallProtectionsAndTokens(t *testing.T) {
	ctx := testCtx()
	s, vpc, subnets := testService(t, ctx)

	rg, err := s.CreateRuleGroup(ctx, &api.CreateRuleGroupRequest{
		RuleGroupName: new("rg-test"),
		Type:          new("STATELESS"),
		Capacity:      new(int64(100)),
		RuleGroup: &api.RuleGroup{
			RulesSource: &api.RulesSource{
				RulesString: new("pass"),
			},
		},
	})
	if err != nil {
		t.Fatalf("create rule group failed: %v", err)
	}

	pol, err := s.CreateFirewallPolicy(ctx, &api.CreateFirewallPolicyRequest{
		FirewallPolicyName: new("pol-test"),
		FirewallPolicy: &api.FirewallPolicy{
			StatelessDefaultActions:         []string{"aws:pass"},
			StatelessFragmentDefaultActions: []string{"aws:pass"},
			StatelessRuleGroupReferences: []api.StatelessRuleGroupReference{
				{ResourceArn: rg.RuleGroupResponse.RuleGroupArn, Priority: new(int64(1))},
			},
		},
	})
	if err != nil {
		t.Fatalf("create firewall policy failed: %v", err)
	}

	// Cannot delete rule group while in use by policy
	_, err = s.DeleteRuleGroup(ctx, &api.DeleteRuleGroupRequest{
		RuleGroupArn: rg.RuleGroupResponse.RuleGroupArn,
	})
	if err == nil {
		t.Fatal("expected error deleting rule group while in use by policy")
	}

	fw, err := s.CreateFirewall(ctx, &api.CreateFirewallRequest{
		FirewallName:                   new("fw-test"),
		FirewallPolicyArn:              pol.FirewallPolicyResponse.FirewallPolicyArn,
		VpcId:                          new(vpc),
		SubnetMappings:                 []api.SubnetMapping{{SubnetId: new(subnets[0])}},
		DeleteProtection:               new(true),
		FirewallPolicyChangeProtection: new(true),
		SubnetChangeProtection:         new(true),
	})
	if err != nil {
		t.Fatalf("create firewall failed: %v", err)
	}

	desc, err := s.DescribeFirewall(ctx, &api.DescribeFirewallRequest{
		FirewallArn: fw.Firewall.FirewallArn,
	})
	if err != nil {
		t.Fatalf("describe firewall failed: %v", err)
	}

	token := desc.UpdateToken

	// Cannot delete policy while associated with firewall
	_, err = s.DeleteFirewallPolicy(ctx, &api.DeleteFirewallPolicyRequest{
		FirewallPolicyArn: pol.FirewallPolicyResponse.FirewallPolicyArn,
	})
	if err == nil {
		t.Fatal("expected error deleting policy while in use by firewall")
	}

	// Cannot delete firewall with delete protection
	_, err = s.DeleteFirewall(ctx, &api.DeleteFirewallRequest{
		FirewallArn: fw.Firewall.FirewallArn,
	})
	if err == nil {
		t.Fatal("expected error deleting firewall with delete protection")
	}

	// Token mismatch
	_, err = s.UpdateFirewallDescription(ctx, &api.UpdateFirewallDescriptionRequest{
		FirewallArn: fw.Firewall.FirewallArn,
		UpdateToken: new("bad-token"),
		Description: new("fail"),
	})
	if err == nil {
		t.Fatal("expected error with invalid token")
	}

	// Associate policy while policy change protection is enabled
	pol2, err := s.CreateFirewallPolicy(ctx, &api.CreateFirewallPolicyRequest{
		FirewallPolicyName: new("pol-test-2"),
		FirewallPolicy: &api.FirewallPolicy{
			StatelessDefaultActions:         []string{"aws:pass"},
			StatelessFragmentDefaultActions: []string{"aws:pass"},
		},
	})
	if err != nil {
		t.Fatalf("create firewall policy 2 failed: %v", err)
	}

	_, err = s.AssociateFirewallPolicy(ctx, &api.AssociateFirewallPolicyRequest{
		FirewallArn:       fw.Firewall.FirewallArn,
		FirewallPolicyArn: pol2.FirewallPolicyResponse.FirewallPolicyArn,
		UpdateToken:       token,
	})
	if err == nil {
		t.Fatal("expected error associating policy with change protection enabled")
	}

	// Associate subnets while subnet change protection is enabled
	_, err = s.AssociateSubnets(ctx, &api.AssociateSubnetsRequest{
		FirewallArn:    fw.Firewall.FirewallArn,
		SubnetMappings: []api.SubnetMapping{{SubnetId: new(subnets[1])}},
		UpdateToken:    token,
	})
	if err == nil {
		t.Fatal("expected error associating subnets with change protection enabled")
	}

	// Disassociate subnets while subnet change protection is enabled
	_, err = s.DisassociateSubnets(ctx, &api.DisassociateSubnetsRequest{
		FirewallArn: fw.Firewall.FirewallArn,
		SubnetIds:   []string{subnets[0]},
		UpdateToken: token,
	})
	if err == nil {
		t.Fatal("expected error disassociating subnets with change protection enabled")
	}
}

func TestLoggingConfigurationValidation(t *testing.T) {
	ctx := testCtx()
	s, vpc, subnets := testService(t, ctx)

	pol, err := s.CreateFirewallPolicy(ctx, &api.CreateFirewallPolicyRequest{
		FirewallPolicyName: new("pol-log-test"),
		FirewallPolicy: &api.FirewallPolicy{
			StatelessDefaultActions:         []string{"aws:pass"},
			StatelessFragmentDefaultActions: []string{"aws:pass"},
		},
	})
	if err != nil {
		t.Fatalf("create firewall policy failed: %v", err)
	}

	fw, err := s.CreateFirewall(ctx, &api.CreateFirewallRequest{
		FirewallName:      new("fw-log-test"),
		FirewallPolicyArn: pol.FirewallPolicyResponse.FirewallPolicyArn,
		VpcId:             new(vpc),
		SubnetMappings:    []api.SubnetMapping{{SubnetId: new(subnets[0])}},
	})
	if err != nil {
		t.Fatalf("create firewall failed: %v", err)
	}

	// Attempting to add 2 log destinations in single call should fail
	_, err = s.UpdateLoggingConfiguration(ctx, &api.UpdateLoggingConfigurationRequest{
		FirewallArn: fw.Firewall.FirewallArn,
		LoggingConfiguration: &api.LoggingConfiguration{
			LogDestinationConfigs: []api.LogDestinationConfig{
				{
					LogType:            new("ALERT"),
					LogDestinationType: new("CloudWatchLogs"),
					LogDestination:     map[string]string{"logGroup": "/aws/alerts"},
				},
				{
					LogType:            new("FLOW"),
					LogDestinationType: new("S3"),
					LogDestination:     map[string]string{"bucketName": "flow-bucket"},
				},
			},
		},
	})
	if err == nil {
		t.Fatal("expected error when adding more than one logging destination at once")
	}

	// Adding 1 destination succeeds
	upd, err := s.UpdateLoggingConfiguration(ctx, &api.UpdateLoggingConfigurationRequest{
		FirewallArn: fw.Firewall.FirewallArn,
		LoggingConfiguration: &api.LoggingConfiguration{
			LogDestinationConfigs: []api.LogDestinationConfig{
				{
					LogType:            new("ALERT"),
					LogDestinationType: new("CloudWatchLogs"),
					LogDestination:     map[string]string{"logGroup": "/aws/alerts"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to add 1 log destination: %v", err)
	}

	if len(upd.LoggingConfiguration.LogDestinationConfigs) != 1 {
		t.Fatalf("unexpected destination configs: %v", upd.LoggingConfiguration.LogDestinationConfigs)
	}
}
