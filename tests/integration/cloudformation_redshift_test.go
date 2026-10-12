package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/redshift"
	"github.com/aws/aws-sdk-go-v2/service/redshift/types"
)

func redshiftTemplate(subnet string, port int, tag string) string {
	tags := []any{map[string]any{"Key": "version", "Value": tag}}
	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"Subnets": map[string]any{"Type": "AWS::Redshift::ClusterSubnetGroup", "Properties": map[string]any{
				"Description": tag, "SubnetIds": []string{subnet}, "Tags": tags,
			}},
			"Parameters": map[string]any{"Type": "AWS::Redshift::ClusterParameterGroup", "Properties": map[string]any{
				"ParameterGroupName":   "cfn-params",
				"ParameterGroupFamily": "redshift-1.0",
				"Description":          "parameters",
				"Parameters":           []any{},
				"Tags":                 tags,
			}},
			"Warehouse": map[string]any{"Type": "AWS::Redshift::Cluster", "Properties": map[string]any{
				"ClusterIdentifier": "cfn-warehouse", "ClusterType": "single-node", "NodeType": "dc2.large",
				"MasterUsername": "admin", "MasterUserPassword": "Password123!", "Port": port,
				"ClusterSubnetGroupName":    map[string]any{"Ref": "Subnets"},
				"ClusterParameterGroupName": map[string]any{"Ref": "Parameters"}, "Tags": tags,
			}},
		},
		"Outputs": map[string]any{
			"Identifier": map[string]any{"Value": map[string]any{"Ref": "Warehouse"}},
			"Address": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"Warehouse", "Endpoint.Address"}},
			},
			"Port":          map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Warehouse", "Endpoint.Port"}}},
			"SubnetName":    map[string]any{"Value": map[string]any{"Ref": "Subnets"}},
			"ParameterName": map[string]any{"Value": map[string]any{"Ref": "Parameters"}},
		},
	})
	return string(raw)
}

func TestCloudFormationRedshiftLifecycle(t *testing.T) {
	server := server(t)
	ctx := context.Background()
	cf := cfnClient(server)
	c := sdk.New(sdk.Options{Region: "us-east-1", BaseEndpoint: new(server.URL), Credentials: creds("test")})
	network := ec2sdk.New(
		ec2sdk.Options{Region: "us-east-1", BaseEndpoint: new(server.URL), Credentials: creds("test")},
	)
	vpc, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(
		ctx,
		&ec2sdk.CreateSubnetInput{VpcId: vpc.Vpc.VpcId, CidrBlock: new("10.0.1.0/24")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("redshift-stack"),
			TemplateBody: new(redshiftTemplate(*subnet.Subnet.SubnetId, 5439, "one")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, cf, "redshift-stack", cfntypes.StackStatusCreateComplete)
	out := outputsOf(&st)
	if out["Identifier"] != "cfn-warehouse" || out["ParameterName"] != "cfn-params" || out["SubnetName"] == "" ||
		out["Port"] != "5439" || !strings.HasSuffix(out["Address"], ".us-east-1.redshift.amazonaws.com") {
		t.Fatal(out)
	}

	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{
			StackName:    new("redshift-stack"),
			TemplateBody: new(redshiftTemplate(*subnet.Subnet.SubnetId, 5440, "two")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	updated := wantStatus(ctx, t, cf, "redshift-stack", cfntypes.StackStatusUpdateComplete)
	next := outputsOf(&updated)
	if next["Identifier"] != out["Identifier"] || next["Address"] != out["Address"] || next["Port"] != "5440" ||
		next["SubnetName"] != out["SubnetName"] {
		t.Fatal(next)
	}

	cluster, err := c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterIdentifier: new("cfn-warehouse")})
	if err != nil || *cluster.Clusters[0].Endpoint.Port != 5440 || *cluster.Clusters[0].Tags[0].Value != "two" {
		t.Fatal(cluster, err)
	}

	group, err := c.DescribeClusterSubnetGroups(
		ctx,
		&sdk.DescribeClusterSubnetGroupsInput{ClusterSubnetGroupName: new(out["SubnetName"])},
	)
	if err != nil || *group.ClusterSubnetGroups[0].Description != "two" ||
		*group.ClusterSubnetGroups[0].Tags[0].Value != "two" {
		t.Fatal(group, err)
	}

	params, err := c.DescribeClusterParameterGroups(
		ctx,
		&sdk.DescribeClusterParameterGroupsInput{ParameterGroupName: new("cfn-params")},
	)
	if err != nil || *params.ParameterGroups[0].Tags[0].Value != "two" {
		t.Fatal(params, err)
	}

	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("redshift-stack")})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	_, err = c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterIdentifier: new("cfn-warehouse")})
	var missing *types.ClusterNotFoundFault
	if !errors.As(err, &missing) {
		t.Fatal(err)
	}

	groups, err := c.DescribeClusterSubnetGroups(ctx, &sdk.DescribeClusterSubnetGroupsInput{})
	if err != nil || len(groups.ClusterSubnetGroups) != 0 {
		t.Fatal(groups, err)
	}

	params, err = c.DescribeClusterParameterGroups(ctx, &sdk.DescribeClusterParameterGroupsInput{})
	if err != nil || len(params.ParameterGroups) != 1 {
		t.Fatal(params, err)
	}
}
