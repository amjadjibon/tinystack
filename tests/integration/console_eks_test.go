package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleEKSLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, service, operation, account, region, input)

		return consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
	}
	eks := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return call("eks", operation, input)
	}
	var caps map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &caps) != http.StatusOK || !consolePage(caps, "eks") ||
		caps["eks_runtime"] != "records" {
		t.Fatal(caps)
	}
	vpc := call("ec2", "CreateVpc", map[string]any{"CidrBlock": "10.30.0.0/16"})["Vpc"].(map[string]any)["VpcId"]
	subnet := func(cidr, zone string) any {
		t.Helper()
		out := call("ec2", "CreateSubnet", map[string]any{"VpcId": vpc, "CidrBlock": cidr, "AvailabilityZone": zone})

		return out["Subnet"].(map[string]any)["SubnetId"]
	}
	subnets := []any{subnet("10.30.0.0/24", "eu-west-1a"), subnet("10.30.1.0/24", "eu-west-1b")}
	role := "arn:aws:iam::" + account + ":role/eks"

	created := eks("CreateCluster", map[string]any{
		"Name":               "console-eks",
		"Version":            "1.34",
		"RoleArn":            role,
		"ResourcesVpcConfig": map[string]any{"SubnetIds": subnets, "EndpointPublicAccess": true},
		"AccessConfig": map[string]any{
			"AuthenticationMode":                      "API",
			"BootstrapClusterCreatorAdminPermissions": false,
		},
		"Tags": map[string]any{"env": "test"},
	})
	c := created["Cluster"].(map[string]any)
	if c["Status"] != "ACTIVE" || c["Endpoint"] == "" || c["CertificateAuthority"].(map[string]any)["Data"] == "" ||
		c["Identity"].(map[string]any)["Oidc"] == nil {
		t.Fatal(c)
	}
	if len(eks("ListClusters", map[string]any{})["Clusters"].([]any)) != 1 {
		t.Fatal("missing cluster")
	}
	eks("UpdateClusterVersion", map[string]any{"Name": "console-eks", "Version": "1.35"})
	eks(
		"UpdateClusterConfig",
		map[string]any{
			"Name": "console-eks",
			"Logging": map[string]any{
				"ClusterLogging": []any{map[string]any{"Types": []any{"api"}, "Enabled": true}},
			},
		},
	)
	eks(
		"UpdateClusterConfig",
		map[string]any{"Name": "console-eks", "ResourcesVpcConfig": map[string]any{"EndpointPrivateAccess": true}},
	)
	eks("TagResource", map[string]any{"ResourceArn": c["Arn"], "Tags": map[string]any{"team": "core"}})
	eks("UntagResource", map[string]any{"ResourceArn": c["Arn"], "TagKeys": []any{"env"}})
	c = eks("DescribeCluster", map[string]any{"Name": "console-eks"})["Cluster"].(map[string]any)
	if c["Version"] != "1.35" || c["Tags"].(map[string]any)["team"] != "core" ||
		c["Tags"].(map[string]any)["env"] != nil {
		t.Fatal(c)
	}
	cluster := map[string]any{"ClusterName": "console-eks"}
	eks(
		"CreateNodegroup",
		map[string]any{
			"ClusterName":   "console-eks",
			"NodegroupName": "workers",
			"NodeRole":      role,
			"Subnets":       subnets,
			"ScalingConfig": map[string]any{"MinSize": 0, "MaxSize": 3, "DesiredSize": 1},
			"Labels":        map[string]any{"app": "test"},
			"InstanceTypes": []any{"t3.medium"},
			"CapacityType":  "SPOT",
			"AmiType":       "AL2023_x86_64_STANDARD",
		},
	)
	eks(
		"UpdateNodegroupConfig",
		map[string]any{
			"ClusterName":   "console-eks",
			"NodegroupName": "workers",
			"ScalingConfig": map[string]any{"DesiredSize": 2},
		},
	)
	described := eks("DescribeNodegroup", map[string]any{"ClusterName": "console-eks", "NodegroupName": "workers"})
	ng := described["Nodegroup"].(map[string]any)
	if ng["ScalingConfig"].(map[string]any)["DesiredSize"] != float64(2) {
		t.Fatal(ng)
	}
	eks("DescribeClusterVersions", map[string]any{"IncludeAll": true})
	eks("DescribeAddonVersions", map[string]any{"KubernetesVersion": "1.35"})
	eks("CreateAddon", map[string]any{"ClusterName": "console-eks", "AddonName": "coredns"})
	eks("DescribeAddon", map[string]any{"ClusterName": "console-eks", "AddonName": "coredns"})
	eks(
		"CreateFargateProfile",
		map[string]any{
			"ClusterName":         "console-eks",
			"FargateProfileName":  "pods",
			"PodExecutionRoleArn": role,
			"Subnets":             subnets,
			"Selectors":           []any{map[string]any{"Namespace": "default"}},
		},
	)
	eks("DescribeFargateProfile", map[string]any{"ClusterName": "console-eks", "FargateProfileName": "pods"})
	principal := "arn:aws:iam::" + account + ":role/developer"
	entry := map[string]any{"ClusterName": "console-eks", "PrincipalArn": principal}
	eks("CreateAccessEntry", entry)
	eks("DescribeAccessEntry", entry)
	policies := eks("ListAccessPolicies", map[string]any{})["AccessPolicies"].([]any)
	policy := policies[0].(map[string]any)["Arn"]
	eks(
		"AssociateAccessPolicy",
		map[string]any{
			"ClusterName":  "console-eks",
			"PrincipalArn": principal,
			"PolicyArn":    policy,
			"AccessScope":  map[string]any{"Type": "cluster"},
		},
	)
	if len(eks("ListAssociatedAccessPolicies", entry)["AssociatedAccessPolicies"].([]any)) != 1 {
		t.Fatal("missing policy")
	}
	for _, op := range []string{"ListNodegroups", "ListAddons", "ListFargateProfiles", "ListAccessEntries"} {
		eks(op, cluster)
	}
	eks(
		"DisassociateAccessPolicy",
		map[string]any{"ClusterName": "console-eks", "PrincipalArn": principal, "PolicyArn": policy},
	)
	eks("DeleteAccessEntry", entry)
	eks("DeleteAddon", map[string]any{"ClusterName": "console-eks", "AddonName": "coredns"})
	eks("DeleteFargateProfile", map[string]any{"ClusterName": "console-eks", "FargateProfileName": "pods"})
	eks("DeleteNodegroup", map[string]any{"ClusterName": "console-eks", "NodegroupName": "workers"})
	eks("DeleteCluster", map[string]any{"Name": "console-eks"})
	denied := consoleResponse(
		t,
		srv,
		consoleLambdaRequest(t, srv, "eks", "ListUpdates", account, region, map[string]any{"Name": "console-eks"}),
		http.StatusBadRequest,
	)
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}
}
