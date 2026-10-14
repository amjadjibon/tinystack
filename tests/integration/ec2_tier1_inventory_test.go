package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroups"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func TestEC2Tier1InventoryResourceGroupsAndTagger(t *testing.T) {
	s := server(t)
	c, tg, g := ec2Client(s, "test", "us-east-1"), taggingClient(s, "us-east-1"), groupsClient(s, "test", "us-east-1")
	ctx := context.Background()
	v1, v2 := createVPC(t, c, "10.0.0.0/16"), createVPC(t, c, "10.1.0.0/16")
	sn := createSubnet(t, c, v1, "10.0.0.0/24")
	if _, err := c.CreateDhcpOptions(
		ctx,
		&ec2.CreateDhcpOptionsInput{
			DhcpConfigurations: []types.NewDhcpConfiguration{
				{Key: new("domain-name-servers"), Values: []string{"AmazonProvidedDNS"}},
			},
			TagSpecifications: tier1Tags(types.ResourceTypeDhcpOptions, "inventory"),
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateEgressOnlyInternetGateway(
		ctx,
		&ec2.CreateEgressOnlyInternetGatewayInput{
			VpcId:             new(v1),
			TagSpecifications: tier1Tags(types.ResourceTypeEgressOnlyInternetGateway, "inventory"),
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateNetworkInterface(
		ctx,
		&ec2.CreateNetworkInterfaceInput{
			SubnetId:          new(sn),
			TagSpecifications: tier1Tags(types.ResourceTypeNetworkInterface, "inventory"),
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateFlowLogs(
		ctx,
		&ec2.CreateFlowLogsInput{
			ResourceIds:        []string{v1},
			ResourceType:       types.FlowLogsResourceTypeVpc,
			TrafficType:        types.TrafficTypeAll,
			LogDestinationType: types.LogDestinationTypeS3,
			LogDestination:     new("arn:aws:s3:::flows"),
			TagSpecifications:  tier1Tags(types.ResourceTypeVpcFlowLog, "inventory"),
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateManagedPrefixList(
		ctx,
		&ec2.CreateManagedPrefixListInput{
			PrefixListName:    new("inventory"),
			AddressFamily:     new("IPv4"),
			MaxEntries:        new(int32(1)),
			TagSpecifications: tier1Tags(types.ResourceTypePrefixList, "inventory"),
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateVpcPeeringConnection(
		ctx,
		&ec2.CreateVpcPeeringConnectionInput{
			VpcId:             new(v1),
			PeerVpcId:         new(v2),
			TagSpecifications: tier1Tags(types.ResourceTypeVpcPeeringConnection, "inventory"),
		},
	); err != nil {
		t.Fatal(err)
	}
	all, err := tg.GetResources(ctx, &resourcegroupstaggingapi.GetResourcesInput{ResourceTypeFilters: []string{
		"ec2:dhcp-options",
		"ec2:egress-only-internet-gateway",
		"ec2:network-interface",
		"ec2:vpc-flow-log",
		"ec2:prefix-list",
		"ec2:vpc-peering-connection",
	}})
	if err != nil || len(all.ResourceTagMappingList) != 6 {
		t.Fatalf("%+v %v", all, err)
	}
	var arns []string
	for _, r := range all.ResourceTagMappingList {
		arns = append(arns, *r.ResourceARN)
	}
	retagged, err := tg.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{ResourceARNList: arns, Tags: map[string]string{"owner": "tier1"}},
	)
	if err != nil || len(retagged.FailedResourcesMap) != 0 {
		t.Fatalf("%+v %v", retagged, err)
	}
	query := `{"ResourceTypeFilters":["AWS::EC2::DHCPOptions","AWS::EC2::EgressOnlyInternetGateway",` +
		`"AWS::EC2::NetworkInterface","AWS::EC2::FlowLog","AWS::EC2::PrefixList","AWS::EC2::VPCPeeringConnection"],` +
		`"TagFilters":[{"Key":"owner","Values":["tier1"]}]}`
	if _, err = g.CreateGroup(
		ctx,
		&resourcegroups.CreateGroupInput{Name: new("tier1"), ResourceQuery: tagQuery(query)},
	); err != nil {
		t.Fatal(err)
	}
	resources, err := g.ListGroupResources(ctx, &resourcegroups.ListGroupResourcesInput{Group: new("tier1")})
	if err != nil || len(resources.Resources) != 6 {
		t.Fatalf("%+v %v", resources, err)
	}
	removed, err := tg.UntagResources(
		ctx,
		&resourcegroupstaggingapi.UntagResourcesInput{ResourceARNList: arns, TagKeys: []string{"owner"}},
	)
	if err != nil || len(removed.FailedResourcesMap) != 0 {
		t.Fatalf("%+v %v", removed, err)
	}
	resources, err = g.ListGroupResources(ctx, &resourcegroups.ListGroupResourcesInput{Group: new("tier1")})
	if err != nil || len(resources.Resources) != 0 {
		t.Fatalf("%+v %v", resources, err)
	}
}
