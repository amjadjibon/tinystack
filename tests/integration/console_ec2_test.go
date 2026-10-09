package integration_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/amjadjibon/tinystack/internal/gateway"
)

func TestConsoleEC2AndVPCLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "ec2", operation, account, region, input), http.StatusOK)["output"].(map[string]any)
	}
	fail := func(operation string, input map[string]any) {
		t.Helper()
		consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "ec2", operation, account, region, input), http.StatusBadRequest)
	}
	id := func(out map[string]any, nested, key string) string {
		t.Helper()
		if nested != "" {
			out = out[nested].(map[string]any)
		}
		return out[key].(string)
	}
	list := func(out map[string]any, key string) []any {
		t.Helper()
		return out[key].([]any)
	}
	tags := func(kind string) []any {
		return []any{
			map[string]any{"ResourceType": kind, "Tags": []any{map[string]any{"Key": "Name", "Value": "console"}}},
		}
	}
	vpc := id(call("CreateVpc", map[string]any{
		"CidrBlock": "10.80.0.0/16", "TagSpecifications": tags("vpc"),
	}), "Vpc", "VpcId")
	call("ModifyVpcAttribute", map[string]any{"VpcId": vpc, "EnableDnsHostnames": map[string]any{"Value": true}})
	dns := call("DescribeVpcAttribute", map[string]any{"VpcId": vpc, "Attribute": "enableDnsHostnames"})
	if dns["EnableDnsHostnames"].(map[string]any)["Value"] != true {
		t.Fatal("DNS settings were not saved")
	}
	subnet := id(call("CreateSubnet", map[string]any{
		"VpcId": vpc, "CidrBlock": "10.80.1.0/24", "TagSpecifications": tags("subnet"),
	}), "Subnet", "SubnetId")
	call("ModifySubnetAttribute", map[string]any{
		"SubnetId": subnet, "MapPublicIpOnLaunch": map[string]any{"Value": true},
	})
	group := id(call("CreateSecurityGroup", map[string]any{
		"VpcId":             vpc,
		"GroupName":         "console-group",
		"Description":       "console test",
		"TagSpecifications": tags("security-group"),
	}), "", "GroupId")
	permission := []any{map[string]any{
		"IpProtocol": "tcp", "FromPort": 443, "ToPort": 443,
		"IpRanges": []any{map[string]any{"CidrIp": "10.80.0.0/16", "Description": "internal HTTPS"}},
	}}
	call("AuthorizeSecurityGroupIngress", map[string]any{"GroupId": group, "IpPermissions": permission})
	call("AuthorizeSecurityGroupEgress", map[string]any{"GroupId": group, "IpPermissions": permission})
	rules := list(call("DescribeSecurityGroupRules", map[string]any{
		"Filters": []any{map[string]any{"Name": "group-id", "Values": []string{group}}}, "MaxResults": 100,
	}), "SecurityGroupRules")
	if len(rules) != 3 {
		t.Fatal("missing inbound, outbound or default rule")
	}
	for _, value := range rules {
		rule := value.(map[string]any)
		if rule["IpProtocol"] == "tcp" {
			op := "RevokeSecurityGroupIngress"
			if rule["IsEgress"] == true {
				op = "RevokeSecurityGroupEgress"
			}
			call(
				op,
				map[string]any{
					"GroupId":              group,
					"SecurityGroupRuleIds": []string{rule["SecurityGroupRuleId"].(string)},
				},
			)
		}
	}
	key := call("CreateKeyPair", map[string]any{
		"KeyName": "console-key", "KeyType": "ed25519", "KeyFormat": "pem", "TagSpecifications": tags("key-pair"),
	})
	material := key["KeyMaterial"].(string)
	if !strings.Contains(material, "PRIVATE KEY") {
		t.Fatal("missing one-time private key")
	}
	keyID := id(key, "", "KeyPairId")
	public := list(call("DescribeKeyPairs", map[string]any{
		"KeyPairIds": []string{keyID}, "IncludePublicKey": true,
	}), "KeyPairs")[0].(map[string]any)
	if public["KeyMaterial"] != nil || public["PublicKey"] == nil {
		t.Fatal("described key did not contain only public material")
	}
	imported := id(call("ImportKeyPair", map[string]any{
		"KeyName":           "console-import",
		"PublicKeyMaterial": base64.StdEncoding.EncodeToString([]byte(public["PublicKey"].(string))),
	}), "", "KeyPairId")
	images := list(call("DescribeImages", map[string]any{"MaxResults": 100}), "Images")
	image := ""
	for _, value := range images {
		row := value.(map[string]any)
		if row["Architecture"] == "x86_64" {
			image = row["ImageId"].(string)
			break
		}
	}
	if image == "" {
		t.Fatal("missing x86 image")
	}
	launch := map[string]any{
		"ImageId": image, "InstanceType": "t3.micro", "MinCount": 1, "MaxCount": 1, "SubnetId": subnet,
		"SecurityGroupIds": []string{group}, "KeyName": "console-key", "ClientToken": "console-launch",
		"UserData": base64.StdEncoding.EncodeToString([]byte("echo stored")), "TagSpecifications": tags("instance"),
	}
	instance := list(call("RunInstances", launch), "Instances")[0].(map[string]any)["InstanceId"].(string)
	retried := list(call("RunInstances", launch), "Instances")[0].(map[string]any)["InstanceId"]
	if retried != instance {
		t.Fatal("launch retry created another instance")
	}
	call("StopInstances", map[string]any{"InstanceIds": []string{instance}})
	client := ec2Client(srv, "test", region)
	got, err := client.DescribeInstances(
		context.Background(),
		&ec2.DescribeInstancesInput{InstanceIds: []string{instance}},
	)
	if err != nil || len(got.Reservations) != 1 || string(got.Reservations[0].Instances[0].State.Name) != "stopped" {
		t.Fatalf("SDK instance after stop = %+v, %v", got, err)
	}
	call("StartInstances", map[string]any{"InstanceIds": []string{instance}})
	call("RebootInstances", map[string]any{"InstanceIds": []string{instance}})
	if len(list(call("DescribeInstanceStatus", map[string]any{
		"InstanceIds": []string{instance}, "IncludeAllInstances": true,
	}), "InstanceStatuses")) != 1 {
		t.Fatal("missing instance status")
	}
	consoleWrite(t, srv, "CreateInstanceProfile", map[string]any{"InstanceProfileName": "console-ec2-profile"})
	profile := map[string]any{"Name": "console-ec2-profile"}
	association := id(call("AssociateIamInstanceProfile", map[string]any{
		"InstanceId": instance, "IamInstanceProfile": profile,
	}), "IamInstanceProfileAssociation", "AssociationId")
	association = id(call("ReplaceIamInstanceProfileAssociation", map[string]any{
		"AssociationId": association, "IamInstanceProfile": profile,
	}), "IamInstanceProfileAssociation", "AssociationId")
	if len(list(call("DescribeIamInstanceProfileAssociations", map[string]any{
		"Filters": []any{map[string]any{"Name": "instance-id", "Values": []string{instance}}},
	}), "IamInstanceProfileAssociations")) != 1 {
		t.Fatal("missing profile association")
	}
	call("DisassociateIamInstanceProfile", map[string]any{"AssociationId": association})
	consoleWrite(t, srv, "DeleteInstanceProfile", map[string]any{"InstanceProfileName": "console-ec2-profile"})
	call(
		"CreateTags",
		map[string]any{"Resources": []string{instance}, "Tags": []any{map[string]any{"Key": "owner", "Value": "ui"}}},
	)
	call("DeleteTags", map[string]any{"Resources": []string{instance}, "Tags": []any{map[string]any{"Key": "Name"}}})
	igw := id(call("CreateInternetGateway", map[string]any{"TagSpecifications": tags("internet-gateway")}),
		"InternetGateway", "InternetGatewayId")
	call("AttachInternetGateway", map[string]any{"InternetGatewayId": igw, "VpcId": vpc})
	route := id(
		call("CreateRouteTable", map[string]any{"VpcId": vpc, "ClientToken": "console-route"}),
		"RouteTable",
		"RouteTableId",
	)
	call("CreateRoute", map[string]any{"RouteTableId": route, "DestinationCidrBlock": "0.0.0.0/0", "GatewayId": igw})
	routeAssoc := id(
		call("AssociateRouteTable", map[string]any{"RouteTableId": route, "SubnetId": subnet}),
		"",
		"AssociationId",
	)
	routeAssoc = id(call("ReplaceRouteTableAssociation", map[string]any{
		"RouteTableId": route, "AssociationId": routeAssoc,
	}), "", "NewAssociationId")
	address := id(call("AllocateAddress", map[string]any{"Domain": "vpc"}), "", "AllocationId")
	addrAssoc := id(call("AssociateAddress", map[string]any{
		"AllocationId": address, "InstanceId": instance, "AllowReassociation": false,
	}), "", "AssociationId")
	call("DisassociateAddress", map[string]any{"AssociationId": addrAssoc})
	nat := id(call("CreateNatGateway", map[string]any{
		"SubnetId": subnet, "AllocationId": address, "ConnectivityType": "public", "ClientToken": "console-nat",
	}), "NatGateway", "NatGatewayId")
	call(
		"ReplaceRoute",
		map[string]any{"RouteTableId": route, "DestinationCidrBlock": "0.0.0.0/0", "NatGatewayId": nat},
	)
	endpoint := id(call("CreateVpcEndpoint", map[string]any{
		"VpcId": vpc, "VpcEndpointType": "Gateway", "ServiceName": "com.amazonaws." + region + ".s3",
		"RouteTableIds": []string{route}, "ClientToken": "console-endpoint",
	}), "VpcEndpoint", "VpcEndpointId")
	call("ModifyVpcEndpoint", map[string]any{
		"VpcEndpointId": endpoint, "RemoveRouteTableIds": []string{route}, "ResetPolicy": true,
	})
	iface := id(call("CreateVpcEndpoint", map[string]any{
		"VpcId": vpc, "VpcEndpointType": "Interface", "ServiceName": "com.amazonaws." + region + ".sqs",
		"SubnetIds": []string{subnet}, "SecurityGroupIds": []string{group}, "PrivateDnsEnabled": true,
	}), "VpcEndpoint", "VpcEndpointId")
	call("ModifyVpcEndpoint", map[string]any{"VpcEndpointId": iface, "PrivateDnsEnabled": false})
	if len(list(call("DescribeNetworkInterfaces", map[string]any{"MaxResults": 100}), "NetworkInterfaces")) != 2 {
		t.Fatal("missing derived network interfaces")
	}
	acl := id(call("CreateNetworkAcl", map[string]any{"VpcId": vpc}), "NetworkAcl", "NetworkAclId")
	entry := map[string]any{
		"NetworkAclId": acl, "RuleNumber": 100, "Egress": false, "Protocol": "6", "RuleAction": "allow",
		"CidrBlock": "10.80.0.0/16", "PortRange": map[string]any{"From": 443, "To": 443},
	}
	call("CreateNetworkAclEntry", entry)
	entry["RuleAction"] = "deny"
	call("ReplaceNetworkAclEntry", entry)
	acls := list(call("DescribeNetworkAcls", map[string]any{
		"Filters": []any{map[string]any{"Name": "vpc-id", "Values": []string{vpc}}},
	}), "NetworkAcls")
	defaultACL, aclAssoc := "", ""
	for _, value := range acls {
		r := value.(map[string]any)
		if r["IsDefault"] == true {
			defaultACL = r["NetworkAclId"].(string)
			aclAssoc = r["Associations"].([]any)[0].(map[string]any)["NetworkAclAssociationId"].(string)
		}
	}
	aclAssoc = id(call("ReplaceNetworkAclAssociation", map[string]any{
		"AssociationId": aclAssoc, "NetworkAclId": acl,
	}), "", "NewAssociationId")
	fail("DeleteVpc", map[string]any{"VpcId": vpc})
	fail("DeleteSubnet", map[string]any{"SubnetId": subnet})
	fail("DeleteSecurityGroup", map[string]any{"GroupId": group})
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		for _, tc := range []struct {
			input map[string]any
			op    string
		}{
			{map[string]any{"VpcIds": []string{vpc}}, "DescribeVpcs"},
			{map[string]any{"InstanceIds": []string{instance}}, "DescribeInstances"},
			{map[string]any{"InstanceIds": []string{instance}}, "StopInstances"},
			{map[string]any{"SubnetId": subnet}, "DeleteSubnet"},
			{map[string]any{"GroupId": group, "IpPermissions": permission}, "AuthorizeSecurityGroupIngress"},
			{map[string]any{
				"Resources": []string{vpc}, "Tags": []any{map[string]any{"Key": "wrong", "Value": "scope"}},
			}, "CreateTags"},
		} {
			consoleResponse(t, srv, consoleLambdaRequest(t, srv, "ec2", tc.op, scope.account, scope.region, tc.input),
				http.StatusBadRequest)
		}
		request := consoleLambdaRequest(t, srv, "ec2", "DeleteVpcEndpoints", scope.account, scope.region,
			map[string]any{"VpcEndpointIds": []string{endpoint}})
		failed := consoleResponse(t, srv, request, http.StatusOK)["output"].(map[string]any)
		if len(failed["Unsuccessful"].([]any)) != 1 {
			t.Fatal("cross-scope endpoint deletion did not fail")
		}
	}
	// Pagination preserves the console's resource list rather than silently dropping later resources.
	for i := range 3 {
		call("CreateSubnet", map[string]any{"VpcId": vpc, "CidrBlock": []string{
			"10.80.2.0/24", "10.80.3.0/24", "10.80.4.0/24",
		}[i]})
	}
	page := call("DescribeSubnets", map[string]any{"MaxResults": 5})
	if len(list(page, "Subnets")) != 5 || page["NextToken"] == nil {
		t.Fatal("missing subnet pagination")
	}
	if len(
		list(call("DescribeSubnets", map[string]any{"MaxResults": 5, "NextToken": page["NextToken"]}), "Subnets"),
	) != 2 {
		t.Fatal("missing second subnet page")
	}
	call("DeleteVpcEndpoints", map[string]any{"VpcEndpointIds": []string{endpoint, iface}})
	call("TerminateInstances", map[string]any{"InstanceIds": []string{instance}})
	call("DeleteNatGateway", map[string]any{"NatGatewayId": nat})
	call("ReleaseAddress", map[string]any{"AllocationId": address})
	call("DeleteRoute", map[string]any{"RouteTableId": route, "DestinationCidrBlock": "0.0.0.0/0"})
	call("DisassociateRouteTable", map[string]any{"AssociationId": routeAssoc})
	call("DeleteRouteTable", map[string]any{"RouteTableId": route})
	call("ReplaceNetworkAclAssociation", map[string]any{"AssociationId": aclAssoc, "NetworkAclId": defaultACL})
	call("DeleteNetworkAclEntry", map[string]any{"NetworkAclId": acl, "RuleNumber": 100, "Egress": false})
	call("DeleteNetworkAcl", map[string]any{"NetworkAclId": acl})
	call("DeleteSecurityGroup", map[string]any{"GroupId": group})
	call("DetachInternetGateway", map[string]any{"InternetGatewayId": igw, "VpcId": vpc})
	call("DeleteInternetGateway", map[string]any{"InternetGatewayId": igw})
	subnets, err := client.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range subnets.Subnets {
		if aws.ToString(row.VpcId) == vpc {
			call("DeleteSubnet", map[string]any{"SubnetId": aws.ToString(row.SubnetId)})
		}
	}
	call("DeleteVpc", map[string]any{"VpcId": vpc})
	call("DeleteKeyPair", map[string]any{"KeyPairId": keyID})
	call("DeleteKeyPair", map[string]any{"KeyPairId": imported})
	var journal struct {
		Requests []gateway.Record `json:"requests"`
	}
	if internal(t, srv, http.MethodGet, "/_tinystack/requests", &journal) != http.StatusOK {
		t.Fatal("cannot read request inspector")
	}
	found := false
	for _, record := range journal.Requests {
		if strings.Contains(record.ResponseBody, material) {
			t.Fatal("private key leaked to inspector")
		}
		if record.Operation == "CreateKeyPair" {
			found = record.Protocol == "console" && strings.Contains(record.ResponseBody, "redacted")
		}
	}
	if !found {
		t.Fatal("missing redacted key creation in inspector")
	}
}
