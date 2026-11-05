package integration_test

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

const (
	clusterRole = "arn:aws:iam::000000000000:role/eks-cluster"
	nodeRole    = "arn:aws:iam::000000000000:role/eks-node"
)

func eksClient(s *httptest.Server, region string) *eks.Client {
	return eks.New(eks.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// defaultSubnets returns the IDs of the default VPC's subnets.
func defaultSubnets(ctx context.Context, t *testing.T, s *httptest.Server) (vpc string, subnets []string) {
	t.Helper()
	out, err := ec2Client(s, "test", "us-east-1").DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("default-for-az"), Values: []string{"true"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := range out.Subnets {
		subnets = append(subnets, aws.ToString(out.Subnets[i].SubnetId))
		vpc = aws.ToString(out.Subnets[i].VpcId)
	}

	return vpc, subnets
}

// createEKSCluster creates a cluster in the default subnets.
func createEKSCluster(
	ctx context.Context,
	t *testing.T,
	c *eks.Client,
	subnets []string,
	name string,
	mode ekstypes.AuthenticationMode,
) *ekstypes.Cluster {
	t.Helper()
	out, err := c.CreateCluster(ctx, &eks.CreateClusterInput{
		Name: aws.String(name), RoleArn: aws.String(clusterRole),
		ResourcesVpcConfig: &ekstypes.VpcConfigRequest{SubnetIds: subnets},
		AccessConfig:       &ekstypes.CreateAccessConfigRequest{AuthenticationMode: mode},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.Cluster
}

func TestEKSClusters(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := eksClient(s, "us-east-1")
	vpc, subnets := defaultSubnets(ctx, t, s)

	input := &eks.CreateClusterInput{
		ClientRequestToken: new("cluster-once"),
		Name:               aws.String("apps"), RoleArn: aws.String(clusterRole),
		ResourcesVpcConfig: &ekstypes.VpcConfigRequest{SubnetIds: subnets[:2], EndpointPrivateAccess: aws.Bool(true)},
		Logging: &ekstypes.Logging{ClusterLogging: []ekstypes.LogSetup{
			{Types: []ekstypes.LogType{ekstypes.LogTypeAudit, ekstypes.LogTypeApi}, Enabled: aws.Bool(true)},
		}},
		Tags: map[string]string{"team": "platform"},
	}
	out, err := c.CreateCluster(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	repeated, replayErr := c.CreateCluster(ctx, input)
	if replayErr != nil || aws.ToString(repeated.Cluster.Arn) != aws.ToString(out.Cluster.Arn) {
		t.Fatalf("cluster replay: %+v %v", repeated, replayErr)
	}

	input.Version = new("1.33")
	_, mismatchErr := c.CreateCluster(ctx, input)
	code(t, mismatchErr, "InvalidParameterException")

	cl := out.Cluster
	if aws.ToString(cl.Arn) != "arn:aws:eks:us-east-1:000000000000:cluster/apps" ||
		cl.Status != ekstypes.ClusterStatusActive ||
		aws.ToString(cl.Version) != "1.34" || !strings.HasPrefix(aws.ToString(cl.Endpoint), "https://") ||
		aws.ToString(cl.ResourcesVpcConfig.VpcId) != vpc || !cl.ResourcesVpcConfig.EndpointPrivateAccess ||
		!cl.ResourcesVpcConfig.EndpointPublicAccess ||
		cl.AccessConfig.AuthenticationMode != ekstypes.AuthenticationModeConfigMap ||
		!strings.HasPrefix(aws.ToString(cl.Identity.Oidc.Issuer), "https://oidc.eks.us-east-1.amazonaws.com/id/") ||
		cl.Tags["team"] != "platform" {
		t.Fatalf("cluster = %+v", cl)
	}

	data, err := base64.StdEncoding.DecodeString(aws.ToString(cl.CertificateAuthority.Data))
	if err != nil {
		t.Fatal(err)
	}

	block, _ := pem.Decode(data)
	if ca, parseErr := x509.ParseCertificate(block.Bytes); parseErr != nil || !ca.IsCA {
		t.Fatalf("certificate authority: %v", parseErr)
	}

	logs := cl.Logging.ClusterLogging
	if len(logs) != 2 || !slices.Equal(logs[0].Types, []ekstypes.LogType{ekstypes.LogTypeApi, ekstypes.LogTypeAudit}) ||
		!aws.ToBool(logs[0].Enabled) || aws.ToBool(logs[1].Enabled) || len(logs[1].Types) != 3 {
		t.Fatalf("logging = %+v", logs)
	}

	_, err = c.CreateCluster(ctx, &eks.CreateClusterInput{
		Name: aws.String("apps"), RoleArn: aws.String(clusterRole),
		ResourcesVpcConfig: &ekstypes.VpcConfigRequest{SubnetIds: subnets},
	})
	code(t, err, "ResourceInUseException")
	for name, vpcConfig := range map[string]*ekstypes.VpcConfigRequest{
		"one subnet":     {SubnetIds: subnets[:1]},
		"missing subnet": {SubnetIds: []string{subnets[0], "subnet-00000000000000000"}},
		"no endpoint": {
			SubnetIds: subnets, EndpointPublicAccess: aws.Bool(false), EndpointPrivateAccess: aws.Bool(false),
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, createErr := c.CreateCluster(ctx, &eks.CreateClusterInput{
				Name: aws.String("bad"), RoleArn: aws.String(clusterRole), ResourcesVpcConfig: vpcConfig,
			})
			code(t, createErr, "InvalidParameterException")
		})
	}

	createEKSCluster(ctx, t, c, subnets, "batch", "")
	listed, err := c.ListClusters(ctx, &eks.ListClustersInput{MaxResults: aws.Int32(1)})
	if err != nil || !slices.Equal(listed.Clusters, []string{"apps"}) || listed.NextToken == nil {
		t.Fatalf("first page = %+v, %v", listed, err)
	}

	rest, err := c.ListClusters(ctx, &eks.ListClustersInput{NextToken: listed.NextToken})
	if err != nil || !slices.Equal(rest.Clusters, []string{"batch"}) {
		t.Fatalf("second page = %+v, %v", rest, err)
	}

	_, err = c.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String("missing")})
	code(t, err, "ResourceNotFoundException")

	t.Run("updates", func(t *testing.T) {
		name := aws.String("apps")
		updateInput := &eks.UpdateClusterConfigInput{
			ClientRequestToken: new("logging-once"),
			Name:               name, Logging: &ekstypes.Logging{ClusterLogging: []ekstypes.LogSetup{
				{Types: []ekstypes.LogType{ekstypes.LogTypeAudit}, Enabled: aws.Bool(false)},
			}},
		}
		updated, err := c.UpdateClusterConfig(ctx, updateInput)
		if err != nil {
			t.Fatal(err)
		}

		repeatedUpdate, replayErr := c.UpdateClusterConfig(ctx, updateInput)
		if replayErr != nil || aws.ToString(repeatedUpdate.Update.Id) != aws.ToString(updated.Update.Id) {
			t.Fatalf("update replay: %+v %v", repeatedUpdate, replayErr)
		}

		updateInput.ZonalShiftConfig = &ekstypes.ZonalShiftConfigRequest{Enabled: new(true)}
		_, mismatchErr := c.UpdateClusterConfig(ctx, updateInput)
		code(t, mismatchErr, "InvalidParameterException")

		u := updated.Update
		if u.Status != ekstypes.UpdateStatusSuccessful || u.Type != ekstypes.UpdateTypeLoggingUpdate {
			t.Fatalf("update = %+v", u)
		}

		described, err := c.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: name, UpdateId: u.Id})
		if err != nil || aws.ToString(described.Update.Id) != aws.ToString(u.Id) {
			t.Fatalf("described = %+v, %v", described, err)
		}

		_, err = c.UpdateClusterConfig(ctx, &eks.UpdateClusterConfigInput{
			Name: name, UpgradePolicy: &ekstypes.UpgradePolicyRequest{SupportType: ekstypes.SupportTypeStandard},
			ZonalShiftConfig: &ekstypes.ZonalShiftConfigRequest{Enabled: aws.Bool(true)},
		})
		code(t, err, "InvalidParameterException")

		if _, err = c.UpdateClusterConfig(ctx, &eks.UpdateClusterConfigInput{
			Name:         name,
			AccessConfig: &ekstypes.UpdateAccessConfigRequest{AuthenticationMode: ekstypes.AuthenticationModeApi},
		}); err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateClusterConfig(ctx, &eks.UpdateClusterConfigInput{
			Name:         name,
			AccessConfig: &ekstypes.UpdateAccessConfigRequest{AuthenticationMode: ekstypes.AuthenticationModeConfigMap},
		})
		code(t, err, "InvalidParameterException")

		if _, err = c.UpdateClusterVersion(ctx, &eks.UpdateClusterVersionInput{
			Name: name, Version: aws.String("1.35"),
		}); err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateClusterVersion(
			ctx,
			&eks.UpdateClusterVersionInput{Name: aws.String("batch"), Version: aws.String("1.32")},
		)
		code(t, err, "InvalidParameterException")

		got, err := c.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: name})
		if err != nil || aws.ToString(got.Cluster.Version) != "1.35" || len(got.Cluster.Logging.ClusterLogging) != 2 ||
			len(got.Cluster.Logging.ClusterLogging[0].Types) != 1 {
			t.Fatalf("after updates = %+v, %v", got, err)
		}

		updates, err := c.ListUpdates(ctx, &eks.ListUpdatesInput{Name: name})
		if err != nil || len(updates.UpdateIds) != 3 || updates.UpdateIds[0] != aws.ToString(u.Id) {
			t.Fatalf("updates = %+v, %v", updates, err)
		}
	})

	t.Run("versions", func(t *testing.T) {
		versions, err := c.DescribeClusterVersions(ctx, &eks.DescribeClusterVersionsInput{DefaultOnly: aws.Bool(true)})
		if err != nil || len(versions.ClusterVersions) != 1 ||
			aws.ToString(versions.ClusterVersions[0].ClusterVersion) != "1.34" {
			t.Fatalf("versions = %+v, %v", versions, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if _, err := c.UpdateClusterConfig(ctx, &eks.UpdateClusterConfigInput{
			Name: aws.String("batch"), DeletionProtection: aws.Bool(true),
		}); err != nil {
			t.Fatal(err)
		}

		_, err := c.DeleteCluster(ctx, &eks.DeleteClusterInput{Name: aws.String("batch")})
		code(t, err, "InvalidRequestException")
		deleted, err := c.DeleteCluster(ctx, &eks.DeleteClusterInput{Name: aws.String("apps")})
		if err != nil || deleted.Cluster.Status != ekstypes.ClusterStatusDeleting {
			t.Fatalf("deleted = %+v, %v", deleted, err)
		}

		_, err = c.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String("apps")})
		code(t, err, "ResourceNotFoundException")
	})
}

