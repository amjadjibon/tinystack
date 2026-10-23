package integration_test

import (
	"context"
	"fmt"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

func networkFirewallTemplate(vpcID, subnetID string) string {
	return fmt.Sprintf(`{
 "Resources":{
   "RuleGroup":{
     "Type":"AWS::NetworkFirewall::RuleGroup",
     "Properties":{
       "RuleGroupName":"cfn-test-rg",
       "Type":"STATELESS",
       "Capacity":100,
       "RuleGroup":{
         "RulesSource":{
           "StatelessRulesAndCustomActions":{
             "StatelessRules":[]
           }
         }
       }
     }
   },
   "FirewallPolicy":{
     "Type":"AWS::NetworkFirewall::FirewallPolicy",
     "Properties":{
       "FirewallPolicyName":"cfn-test-policy",
       "FirewallPolicy":{
         "StatelessDefaultActions":["aws:pass"],
         "StatelessFragmentDefaultActions":["aws:pass"],
         "StatelessRuleGroupReferences":[
           {
             "Priority":1,
             "ResourceArn":{"Fn::GetAtt":["RuleGroup","RuleGroupArn"]}
           }
         ]
       }
     }
   },
   "Firewall":{
     "Type":"AWS::NetworkFirewall::Firewall",
     "Properties":{
       "FirewallName":"cfn-test-fw",
       "FirewallPolicyArn":{"Fn::GetAtt":["FirewallPolicy","FirewallPolicyArn"]},
       "VpcId":%q,
       "SubnetMappings":[{"SubnetId":%q}]
     }
   },
   "LoggingConfiguration":{
     "Type":"AWS::NetworkFirewall::LoggingConfiguration",
     "Properties":{
       "FirewallArn":{"Fn::GetAtt":["Firewall","FirewallArn"]},
       "LoggingConfiguration":{
         "LogDestinationConfigs":[
           {
             "LogType":"ALERT",
             "LogDestinationType":"CloudWatchLogs",
             "LogDestination":{"logGroup":"/aws/networkfirewall/cfn-alerts"}
           }
         ]
       }
     }
   }
 },
 "Outputs":{
   "RuleGroupArn":{"Value":{"Fn::GetAtt":["RuleGroup","RuleGroupArn"]}},
   "RuleGroupId":{"Value":{"Fn::GetAtt":["RuleGroup","RuleGroupId"]}},
   "FirewallPolicyArn":{"Value":{"Fn::GetAtt":["FirewallPolicy","FirewallPolicyArn"]}},
   "FirewallPolicyId":{"Value":{"Fn::GetAtt":["FirewallPolicy","FirewallPolicyId"]}},
   "FirewallArn":{"Value":{"Fn::GetAtt":["Firewall","FirewallArn"]}},
   "FirewallId":{"Value":{"Fn::GetAtt":["Firewall","FirewallId"]}},
   "EndpointIds":{"Value":{"Fn::Join":[",",{"Fn::GetAtt":["Firewall","EndpointIds"]}]}}
 }
}`, vpcID, subnetID)
}

func TestCloudFormationNetworkFirewall(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	net := ec2Client(s, "test", "us-east-1")

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

	_, err = cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    new("cfn-nfw-stack"),
		TemplateBody: new(networkFirewallTemplate(vpcID, subnet1)),
	})
	if err != nil {
		t.Fatalf("CreateStack failed: %v", err)
	}

	stack := wantStatus(ctx, t, cf, "cfn-nfw-stack", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&stack)

	if outputs["RuleGroupArn"] == "" || outputs["RuleGroupId"] == "" {
		t.Errorf("missing RuleGroup attributes: %v", outputs)
	}

	if outputs["FirewallPolicyArn"] == "" || outputs["FirewallPolicyId"] == "" {
		t.Errorf("missing FirewallPolicy attributes: %v", outputs)
	}

	if outputs["FirewallArn"] == "" || outputs["FirewallId"] == "" {
		t.Errorf("missing Firewall attributes: %v", outputs)
	}

	if outputs["EndpointIds"] == "" {
		t.Errorf("missing EndpointIds attribute: %v", outputs)
	}

	// Delete stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: new("cfn-nfw-stack"),
	})
	if err != nil {
		t.Fatalf("DeleteStack failed: %v", err)
	}

	wantStatus(ctx, t, cf, *stack.StackId, cfntypes.StackStatusDeleteComplete)
}
