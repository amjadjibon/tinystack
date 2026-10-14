package integration_test

import (
	"context"
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestCloudFormationEC2Tier1NetworkingLifecycleAndExplicitUpdateFailures(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c, cf := ec2Client(s, "test", "us-east-1"), cfnClient(s)
	v, err := c.CreateVpc(
		ctx,
		&ec2.CreateVpcInput{CidrBlock: new("10.0.0.0/16"), AmazonProvidedIpv6CidrBlock: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	vpcID := *v.Vpc.VpcId
	peerID := createVPC(t, c, "10.1.0.0/16")
	subnetID := createSubnet(t, c, vpcID, "10.0.0.0/24")
	images, err := c.DescribeImages(
		ctx,
		&ec2.DescribeImagesInput{
			Owners:  []string{"amazon"},
			Filters: []ec2types.Filter{{Name: new("architecture"), Values: []string{"x86_64"}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	instance, err := c.RunInstances(
		ctx,
		&ec2.RunInstancesInput{
			ImageId:      images.Images[0].ImageId,
			InstanceType: ec2types.InstanceTypeT3Micro,
			SubnetId:     new(subnetID),
			MinCount:     new(int32(1)),
			MaxCount:     new(int32(1)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	block := netip.PrefixFrom(netip.MustParsePrefix(*v.Vpc.Ipv6CidrBlockAssociationSet[0].Ipv6CidrBlock).Addr(), 64).
		String()
	ref := func(logical string) map[string]any { return map[string]any{"Ref": logical} }
	properties := map[string]map[string]any{
		"DHCP":            {"DomainName": "tiny.internal", "DomainNameServers": []any{"AmazonProvidedDNS"}},
		"DHCPAssociation": {"VpcId": vpcID, "DhcpOptionsId": ref("DHCP")},
		"Egress":          {"VpcId": vpcID},
		"IPv4":            {"VpcId": vpcID, "CidrBlock": "10.2.0.0/16"},
		"IPv6":            {"SubnetId": subnetID, "Ipv6CidrBlock": block},
		"Interface": {
			"SubnetId":         subnetID,
			"Description":      "before",
			"PrivateIpAddress": "10.0.0.10",
			"SourceDestCheck":  false,
		},
		"Attachment": {
			"NetworkInterfaceId":  ref("Interface"),
			"InstanceId":          *instance.Instances[0].InstanceId,
			"DeviceIndex":         1,
			"DeleteOnTermination": false,
		},
		"Flow": {
			"ResourceId":         vpcID,
			"ResourceType":       "VPC",
			"TrafficType":        "ALL",
			"LogDestinationType": "s3",
			"LogDestination":     "arn:aws:s3:::flows",
		},
		"Prefix": {
			"PrefixListName": "cf-services",
			"AddressFamily":  "IPv4",
			"MaxEntries":     3,
			"Entries":        []any{map[string]any{"Cidr": "10.3.0.0/24", "Description": "before"}},
		},
		"Peering": {"VpcId": vpcID, "PeerVpcId": peerID},
	}

	resourceTypes := map[string]string{
		"DHCP":            "DHCPOptions",
		"DHCPAssociation": "VPCDHCPOptionsAssociation",
		"Egress":          "EgressOnlyInternetGateway",
		"IPv4":            "VPCCidrBlock",
		"IPv6":            "SubnetCidrBlock",
		"Interface":       "NetworkInterface",
		"Attachment":      "NetworkInterfaceAttachment",
		"Flow":            "FlowLog",
		"Prefix":          "PrefixList",
		"Peering":         "VPCPeeringConnection",
	}

	template := func() string {
		resources := map[string]any{}
		outputs := map[string]any{}
		for logical, p := range properties {
			resources[logical] = map[string]any{"Type": "AWS::EC2::" + resourceTypes[logical], "Properties": p}
			outputs[logical] = map[string]any{"Value": ref(logical)}
		}

		outputs["PrefixArn"] = map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Prefix", "Arn"}}}
		outputs["PrivateIP"] = map[string]any{
			"Value": map[string]any{"Fn::GetAtt": []any{"Interface", "PrimaryPrivateIpAddress"}},
		}

		body, marshalErr := json.Marshal(map[string]any{"Resources": resources, "Outputs": outputs})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}

		return string(body)
	}

	if _, err = cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("ec2-tier1"), TemplateBody: new(template())},
	); err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, cf, "ec2-tier1", cfntypes.StackStatusCreateComplete)
	out := outputsOf(&st)
	if !strings.HasPrefix(out["DHCP"], "dopt-") || !strings.HasPrefix(out["Egress"], "eigw-") ||
		!strings.HasPrefix(out["Interface"], "eni-") || !strings.HasPrefix(out["Peering"], "pcx-") ||
		out["PrivateIP"] != "10.0.0.10" || !strings.Contains(out["PrefixArn"], ":prefix-list/") {
		t.Fatal(out)
	}

	for _, logical := range []string{"DHCP", "Egress", "Interface", "Flow", "Prefix", "Peering"} {
		properties[logical]["Tags"] = []any{map[string]any{"Key": "team", "Value": "updated"}}
	}

	properties["Interface"]["Description"] = "after"
	properties["Interface"]["SourceDestCheck"] = true
	properties["Attachment"]["DeleteOnTermination"] = true
	properties["Prefix"]["PrefixListName"] = "cf-updated"
	properties["Prefix"]["Entries"] = []any{
		map[string]any{"Cidr": "10.3.0.0/24", "Description": "after"},
		map[string]any{"Cidr": "10.4.0.0/24"},
	}

	if _, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("ec2-tier1"), TemplateBody: new(template())},
	); err != nil {
		t.Fatal(err)
	}

	updated := wantStatus(ctx, t, cf, "ec2-tier1", cfntypes.StackStatusUpdateComplete)
	ids := outputsOf(&updated)
	if out["Interface"] != ids["Interface"] || out["Prefix"] != ids["Prefix"] || out["DHCP"] != ids["DHCP"] {
		t.Fatal("Unexpected replacement", ids)
	}

	ni, err := c.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{out["Interface"]}},
	)
	if err != nil || *ni.NetworkInterfaces[0].Description != "after" || !*ni.NetworkInterfaces[0].SourceDestCheck ||
		!*ni.NetworkInterfaces[0].Attachment.DeleteOnTermination {
		t.Fatalf("%+v %v", ni, err)
	}

	entries, err := c.GetManagedPrefixListEntries(
		ctx,
		&ec2.GetManagedPrefixListEntriesInput{PrefixListId: new(out["Prefix"])},
	)
	if err != nil || len(entries.Entries) != 2 || *entries.Entries[0].Description != "after" {
		t.Fatalf("%+v %v", entries, err)
	}

	// These properties are declared at creation, but their unsupported in-place changes must fail explicitly.
	for _, candidate := range []struct {
		value        any
		logical, key string
	}{
		{1, "Interface", "SecondaryPrivateIpAddressCount"},
		{1, "Interface", "Ipv6AddressCount"},
		{[]any{map[string]any{"PrivateIpAddress": "10.0.0.12", "Primary": false}}, "Interface", "PrivateIpAddresses"},
		{[]any{map[string]any{"Ipv6Address": "2001:db8::1"}}, "Interface", "Ipv6Addresses"},
		{"IPv6", "Prefix", "AddressFamily"},
	} {
		t.Run(candidate.logical+"/"+candidate.key, func(t *testing.T) {
			prior, present := properties[candidate.logical][candidate.key]
			properties[candidate.logical][candidate.key] = candidate.value
			_, updateErr := cf.UpdateStack(
				ctx,
				&cfn.UpdateStackInput{StackName: new("ec2-tier1"), TemplateBody: new(template())},
			)
			if updateErr != nil {
				t.Fatal(updateErr)
			}

			wantStatus(ctx, t, cf, "ec2-tier1", cfntypes.StackStatusUpdateRollbackComplete)
			events, eventErr := cf.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: new("ec2-tier1")})
			if eventErr != nil {
				t.Fatal(eventErr)
			}

			explicit := false
			for _, event := range events.StackEvents {
				if event.ResourceStatusReason != nil &&
					strings.Contains(*event.ResourceStatusReason, "does not support updating") &&
					strings.Contains(*event.ResourceStatusReason, candidate.key) {
					explicit = true
				}
			}

			if !explicit {
				t.Fatal("Update did not report its unsupported property", events)
			}

			after, readErr := c.DescribeNetworkInterfaces(
				ctx,
				&ec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{out["Interface"]}},
			)
			if readErr != nil || !reflect.DeepEqual(ni.NetworkInterfaces, after.NetworkInterfaces) {
				t.Fatalf("Rejected stack update changed ENI: %+v %v", after, readErr)
			}

			if present {
				properties[candidate.logical][candidate.key] = prior
			} else {
				delete(properties[candidate.logical], candidate.key)
			}
		})
	}

	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("ec2-tier1")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "tier1 stack deletion", func() bool {
		stacks, readErr := cf.ListStacks(
			ctx,
			&cfn.ListStacksInput{StackStatusFilter: []cfntypes.StackStatus{cfntypes.StackStatusDeleteComplete}},
		)
		if readErr != nil {
			t.Fatal(readErr)
		}

		for _, stack := range stacks.StackSummaries {
			if *stack.StackName == "ec2-tier1" {
				return true
			}
		}

		return false
	})
	_, err = c.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{out["Interface"]}},
	)
	code(t, err, "InvalidNetworkInterfaceID.NotFound")
	if _, err = c.TerminateInstances(
		ctx,
		&ec2.TerminateInstancesInput{InstanceIds: []string{*instance.Instances[0].InstanceId}},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: new(subnetID)}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: new(vpcID)}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: new(peerID)}); err != nil {
		t.Fatal(err)
	}
}