func TestEKSNodegroupsAndFargate(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := eksClient(s, "us-east-1")
	_, subnets := defaultSubnets(ctx, t, s)
	createEKSCluster(ctx, t, c, subnets, "apps", "")
	cluster := aws.String("apps")

	created, err := c.CreateNodegroup(ctx, &eks.CreateNodegroupInput{
		ClusterName:   cluster,
		NodegroupName: aws.String("workers"),
		NodeRole:      aws.String(nodeRole),
		Subnets:       subnets,
		Labels:        map[string]string{"role": "worker"},
		Taints: []ekstypes.Taint{
			{Key: aws.String("gpu"), Value: aws.String("true"), Effect: ekstypes.TaintEffectNoSchedule},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ng := created.Nodegroup
	if ng.Status != ekstypes.NodegroupStatusActive || aws.ToString(ng.Version) != "1.34" ||
		aws.ToInt32(ng.ScalingConfig.DesiredSize) != 2 || !slices.Equal(ng.InstanceTypes, []string{"t3.medium"}) ||
		ng.AmiType != ekstypes.AMITypesAl2023X8664Standard || aws.ToInt32(ng.DiskSize) != 20 ||
		ng.CapacityType != ekstypes.CapacityTypesOnDemand || len(ng.Resources.AutoScalingGroups) != 1 ||
		!strings.HasPrefix(
			aws.ToString(ng.NodegroupArn),
			"arn:aws:eks:us-east-1:000000000000:nodegroup/apps/workers/",
		) {
		t.Fatalf("node group = %+v", ng)
	}

	_, err = c.CreateNodegroup(ctx, &eks.CreateNodegroupInput{
		ClusterName:   cluster,
		NodegroupName: aws.String("bad"),
		NodeRole:      aws.String(nodeRole),
		Subnets:       subnets,
		ScalingConfig: &ekstypes.NodegroupScalingConfig{
			MinSize:     aws.Int32(3),
			MaxSize:     aws.Int32(5),
			DesiredSize: aws.Int32(1),
		},
	})
	code(t, err, "InvalidParameterException")
	_, err = c.CreateNodegroup(ctx, &eks.CreateNodegroupInput{
		ClusterName: aws.String("missing"), NodegroupName: aws.String("x"), NodeRole: aws.String(nodeRole),
		Subnets: subnets,
	})
	code(t, err, "ResourceNotFoundException")

	configured, err := c.UpdateNodegroupConfig(ctx, &eks.UpdateNodegroupConfigInput{
		ClusterName:   cluster,
		NodegroupName: aws.String("workers"),
		Labels: &ekstypes.UpdateLabelsPayload{
			AddOrUpdateLabels: map[string]string{"tier": "a"},
			RemoveLabels:      []string{"role"},
		},
		Taints:        &ekstypes.UpdateTaintsPayload{RemoveTaints: ng.Taints},
		ScalingConfig: &ekstypes.NodegroupScalingConfig{DesiredSize: aws.Int32(1)},
	})
	if err != nil || configured.Update.Type != ekstypes.UpdateTypeConfigUpdate {
		t.Fatalf("configured = %+v, %v", configured, err)
	}

	if _, err = c.DescribeUpdate(ctx, &eks.DescribeUpdateInput{
		Name: cluster, UpdateId: configured.Update.Id, NodegroupName: aws.String("workers"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: cluster, UpdateId: configured.Update.Id})
	code(t, err, "ResourceNotFoundException")

	described, err := c.DescribeNodegroup(
		ctx,
		&eks.DescribeNodegroupInput{ClusterName: cluster, NodegroupName: aws.String("workers")},
	)
	if err != nil || described.Nodegroup.Labels["tier"] != "a" || described.Nodegroup.Labels["role"] != "" ||
		len(described.Nodegroup.Taints) != 0 || aws.ToInt32(described.Nodegroup.ScalingConfig.DesiredSize) != 1 {
		t.Fatalf("described = %+v, %v", described, err)
	}

	_, err = c.UpdateNodegroupVersion(ctx, &eks.UpdateNodegroupVersionInput{
		ClusterName: cluster, NodegroupName: aws.String("workers"), Version: aws.String("1.35"),
	})
	code(t, err, "InvalidParameterException")

	t.Run("fargate", func(t *testing.T) {
		profile, fErr := c.CreateFargateProfile(ctx, &eks.CreateFargateProfileInput{
			ClusterName: cluster, FargateProfileName: aws.String("jobs"), Subnets: subnets[:2],
			PodExecutionRoleArn: aws.String("arn:aws:iam::000000000000:role/pods"),
			Selectors:           []ekstypes.FargateProfileSelector{{Namespace: aws.String("jobs")}},
		})
		if fErr != nil || profile.FargateProfile.Status != ekstypes.FargateProfileStatusActive {
			t.Fatalf("profile = %+v, %v", profile, fErr)
		}

		_, fErr = c.CreateFargateProfile(ctx, &eks.CreateFargateProfileInput{
			ClusterName: cluster, FargateProfileName: aws.String("none"),
			PodExecutionRoleArn: aws.String("arn:aws:iam::000000000000:role/pods"),
		})
		code(t, fErr, "InvalidParameterException")

		names, fErr := c.ListFargateProfiles(ctx, &eks.ListFargateProfilesInput{ClusterName: cluster})
		if fErr != nil || !slices.Equal(names.FargateProfileNames, []string{"jobs"}) {
			t.Fatalf("profiles = %+v, %v", names, fErr)
		}

		_, fErr = c.DeleteCluster(ctx, &eks.DeleteClusterInput{Name: cluster})
		code(t, fErr, "ResourceInUseException")
		if _, fErr = c.DeleteFargateProfile(ctx, &eks.DeleteFargateProfileInput{
			ClusterName: cluster, FargateProfileName: aws.String("jobs"),
		}); fErr != nil {
			t.Fatal(fErr)
		}
	})

	if _, err = c.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{
		ClusterName: cluster, NodegroupName: aws.String("workers"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteCluster(ctx, &eks.DeleteClusterInput{Name: cluster}); err != nil {
		t.Fatal(err)
	}
}

func TestEKSAddonsAccessAndIdentity(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := eksClient(s, "us-east-1")
	_, subnets := defaultSubnets(ctx, t, s)
	createEKSCluster(ctx, t, c, subnets, "legacy", "")
	cl := createEKSCluster(ctx, t, c, subnets, "apps", ekstypes.AuthenticationModeApiAndConfigMap)
	cluster := aws.String("apps")

	t.Run("addons", func(t *testing.T) {
		versions, err := c.DescribeAddonVersions(ctx, &eks.DescribeAddonVersionsInput{
			AddonName: aws.String("kube-proxy"), KubernetesVersion: aws.String("1.34"),
		})
		if err != nil || len(versions.Addons) != 1 ||
			!strings.HasPrefix(aws.ToString(versions.Addons[0].AddonVersions[0].AddonVersion), "v1.34.") {
			t.Fatalf("versions = %+v, %v", versions, err)
		}

		added, err := c.CreateAddon(ctx, &eks.CreateAddonInput{
			ClusterName:         cluster,
			AddonName:           aws.String("coredns"),
			ConfigurationValues: aws.String(`{"replicaCount":3}`),
		})
		if err != nil || added.Addon.Status != ekstypes.AddonStatusActive ||
			aws.ToString(added.Addon.AddonVersion) != "v1.12.3-eksbuild.1" {
			t.Fatalf("addon = %+v, %v", added, err)
		}

		_, err = c.CreateAddon(ctx, &eks.CreateAddonInput{ClusterName: cluster, AddonName: aws.String("istio")})
		code(t, err, "InvalidParameterException")
		_, err = c.CreateAddon(ctx, &eks.CreateAddonInput{ClusterName: cluster, AddonName: aws.String("coredns")})
		code(t, err, "ResourceInUseException")

		updated, err := c.UpdateAddon(ctx, &eks.UpdateAddonInput{
			ClusterName: cluster, AddonName: aws.String("coredns"), AddonVersion: aws.String("v1.11.4-eksbuild.14"),
		})
		if err != nil || updated.Update.Type != ekstypes.UpdateTypeAddonUpdate {
			t.Fatalf("update = %+v, %v", updated, err)
		}

		_, err = c.UpdateAddon(ctx, &eks.UpdateAddonInput{
			ClusterName: cluster, AddonName: aws.String("coredns"), AddonVersion: aws.String("v0.0.1"),
		})
		code(t, err, "InvalidParameterException")

		listed, err := c.ListAddons(ctx, &eks.ListAddonsInput{ClusterName: cluster})
		if err != nil || !slices.Equal(listed.Addons, []string{"coredns"}) {
			t.Fatalf("addons = %+v, %v", listed, err)
		}

		if _, err = c.DeleteAddon(
			ctx,
			&eks.DeleteAddonInput{ClusterName: cluster, AddonName: aws.String("coredns")},
		); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("access", func(t *testing.T) {
		_, err := c.ListAccessEntries(ctx, &eks.ListAccessEntriesInput{ClusterName: aws.String("legacy")})
		code(t, err, "InvalidRequestException")

		// The creator gets a cluster admin entry.
		admin := "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
		entries, err := c.ListAccessEntries(ctx, &eks.ListAccessEntriesInput{
			ClusterName: cluster, AssociatedPolicyArn: aws.String(admin),
		})
		if err != nil || !slices.Equal(entries.AccessEntries, []string{"arn:aws:iam::000000000000:root"}) {
			t.Fatalf("entries = %+v, %v", entries, err)
		}

		principal := aws.String("arn:aws:iam::000000000000:role/team/dev")
		entry, err := c.CreateAccessEntry(ctx, &eks.CreateAccessEntryInput{
			ClusterName: cluster, PrincipalArn: principal, KubernetesGroups: []string{"developers"},
		})
		if err != nil ||
			aws.ToString(entry.AccessEntry.Username) != "arn:aws:sts::000000000000:assumed-role/dev/{{SessionName}}" ||
			aws.ToString(entry.AccessEntry.Type) != "STANDARD" {
			t.Fatalf("entry = %+v, %v", entry, err)
		}

		_, err = c.CreateAccessEntry(ctx, &eks.CreateAccessEntryInput{
			ClusterName: cluster, PrincipalArn: aws.String("arn:aws:iam::000000000000:role/x"),
			KubernetesGroups: []string{"system:masters"},
		})
		code(t, err, "InvalidParameterException")

		associated, err := c.AssociateAccessPolicy(ctx, &eks.AssociateAccessPolicyInput{
			ClusterName: cluster, PrincipalArn: principal,
			PolicyArn:   aws.String("arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy"),
			AccessScope: &ekstypes.AccessScope{Type: ekstypes.AccessScopeTypeNamespace, Namespaces: []string{"dev"}},
		})
		if err != nil || associated.AssociatedAccessPolicy.AccessScope.Type != ekstypes.AccessScopeTypeNamespace {
			t.Fatalf("associated = %+v, %v", associated, err)
		}

		_, err = c.AssociateAccessPolicy(ctx, &eks.AssociateAccessPolicyInput{
			ClusterName:  cluster,
			PrincipalArn: principal,
			PolicyArn:    aws.String("arn:aws:eks::aws:cluster-access-policy/Made"),
			AccessScope:  &ekstypes.AccessScope{Type: ekstypes.AccessScopeTypeCluster},
		})
		code(t, err, "InvalidParameterException")

		policies, err := c.ListAssociatedAccessPolicies(ctx, &eks.ListAssociatedAccessPoliciesInput{
			ClusterName: cluster, PrincipalArn: principal,
		})
		if err != nil || len(policies.AssociatedAccessPolicies) != 1 {
			t.Fatalf("policies = %+v, %v", policies, err)
		}

		if _, err = c.DisassociateAccessPolicy(ctx, &eks.DisassociateAccessPolicyInput{
			ClusterName: cluster, PrincipalArn: principal, PolicyArn: associated.AssociatedAccessPolicy.PolicyArn,
		}); err != nil {
			t.Fatal(err)
		}

		all, err := c.ListAccessPolicies(ctx, &eks.ListAccessPoliciesInput{})
		if err != nil || len(all.AccessPolicies) != 5 {
			t.Fatalf("access policies = %+v, %v", all, err)
		}

		if _, err = c.DeleteAccessEntry(
			ctx,
			&eks.DeleteAccessEntryInput{ClusterName: cluster, PrincipalArn: principal},
		); err != nil {
			t.Fatal(err)
		}

		_, err = c.DescribeAccessEntry(
			ctx,
			&eks.DescribeAccessEntryInput{ClusterName: cluster, PrincipalArn: principal},
		)
		code(t, err, "ResourceNotFoundException")
	})

	t.Run("pod identity", func(t *testing.T) {
		created, err := c.CreatePodIdentityAssociation(ctx, &eks.CreatePodIdentityAssociationInput{
			ClusterName: cluster, Namespace: aws.String("default"), ServiceAccount: aws.String("app"),
			RoleArn: aws.String("arn:aws:iam::000000000000:role/app"),
		})
		if err != nil || !strings.HasPrefix(aws.ToString(created.Association.AssociationId), "a-") {
			t.Fatalf("association = %+v, %v", created, err)
		}

		_, err = c.CreatePodIdentityAssociation(ctx, &eks.CreatePodIdentityAssociationInput{
			ClusterName: cluster, Namespace: aws.String("default"), ServiceAccount: aws.String("app"),
			RoleArn: aws.String("arn:aws:iam::000000000000:role/other"),
		})
		code(t, err, "ResourceInUseException")

		id := created.Association.AssociationId
		updated, err := c.UpdatePodIdentityAssociation(ctx, &eks.UpdatePodIdentityAssociationInput{
			ClusterName: cluster, AssociationId: id, TargetRoleArn: aws.String("arn:aws:iam::111111111111:role/target"),
		})
		if err != nil || updated.Association.ExternalId == nil {
			t.Fatalf("updated = %+v, %v", updated, err)
		}

		listed, err := c.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{
			ClusterName: cluster, Namespace: aws.String("default"),
		})
		if err != nil || len(listed.Associations) != 1 {
			t.Fatalf("associations = %+v, %v", listed, err)
		}

		if _, err = c.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{
			ClusterName: cluster, AssociationId: id,
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("tags", func(t *testing.T) {
		added, err := c.CreateAddon(ctx, &eks.CreateAddonInput{ClusterName: cluster, AddonName: aws.String("vpc-cni")})
		if err != nil {
			t.Fatal(err)
		}

		for _, arn := range []*string{cl.Arn, added.Addon.AddonArn} {
			if _, err = c.TagResource(
				ctx,
				&eks.TagResourceInput{ResourceArn: arn, Tags: map[string]string{"env": "dev"}},
			); err != nil {
				t.Fatal(err)
			}

			tags, listErr := c.ListTagsForResource(ctx, &eks.ListTagsForResourceInput{ResourceArn: arn})
			if listErr != nil || tags.Tags["env"] != "dev" {
				t.Fatalf("%s tags = %+v, %v", aws.ToString(arn), tags, listErr)
			}

			if _, err = c.UntagResource(
				ctx,
				&eks.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"env"}},
			); err != nil {
				t.Fatal(err)
			}
		}

		_, err = c.TagResource(ctx, &eks.TagResourceInput{
			ResourceArn: aws.String("arn:aws:eks:us-east-1:000000000000:cluster/missing"),
			Tags:        map[string]string{"a": "b"},
		})
		code(t, err, "NotFoundException")
		_, err = c.TagResource(ctx, &eks.TagResourceInput{ResourceArn: cl.Arn, Tags: map[string]string{"aws:x": "b"}})
		code(t, err, "BadRequestException")
	})
}

func TestEKSIdentityProviderConfigs(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := eksClient(s, "us-east-1")
	_, subnets := defaultSubnets(ctx, t, s)
	createEKSCluster(ctx, t, c, subnets, "apps", "")
	cluster := aws.String("apps")
	ref := &ekstypes.IdentityProviderConfig{Type: aws.String("oidc"), Name: aws.String("google")}

	_, err := c.AssociateIdentityProviderConfig(ctx, &eks.AssociateIdentityProviderConfigInput{
		ClusterName: cluster, Oidc: &ekstypes.OidcIdentityProviderConfigRequest{
			IdentityProviderConfigName: aws.String("google"), IssuerUrl: aws.String("http://accounts.google.com"),
			ClientId: aws.String("client"),
		},
	})
	code(t, err, "InvalidParameterException")

	associated, err := c.AssociateIdentityProviderConfig(ctx, &eks.AssociateIdentityProviderConfigInput{
		ClusterName: cluster, Tags: map[string]string{"team": "core"},
		Oidc: &ekstypes.OidcIdentityProviderConfigRequest{
			IdentityProviderConfigName: aws.String("google"), IssuerUrl: aws.String("https://accounts.google.com"),
			ClientId: aws.String("client"), GroupsClaim: aws.String("groups"),
			RequiredClaims: map[string]string{"hd": "example.com"},
		},
	})
	if err != nil || associated.Update.Type != ekstypes.UpdateTypeAssociateIdentityProviderConfig ||
		associated.Update.Status != ekstypes.UpdateStatusSuccessful || associated.Tags["team"] != "core" {
		t.Fatalf("associate = %+v, %v", associated, err)
	}

	_, err = c.AssociateIdentityProviderConfig(ctx, &eks.AssociateIdentityProviderConfigInput{
		ClusterName: cluster, Oidc: &ekstypes.OidcIdentityProviderConfigRequest{
			IdentityProviderConfigName: aws.String("okta"), IssuerUrl: aws.String("https://example.okta.com"),
			ClientId: aws.String("client"),
		},
	})
	code(t, err, "ResourceInUseException")

	described, err := c.DescribeIdentityProviderConfig(ctx, &eks.DescribeIdentityProviderConfigInput{
		ClusterName: cluster, IdentityProviderConfig: ref,
	})
	if err != nil {
		t.Fatal(err)
	}

	oidc := described.IdentityProviderConfig.Oidc
	if oidc.Status != ekstypes.ConfigStatusActive || oidc.UsernameClaim != nil ||
		aws.ToString(oidc.GroupsClaim) != "groups" || oidc.RequiredClaims["hd"] != "example.com" ||
		!strings.Contains(aws.ToString(oidc.IdentityProviderConfigArn), ":identityproviderconfig/apps/oidc/google/") {
		t.Fatalf("config = %+v", oidc)
	}

	arn := oidc.IdentityProviderConfigArn
	if _, err = c.TagResource(ctx, &eks.TagResourceInput{
		ResourceArn: arn, Tags: map[string]string{"env": "dev"},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &eks.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatalf("tags = %+v, %v", tags, err)
	}

	listed, err := c.ListIdentityProviderConfigs(ctx, &eks.ListIdentityProviderConfigsInput{ClusterName: cluster})
	if err != nil || len(listed.IdentityProviderConfigs) != 1 ||
		aws.ToString(listed.IdentityProviderConfigs[0].Name) != "google" {
		t.Fatalf("list = %+v, %v", listed, err)
	}

	// Configs belong to a cluster in one Region.
	_, err = eksClient(s, "eu-west-1").ListIdentityProviderConfigs(ctx, &eks.ListIdentityProviderConfigsInput{
		ClusterName: cluster,
	})
	code(t, err, "ResourceNotFoundException")

	removed, err := c.DisassociateIdentityProviderConfig(ctx, &eks.DisassociateIdentityProviderConfigInput{
		ClusterName: cluster, IdentityProviderConfig: ref,
	})
	if err != nil || removed.Update.Type != ekstypes.UpdateTypeDisassociateIdentityProviderConfig {
		t.Fatalf("disassociate = %+v, %v", removed, err)
	}

	_, err = c.DescribeIdentityProviderConfig(ctx, &eks.DescribeIdentityProviderConfigInput{
		ClusterName: cluster, IdentityProviderConfig: ref,
	})
	code(t, err, "ResourceNotFoundException")
}
