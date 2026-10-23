package integration_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/networkfirewall"
	"github.com/aws/aws-sdk-go-v2/service/networkfirewall/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func networkFirewallClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func TestNetworkFirewallLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := networkFirewallClient(s, "test", "us-east-1")
	net := ec2Client(s, "test", "us-east-1")

	// Get a default VPC and subnet
	vpcs, err := net.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil || len(vpcs.Vpcs) == 0 {
		t.Fatalf("failed to describe vpcs: %v", err)
	}

	vpcID := *vpcs.Vpcs[0].VpcId

	subnets, err := net.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	if err != nil || len(subnets.Subnets) == 0 {
		t.Fatalf("failed to describe subnets: %v", err)
	}

	subnet1 := *subnets.Subnets[0].SubnetId

	// 1. Create Rule Group
	rgCreated, err := c.CreateRuleGroup(ctx, &sdk.CreateRuleGroupInput{
		RuleGroupName: aws.String("test-rg"),
		Type:          types.RuleGroupTypeStateless,
		Capacity:      aws.Int32(100),
		RuleGroup: &types.RuleGroup{
			RulesSource: &types.RulesSource{
				RulesString: aws.String("pass ip any any -> any any (sid:1;)"),
			},
		},
		Tags: []types.Tag{
			{Key: aws.String("Env"), Value: aws.String("Test")},
		},
	})
	if err != nil {
		t.Fatalf("CreateRuleGroup failed: %v", err)
	}

	rgArn := *rgCreated.RuleGroupResponse.RuleGroupArn

	// Describe Rule Group & Metadata
	rgDesc, err := c.DescribeRuleGroup(ctx, &sdk.DescribeRuleGroupInput{
		RuleGroupArn: aws.String(rgArn),
	})
	if err != nil {
		t.Fatalf("DescribeRuleGroup failed: %v", err)
	}

	if *rgDesc.RuleGroupResponse.Capacity != 100 {
		t.Fatalf("unexpected capacity: %d", *rgDesc.RuleGroupResponse.Capacity)
	}

	rgMeta, err := c.DescribeRuleGroupMetadata(ctx, &sdk.DescribeRuleGroupMetadataInput{
		RuleGroupArn: aws.String(rgArn),
	})
	if err != nil {
		t.Fatalf("DescribeRuleGroupMetadata failed: %v", err)
	}

	if *rgMeta.Capacity != 100 {
		t.Fatalf("unexpected metadata capacity: %d", *rgMeta.Capacity)
	}

	// List Rule Groups
	rgList, err := c.ListRuleGroups(ctx, &sdk.ListRuleGroupsInput{})
	if err != nil || len(rgList.RuleGroups) == 0 {
		t.Fatalf("ListRuleGroups failed: %v", err)
	}

	// 2. Create Firewall Policy referencing rule group
	policyCreated, err := c.CreateFirewallPolicy(ctx, &sdk.CreateFirewallPolicyInput{
		FirewallPolicyName: aws.String("test-policy"),
		FirewallPolicy: &types.FirewallPolicy{
			StatelessDefaultActions:         []string{"aws:pass"},
			StatelessFragmentDefaultActions: []string{"aws:pass"},
			StatelessRuleGroupReferences: []types.StatelessRuleGroupReference{
				{
					Priority:    aws.Int32(1),
					ResourceArn: aws.String(rgArn),
				},
			},
		},
		Tags: []types.Tag{
			{Key: aws.String("Env"), Value: aws.String("Test")},
		},
	})
	if err != nil {
		t.Fatalf("CreateFirewallPolicy failed: %v", err)
	}

	policyArn := *policyCreated.FirewallPolicyResponse.FirewallPolicyArn

	// Describe Firewall Policy
	polDesc, err := c.DescribeFirewallPolicy(ctx, &sdk.DescribeFirewallPolicyInput{
		FirewallPolicyArn: aws.String(policyArn),
	})
	if err != nil {
		t.Fatalf("DescribeFirewallPolicy failed: %v", err)
	}

	if len(polDesc.FirewallPolicy.StatelessDefaultActions) != 1 ||
		polDesc.FirewallPolicy.StatelessDefaultActions[0] != "aws:pass" {
		t.Fatalf("unexpected stateless default actions: %v", polDesc.FirewallPolicy.StatelessDefaultActions)
	}

	// List Firewall Policies
	polList, err := c.ListFirewallPolicies(ctx, &sdk.ListFirewallPoliciesInput{})
	if err != nil || len(polList.FirewallPolicies) == 0 {
		t.Fatalf("ListFirewallPolicies failed: %v", err)
	}

	// Deleting Rule Group should fail because it is referenced by a policy
	_, err = c.DeleteRuleGroup(ctx, &sdk.DeleteRuleGroupInput{
		RuleGroupArn: aws.String(rgArn),
	})
	if err == nil {
		t.Fatal("expected error deleting rule group referenced by policy")
	}

	// 3. Create Firewall
	fwCreated, err := c.CreateFirewall(ctx, &sdk.CreateFirewallInput{
		FirewallName:      aws.String("test-firewall"),
		FirewallPolicyArn: aws.String(policyArn),
		VpcId:             aws.String(vpcID),
		SubnetMappings: []types.SubnetMapping{
			{SubnetId: aws.String(subnet1)},
		},
		DeleteProtection: true,
		Tags: []types.Tag{
			{Key: aws.String("Env"), Value: aws.String("Test")},
		},
	})
	if err != nil {
		t.Fatalf("CreateFirewall failed: %v", err)
	}

	fwArn := *fwCreated.Firewall.FirewallArn

	// Describe Firewall
	fwDesc, err := c.DescribeFirewall(ctx, &sdk.DescribeFirewallInput{
		FirewallArn: aws.String(fwArn),
	})
	if err != nil {
		t.Fatalf("DescribeFirewall failed: %v", err)
	}

	if fwDesc.FirewallStatus.Status != types.FirewallStatusValueReady {
		t.Fatalf("expected status READY, got %s", fwDesc.FirewallStatus.Status)
	}

	if len(fwDesc.FirewallStatus.SyncStates) == 0 {
		t.Fatalf("expected sync states for subnets")
	}

	// List Firewalls
	fwList, err := c.ListFirewalls(ctx, &sdk.ListFirewallsInput{
		VpcIds: []string{vpcID},
	})
	if err != nil || len(fwList.Firewalls) == 0 {
		t.Fatalf("ListFirewalls failed: %v", err)
	}

	// Deleting Policy should fail because it is associated with firewall
	_, err = c.DeleteFirewallPolicy(ctx, &sdk.DeleteFirewallPolicyInput{
		FirewallPolicyArn: aws.String(policyArn),
	})
	if err == nil {
		t.Fatal("expected error deleting policy associated with firewall")
	}

	// Deleting Firewall should fail due to DeleteProtection
	_, err = c.DeleteFirewall(ctx, &sdk.DeleteFirewallInput{
		FirewallArn: aws.String(fwArn),
	})
	if err == nil {
		t.Fatal("expected error deleting firewall with delete protection enabled")
	}

	// Update Delete Protection to false
	token := fwDesc.UpdateToken
	uDP, err := c.UpdateFirewallDeleteProtection(ctx, &sdk.UpdateFirewallDeleteProtectionInput{
		FirewallArn:      aws.String(fwArn),
		UpdateToken:      token,
		DeleteProtection: false,
	})
	if err != nil {
		t.Fatalf("UpdateFirewallDeleteProtection failed: %v", err)
	}

	token = uDP.UpdateToken

	// Update Description
	uDesc, err := c.UpdateFirewallDescription(ctx, &sdk.UpdateFirewallDescriptionInput{
		FirewallArn: aws.String(fwArn),
		UpdateToken: token,
		Description: aws.String("Updated description"),
	})
	if err != nil {
		t.Fatalf("UpdateFirewallDescription failed: %v", err)
	}

	_ = uDesc

	// 4. Logging Configuration
	logUpdate, err := c.UpdateLoggingConfiguration(ctx, &sdk.UpdateLoggingConfigurationInput{
		FirewallArn: aws.String(fwArn),
		LoggingConfiguration: &types.LoggingConfiguration{
			LogDestinationConfigs: []types.LogDestinationConfig{
				{
					LogType:            types.LogTypeAlert,
					LogDestinationType: types.LogDestinationTypeCloudwatchLogs,
					LogDestination: map[string]string{
						"logGroup": "/aws/networkfirewall/alerts",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateLoggingConfiguration failed: %v", err)
	}

	if len(logUpdate.LoggingConfiguration.LogDestinationConfigs) != 1 {
		t.Fatalf("unexpected configs length: %d", len(logUpdate.LoggingConfiguration.LogDestinationConfigs))
	}

	logDesc, err := c.DescribeLoggingConfiguration(ctx, &sdk.DescribeLoggingConfigurationInput{
		FirewallArn: aws.String(fwArn),
	})
	if err != nil {
		t.Fatalf("DescribeLoggingConfiguration failed: %v", err)
	}

	if len(logDesc.LoggingConfiguration.LogDestinationConfigs) != 1 {
		t.Fatalf("unexpected describe configs length: %d", len(logDesc.LoggingConfiguration.LogDestinationConfigs))
	}

	// 5. Resource Policy
	policyDoc := `{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Principal":"*","Action":"network-firewall:*","Resource":"*"}]}`
	_, err = c.PutResourcePolicy(ctx, &sdk.PutResourcePolicyInput{
		ResourceArn: aws.String(rgArn),
		Policy:      aws.String(policyDoc),
	})
	if err != nil {
		t.Fatalf("PutResourcePolicy failed: %v", err)
	}

	rpDesc, err := c.DescribeResourcePolicy(ctx, &sdk.DescribeResourcePolicyInput{
		ResourceArn: aws.String(rgArn),
	})
	if err != nil || *rpDesc.Policy != policyDoc {
		t.Fatalf("DescribeResourcePolicy failed: %v", err)
	}

	_, err = c.DeleteResourcePolicy(ctx, &sdk.DeleteResourcePolicyInput{
		ResourceArn: aws.String(rgArn),
	})
	if err != nil {
		t.Fatalf("DeleteResourcePolicy failed: %v", err)
	}

	// 6. Tags
	tagsList, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: aws.String(fwArn),
	})
	if err != nil || len(tagsList.Tags) == 0 {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: aws.String(fwArn),
		Tags: []types.Tag{
			{Key: aws.String("Project"), Value: aws.String("Alpha")},
		},
	})
	if err != nil {
		t.Fatalf("TagResource failed: %v", err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceArn: aws.String(fwArn),
		TagKeys:     []string{"Project"},
	})
	if err != nil {
		t.Fatalf("UntagResource failed: %v", err)
	}

	// 7. Cleanup
	_, err = c.DeleteFirewall(ctx, &sdk.DeleteFirewallInput{
		FirewallArn: aws.String(fwArn),
	})
	if err != nil {
		t.Fatalf("DeleteFirewall failed: %v", err)
	}

	_, err = c.DeleteFirewallPolicy(ctx, &sdk.DeleteFirewallPolicyInput{
		FirewallPolicyArn: aws.String(policyArn),
	})
	if err != nil {
		t.Fatalf("DeleteFirewallPolicy failed: %v", err)
	}

	_, err = c.DeleteRuleGroup(ctx, &sdk.DeleteRuleGroupInput{
		RuleGroupArn: aws.String(rgArn),
	})
	if err != nil {
		t.Fatalf("DeleteRuleGroup failed: %v", err)
	}
}

func TestNetworkFirewallValidationAndProtections(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := networkFirewallClient(s, "test", "us-east-1")
	net := ec2Client(s, "test", "us-east-1")

	// Create custom VPC and two subnets in the same AZ
	vpc, err := net.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.10.0.0/16")})
	if err != nil {
		t.Fatalf("CreateVpc failed: %v", err)
	}

	vpcID := *vpc.Vpc.VpcId

	sn1, err := net.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:            aws.String(vpcID),
		CidrBlock:        aws.String("10.10.1.0/24"),
		AvailabilityZone: aws.String("us-east-1a"),
	})
	if err != nil {
		t.Fatalf("CreateSubnet 1 failed: %v", err)
	}

	sn2, err := net.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:            aws.String(vpcID),
		CidrBlock:        aws.String("10.10.2.0/24"),
		AvailabilityZone: aws.String("us-east-1a"),
	})
	if err != nil {
		t.Fatalf("CreateSubnet 2 failed: %v", err)
	}

	sn3, err := net.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:            aws.String(vpcID),
		CidrBlock:        aws.String("10.10.3.0/24"),
		AvailabilityZone: aws.String("us-east-1b"),
	})
	if err != nil {
		t.Fatalf("CreateSubnet 3 failed: %v", err)
	}

	// 1. Create Policy
	pol1, err := c.CreateFirewallPolicy(ctx, &sdk.CreateFirewallPolicyInput{
		FirewallPolicyName: aws.String("pol-1"),
		FirewallPolicy: &types.FirewallPolicy{
			StatelessDefaultActions:         []string{"aws:pass"},
			StatelessFragmentDefaultActions: []string{"aws:pass"},
		},
	})
	if err != nil {
		t.Fatalf("CreateFirewallPolicy 1 failed: %v", err)
	}

	pol1Arn := *pol1.FirewallPolicyResponse.FirewallPolicyArn

	pol2, err := c.CreateFirewallPolicy(ctx, &sdk.CreateFirewallPolicyInput{
		FirewallPolicyName: aws.String("pol-2"),
		FirewallPolicy: &types.FirewallPolicy{
			StatelessDefaultActions:         []string{"aws:drop"},
			StatelessFragmentDefaultActions: []string{"aws:drop"},
		},
	})
	if err != nil {
		t.Fatalf("CreateFirewallPolicy 2 failed: %v", err)
	}

	pol2Arn := *pol2.FirewallPolicyResponse.FirewallPolicyArn

	// 2. Subnet Validation: non-existent subnet
	_, err = c.CreateFirewall(ctx, &sdk.CreateFirewallInput{
		FirewallName:      aws.String("fw-bad-sn"),
		FirewallPolicyArn: aws.String(pol1Arn),
		VpcId:             aws.String(vpcID),
		SubnetMappings:    []types.SubnetMapping{{SubnetId: aws.String("subnet-nonexistent")}},
	})
	if err == nil {
		t.Fatal("expected error with non-existent subnet")
	}

	// 3. AZ Collision: two subnets in same AZ
	_, err = c.CreateFirewall(ctx, &sdk.CreateFirewallInput{
		FirewallName:      aws.String("fw-az-collision"),
		FirewallPolicyArn: aws.String(pol1Arn),
		VpcId:             aws.String(vpcID),
		SubnetMappings: []types.SubnetMapping{
			{SubnetId: sn1.Subnet.SubnetId},
			{SubnetId: sn2.Subnet.SubnetId},
		},
	})
	if err == nil {
		t.Fatal("expected error with AZ collision in subnets")
	}

	// 4. Create Firewall with Protections
	fw, err := c.CreateFirewall(ctx, &sdk.CreateFirewallInput{
		FirewallName:                   aws.String("fw-protections"),
		FirewallPolicyArn:              aws.String(pol1Arn),
		VpcId:                          aws.String(vpcID),
		SubnetMappings:                 []types.SubnetMapping{{SubnetId: sn1.Subnet.SubnetId}},
		FirewallPolicyChangeProtection: true,
		SubnetChangeProtection:         true,
	})
	if err != nil {
		t.Fatalf("CreateFirewall failed: %v", err)
	}

	fwArn := *fw.Firewall.FirewallArn

	desc, err := c.DescribeFirewall(ctx, &sdk.DescribeFirewallInput{FirewallArn: aws.String(fwArn)})
	if err != nil {
		t.Fatalf("DescribeFirewall failed: %v", err)
	}

	token := desc.UpdateToken

	// 5. UpdateToken mismatch
	_, err = c.UpdateFirewallDescription(ctx, &sdk.UpdateFirewallDescriptionInput{
		FirewallArn: aws.String(fwArn),
		UpdateToken: aws.String("wrong-token"),
		Description: aws.String("nope"),
	})
	if err == nil {
		t.Fatal("expected error with mismatched update token")
	}

	// 6. Policy change protection test
	_, err = c.AssociateFirewallPolicy(ctx, &sdk.AssociateFirewallPolicyInput{
		FirewallArn:       aws.String(fwArn),
		UpdateToken:       token,
		FirewallPolicyArn: aws.String(pol2Arn),
	})
	if err == nil {
		t.Fatal("expected error associating policy while policy change protection is enabled")
	}

	// Disable policy change protection
	updProt, err := c.UpdateFirewallPolicyChangeProtection(ctx, &sdk.UpdateFirewallPolicyChangeProtectionInput{
		FirewallArn:                    aws.String(fwArn),
		UpdateToken:                    token,
		FirewallPolicyChangeProtection: false,
	})
	if err != nil {
		t.Fatalf("UpdateFirewallPolicyChangeProtection failed: %v", err)
	}

	token = updProt.UpdateToken

	// Now AssociateFirewallPolicy should succeed
	assocPol, err := c.AssociateFirewallPolicy(ctx, &sdk.AssociateFirewallPolicyInput{
		FirewallArn:       aws.String(fwArn),
		UpdateToken:       token,
		FirewallPolicyArn: aws.String(pol2Arn),
	})
	if err != nil {
		t.Fatalf("AssociateFirewallPolicy failed: %v", err)
	}

	token = assocPol.UpdateToken

	// 7. Subnet change protection test
	_, err = c.AssociateSubnets(ctx, &sdk.AssociateSubnetsInput{
		FirewallArn:    aws.String(fwArn),
		UpdateToken:    token,
		SubnetMappings: []types.SubnetMapping{{SubnetId: sn3.Subnet.SubnetId}},
	})
	if err == nil {
		t.Fatal("expected error associating subnets while subnet change protection is enabled")
	}

	// Disable subnet change protection
	updSubProt, err := c.UpdateSubnetChangeProtection(ctx, &sdk.UpdateSubnetChangeProtectionInput{
		FirewallArn:            aws.String(fwArn),
		UpdateToken:            token,
		SubnetChangeProtection: false,
	})
	if err != nil {
		t.Fatalf("UpdateSubnetChangeProtection failed: %v", err)
	}

	token = updSubProt.UpdateToken

	// Now AssociateSubnets should succeed
	assocSn, err := c.AssociateSubnets(ctx, &sdk.AssociateSubnetsInput{
		FirewallArn:    aws.String(fwArn),
		UpdateToken:    token,
		SubnetMappings: []types.SubnetMapping{{SubnetId: sn3.Subnet.SubnetId}},
	})
	if err != nil {
		t.Fatalf("AssociateSubnets failed: %v", err)
	}

	token = assocSn.UpdateToken

	// Disassociate subnet
	disassocSn, err := c.DisassociateSubnets(ctx, &sdk.DisassociateSubnetsInput{
		FirewallArn: aws.String(fwArn),
		UpdateToken: token,
		SubnetIds:   []string{*sn3.Subnet.SubnetId},
	})
	if err != nil {
		t.Fatalf("DisassociateSubnets failed: %v", err)
	}

	_ = disassocSn

	// Clean up
	_, err = c.DeleteFirewall(ctx, &sdk.DeleteFirewallInput{FirewallArn: aws.String(fwArn)})
	if err != nil {
		t.Fatalf("DeleteFirewall failed: %v", err)
	}

	// Test Resource Groups Tagging API
	rgClient := rgta.New(rgta.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  creds("test"),
		Retryer:      aws.NopRetryer{},
	})
	tagged, err := rgClient.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{pol1Arn},
		Tags:            map[string]string{"ManagedBy": "Terraform"},
	})
	if err != nil || len(tagged.FailedResourcesMap) > 0 {
		t.Fatalf("TagResources failed: %v %v", err, tagged)
	}

	tagsOut, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: aws.String(pol1Arn)})
	if err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}

	found := false
	for _, tag := range tagsOut.Tags {
		if *tag.Key == "ManagedBy" && *tag.Value == "Terraform" {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("tag ManagedBy not found on policy")
	}

	untagged, err := rgClient.UntagResources(ctx, &rgta.UntagResourcesInput{
		ResourceARNList: []string{pol1Arn},
		TagKeys:         []string{"ManagedBy"},
	})
	if err != nil || len(untagged.FailedResourcesMap) > 0 {
		t.Fatalf("UntagResources failed: %v %v", err, untagged)
	}

	// 8. Stateful Rule Group
	statefulRG, err := c.CreateRuleGroup(ctx, &sdk.CreateRuleGroupInput{
		RuleGroupName: aws.String("stateful-rg"),
		Type:          types.RuleGroupTypeStateful,
		Capacity:      aws.Int32(50),
		RuleGroup: &types.RuleGroup{
			RulesSource: &types.RulesSource{
				StatefulRules: []types.StatefulRule{
					{
						Action: types.StatefulActionPass,
						Header: &types.Header{
							Protocol:        types.StatefulRuleProtocolTcp,
							Source:          aws.String("ANY"),
							SourcePort:      aws.String("ANY"),
							Direction:       types.StatefulRuleDirectionForward,
							Destination:     aws.String("ANY"),
							DestinationPort: aws.String("80"),
						},
						RuleOptions: []types.RuleOption{
							{Keyword: aws.String("sid:1")},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateRuleGroup stateful failed: %v", err)
	}

	srgArn := *statefulRG.RuleGroupResponse.RuleGroupArn

	srgDesc, err := c.DescribeRuleGroup(ctx, &sdk.DescribeRuleGroupInput{RuleGroupArn: aws.String(srgArn)})
	if err != nil || srgDesc.RuleGroupResponse.Type != types.RuleGroupTypeStateful {
		t.Fatalf("DescribeRuleGroup stateful failed: %v", err)
	}

	_, err = c.DeleteRuleGroup(ctx, &sdk.DeleteRuleGroupInput{RuleGroupArn: aws.String(srgArn)})
	if err != nil {
		t.Fatalf("DeleteRuleGroup stateful failed: %v", err)
	}
}
