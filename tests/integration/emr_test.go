package integration_test

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/emr"
	"github.com/aws/aws-sdk-go-v2/service/emr/types"
	iamsdk "github.com/aws/aws-sdk-go-v2/service/iam"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestEMRClassicControlPlane(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	identity := iamsdk.New(iamsdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	role, err := identity.CreateRole(
		ctx,
		&iamsdk.CreateRoleInput{
			RoleName: new("emr-service"),
			AssumeRolePolicyDocument: new(
				`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
					`"Principal":{"Service":["elasticmapreduce.amazonaws.com","ec2.amazonaws.com"]},"Action":"sts:AssumeRole"}]}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = identity.CreateInstanceProfile(
		ctx,
		&iamsdk.CreateInstanceProfileInput{InstanceProfileName: new("emr-profile")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = identity.AddRoleToInstanceProfile(
		ctx,
		&iamsdk.AddRoleToInstanceProfileInput{InstanceProfileName: new("emr-profile"), RoleName: role.Role.RoleName},
	)
	if err != nil {
		t.Fatal(err)
	}

	network := ec2sdk.New(ec2sdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	vpc, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.50.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(
		ctx,
		&ec2sdk.CreateSubnetInput{VpcId: vpc.Vpc.VpcId, CidrBlock: new("10.50.1.0/24")},
	)
	if err != nil {
		t.Fatal(err)
	}

	sg, err := network.CreateSecurityGroup(
		ctx,
		&ec2sdk.CreateSecurityGroupInput{VpcId: vpc.Vpc.VpcId, GroupName: new("emr"), Description: new("EMR metadata")},
	)
	if err != nil {
		t.Fatal(err)
	}

	bucket := s3sdk.New(
		s3sdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	_, err = bucket.CreateBucket(ctx, &s3sdk.CreateBucketInput{Bucket: new("emr-logs")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateSecurityConfiguration(
		ctx,
		&sdk.CreateSecurityConfigurationInput{
			Name:                  new("secure"),
			SecurityConfiguration: new(`{"EncryptionConfiguration":{}}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	security, err := c.DescribeSecurityConfiguration(ctx, &sdk.DescribeSecurityConfigurationInput{Name: new("secure")})
	if err != nil || aws.ToString(security.SecurityConfiguration) != `{"EncryptionConfiguration":{}}` {
		t.Fatal(security, err)
	}

	configs, err := c.ListSecurityConfigurations(ctx, &sdk.ListSecurityConfigurationsInput{})
	if err != nil || len(configs.SecurityConfigurations) != 1 {
		t.Fatal(configs, err)
	}

	create := func(name string, keep bool) *sdk.RunJobFlowInput {
		return &sdk.RunJobFlowInput{
			Name: new(
				name,
			),
			ReleaseLabel:          new("emr-7.12.0"),
			ServiceRole:           role.Role.Arn,
			JobFlowRole:           new("emr-profile"),
			LogUri:                new("s3://emr-logs/cluster"),
			SecurityConfiguration: new("secure"),
			Applications: []types.Application{
				{Name: new("Spark")},
			},
			Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
			Instances: &types.JobFlowInstancesConfig{
				Ec2SubnetId: subnet.Subnet.SubnetId, EmrManagedMasterSecurityGroup: sg.GroupId,
				Ec2KeyName: new("metadata-key"), KeepJobFlowAliveWhenNoSteps: new(keep),
				InstanceGroups: []types.InstanceGroupConfig{
					{
						InstanceRole:  types.InstanceRoleTypeMaster,
						InstanceType:  new("m5.xlarge"),
						InstanceCount: new(int32(1)),
					},
					{
						InstanceRole:  types.InstanceRoleTypeCore,
						InstanceType:  new("m5.xlarge"),
						InstanceCount: new(int32(2)),
					},
				},
			},
			BootstrapActions: []types.BootstrapActionConfig{
				{
					Name:                  new("bootstrap"),
					ScriptBootstrapAction: &types.ScriptBootstrapActionConfig{Path: new("s3://emr-logs/bootstrap.sh")},
				},
			},
		}
	}

	started, err := c.RunJobFlow(ctx, create("analytics", true))
	if err != nil {
		t.Fatal(err)
	}

	id := started.JobFlowId
	clusterStates := []types.ClusterState{
		types.ClusterStateBootstrapping, types.ClusterStateRunning, types.ClusterStateWaiting,
	}

	for _, want := range clusterStates {
		got, e := c.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterId: id})
		if e != nil || got.Cluster.Status.State != want {
			t.Fatal(got, e, want)
		}
	}

	_, err = c.SetTerminationProtection(
		ctx,
		&sdk.SetTerminationProtectionInput{JobFlowIds: []string{*id}, TerminationProtected: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TerminateJobFlows(ctx, &sdk.TerminateJobFlowsInput{JobFlowIds: []string{*id}})
	wantEMRError(t, err, "ValidationException")
	_, err = c.SetVisibleToAllUsers(
		ctx,
		&sdk.SetVisibleToAllUsersInput{JobFlowIds: []string{*id}, VisibleToAllUsers: new(false)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyCluster(ctx, &sdk.ModifyClusterInput{ClusterId: id, StepConcurrencyLevel: new(int32(2))})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AddJobFlowSteps(
		ctx,
		&sdk.AddJobFlowStepsInput{
			JobFlowId: id,
			Steps: []types.StepConfig{
				{
					Name:            new("bad concurrency"),
					ActionOnFailure: types.ActionOnFailureCancelAndWait,
					HadoopJarStep:   &types.HadoopJarStepConfig{Jar: new("command-runner.jar")},
				},
			},
		},
	)
	wantEMRError(t, err, "ValidationException")
	_, err = c.ModifyCluster(ctx, &sdk.ModifyClusterInput{ClusterId: id, StepConcurrencyLevel: new(int32(1))})
	if err != nil {
		t.Fatal(err)
	}

	groups, err := c.ListInstanceGroups(ctx, &sdk.ListInstanceGroupsInput{ClusterId: id})
	if err != nil || len(groups.InstanceGroups) != 2 {
		t.Fatal(groups, err)
	}

	added, err := c.AddInstanceGroups(
		ctx,
		&sdk.AddInstanceGroupsInput{
			JobFlowId: id,
			InstanceGroups: []types.InstanceGroupConfig{
				{
					Name:          new("task"),
					InstanceRole:  types.InstanceRoleTypeTask,
					InstanceType:  new("m5.xlarge"),
					InstanceCount: new(int32(1)),
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyInstanceGroups(
		ctx,
		&sdk.ModifyInstanceGroupsInput{
			ClusterId: id,
			InstanceGroups: []types.InstanceGroupModifyConfig{
				{InstanceGroupId: new(added.InstanceGroupIds[0]), InstanceCount: new(int32(3))},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	instances, err := c.ListInstances(
		ctx,
		&sdk.ListInstancesInput{ClusterId: id, InstanceGroupId: new(added.InstanceGroupIds[0])},
	)
	if err != nil || len(instances.Instances) != 3 {
		t.Fatal(instances, err)
	}

	prefix := netip.MustParsePrefix("10.50.1.0/24")
	for _, v := range instances.Instances {
		if !prefix.Contains(netip.MustParseAddr(*v.PrivateIpAddress)) {
			t.Fatal(v)
		}
	}

	bootstrap, err := c.ListBootstrapActions(ctx, &sdk.ListBootstrapActionsInput{ClusterId: id})
	if err != nil || len(bootstrap.BootstrapActions) != 1 {
		t.Fatal(bootstrap, err)
	}

	actions := []types.ActionOnFailure{
		types.ActionOnFailureContinue, types.ActionOnFailureCancelAndWait, types.ActionOnFailureTerminateCluster,
	}

	for _, action := range actions {
		steps, e := c.AddJobFlowSteps(
			ctx,
			&sdk.AddJobFlowStepsInput{
				JobFlowId: id,
				Steps: []types.StepConfig{
					{
						Name:            new(string(action)),
						ActionOnFailure: action,
						HadoopJarStep: &types.HadoopJarStepConfig{
							Jar:  new("command-runner.jar"),
							Args: []string{"echo", "nothing executes"},
						},
					},
				},
			},
		)
		if e != nil {
			t.Fatal(e)
		}

		for _, want := range []types.StepState{types.StepStateRunning, types.StepStateCompleted} {
			st, e := c.DescribeStep(ctx, &sdk.DescribeStepInput{ClusterId: id, StepId: new(steps.StepIds[0])})
			if e != nil || st.Step.Status.State != want {
				t.Fatal(st, e, want)
			}
		}
	}

	pending, err := c.AddJobFlowSteps(
		ctx,
		&sdk.AddJobFlowStepsInput{
			JobFlowId: id,
			Steps: []types.StepConfig{
				{Name: new("cancel"), HadoopJarStep: &types.HadoopJarStepConfig{Jar: new("command-runner.jar")}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	cancelled, err := c.CancelSteps(ctx, &sdk.CancelStepsInput{ClusterId: id, StepIds: pending.StepIds})
	if err != nil || cancelled.CancelStepsInfoList[0].Status != types.CancelStepsRequestStatusSubmitted {
		t.Fatal(cancelled, err)
	}

	listed, err := c.ListSteps(
		ctx,
		&sdk.ListStepsInput{
			ClusterId:  id,
			StepIds:    pending.StepIds,
			StepStates: []types.StepState{types.StepStateCancelled},
		},
	)
	if err != nil || len(listed.Steps) != 1 {
		t.Fatal(listed, err)
	}

	_, err = c.AddTags(
		ctx,
		&sdk.AddTagsInput{ResourceId: id, Tags: []types.Tag{{Key: new("environment"), Value: new("test")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RemoveTags(ctx, &sdk.RemoveTagsInput{ResourceId: id, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterId: id})
	if err != nil || len(got.Cluster.Tags) != 1 || *got.Cluster.Tags[0].Key != "environment" ||
		*got.Cluster.VisibleToAllUsers {
		t.Fatal(got, err)
	}

	filtered, err := c.ListClusters(
		ctx,
		&sdk.ListClustersInput{
			ClusterStates: []types.ClusterState{types.ClusterStateWaiting},
			CreatedAfter:  new(time.Now().Add(-time.Hour)),
			CreatedBefore: new(time.Now().Add(time.Hour)),
		},
	)
	if err != nil || len(filtered.Clusters) != 1 {
		t.Fatal(filtered, err)
	}

	taggingClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, tagErr := taggingClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{*started.ClusterArn},
			Tags:            map[string]string{"shared": "yes"},
		},
	)
	if tagErr != nil || len(tagged.FailedResourcesMap) > 0 {
		t.Fatal(tagged, tagErr)
	}

	untagged, tagErr := taggingClient.UntagResources(
		ctx,
		&rgta.UntagResourcesInput{ResourceARNList: []string{*started.ClusterArn}, TagKeys: []string{"shared"}},
	)
	if tagErr != nil || len(untagged.FailedResourcesMap) > 0 {
		t.Fatal(untagged, tagErr)
	}

	otherScopes := []sdk.Options{
		{Region: "us-west-2", Credentials: creds("test")},
		{Region: "us-east-1", Credentials: creds("team-b")},
	}

	for _, opts := range otherScopes {
		opts.BaseEndpoint = new(s.URL)
		other := sdk.New(opts)
		_, e := other.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterId: id})
		wantEMRError(t, e, "ValidationException")
		empty, e := other.ListClusters(ctx, &sdk.ListClustersInput{})
		if e != nil || len(empty.Clusters) != 0 {
			t.Fatal(empty, e)
		}
	}

	fleetInput := create("fleets", true)
	fleetInput.Instances.InstanceGroups = nil
	fleetInput.Instances.Ec2SubnetId = nil
	fleetInput.Instances.Ec2SubnetIds = []string{*subnet.Subnet.SubnetId}
	fleetInput.Instances.InstanceFleets = []types.InstanceFleetConfig{
		{
			Name:                   new("master"),
			InstanceFleetType:      types.InstanceFleetTypeMaster,
			TargetOnDemandCapacity: new(int32(1)),
			InstanceTypeConfigs:    []types.InstanceTypeConfig{{InstanceType: new("m5.xlarge")}},
		},
	}

	fleetCluster, err := c.RunJobFlow(ctx, fleetInput)
	if err != nil {
		t.Fatal(err)
	}

	fleet, err := c.AddInstanceFleet(
		ctx,
		&sdk.AddInstanceFleetInput{
			ClusterId: fleetCluster.JobFlowId,
			InstanceFleet: &types.InstanceFleetConfig{
				InstanceFleetType:   types.InstanceFleetTypeTask,
				TargetSpotCapacity:  new(int32(2)),
				InstanceTypeConfigs: []types.InstanceTypeConfig{{InstanceType: new("m5.xlarge")}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyInstanceFleet(
		ctx,
		&sdk.ModifyInstanceFleetInput{
			ClusterId: fleetCluster.JobFlowId,
			InstanceFleet: &types.InstanceFleetModifyConfig{
				InstanceFleetId:    fleet.InstanceFleetId,
				TargetSpotCapacity: new(int32(3)),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	fleets, err := c.ListInstanceFleets(ctx, &sdk.ListInstanceFleetsInput{ClusterId: fleetCluster.JobFlowId})
	if err != nil || len(fleets.InstanceFleets) != 2 {
		t.Fatal(fleets, err)
	}

	fi, err := c.ListInstances(
		ctx,
		&sdk.ListInstancesInput{ClusterId: fleetCluster.JobFlowId, InstanceFleetId: fleet.InstanceFleetId},
	)
	if err != nil || len(fi.Instances) != 3 {
		t.Fatal(fi, err)
	}

	autoInput := create("auto", false)
	autoInput.AutoTerminationPolicy = &types.AutoTerminationPolicy{IdleTimeout: new(int64(60))}
	autoInput.Steps = []types.StepConfig{
		{Name: new("auto-step"), HadoopJarStep: &types.HadoopJarStepConfig{Jar: new("command-runner.jar")}},
	}

	auto, err := c.RunJobFlow(ctx, autoInput)
	if err != nil {
		t.Fatal(err)
	}

	policy, policyErr := c.GetAutoTerminationPolicy(ctx, &sdk.GetAutoTerminationPolicyInput{ClusterId: auto.JobFlowId})
	if policyErr != nil || policy.AutoTerminationPolicy == nil || *policy.AutoTerminationPolicy.IdleTimeout != 60 {
		t.Fatal(policy, policyErr)
	}

	for range 5 {
		got, err = c.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterId: auto.JobFlowId})
		if err != nil {
			t.Fatal(err)
		}
	}

	if got.Cluster.Status.State != types.ClusterStateTerminated ||
		got.Cluster.Status.StateChangeReason.Code != types.ClusterStateChangeReasonCodeAllStepsCompleted {
		t.Fatal(got)
	}

	_, err = c.SetTerminationProtection(
		ctx,
		&sdk.SetTerminationProtectionInput{JobFlowIds: []string{*id}, TerminationProtected: new(false)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TerminateJobFlows(ctx, &sdk.TerminateJobFlowsInput{JobFlowIds: []string{*id}})
	if err != nil {
		t.Fatal(err)
	}

	got, err = c.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterId: id})
	if err != nil || got.Cluster.Status.State != types.ClusterStateTerminated {
		t.Fatal(got, err)
	}

	for name, mutate := range map[string]func(*sdk.RunJobFlowInput){
		"release":  func(v *sdk.RunJobFlowInput) { v.ReleaseLabel = new("emr-99.0.0") },
		"subnet":   func(v *sdk.RunJobFlowInput) { v.Instances.Ec2SubnetId = new("subnet-missing") },
		"group":    func(v *sdk.RunJobFlowInput) { v.Instances.EmrManagedMasterSecurityGroup = new("sg-missing") },
		"role":     func(v *sdk.RunJobFlowInput) { v.ServiceRole = new("missing") },
		"profile":  func(v *sdk.RunJobFlowInput) { v.JobFlowRole = new("missing") },
		"bucket":   func(v *sdk.RunJobFlowInput) { v.LogUri = new("s3://missing-emr-logs/path") },
		"loguri":   func(v *sdk.RunJobFlowInput) { v.LogUri = new("https://example.com") },
		"security": func(v *sdk.RunJobFlowInput) { v.SecurityConfiguration = new("missing") },
	} {
		t.Run(name, func(t *testing.T) {
			v := create(name, true)
			mutate(v)
			_, e := c.RunJobFlow(ctx, v)
			if e == nil {
				t.Fatal("accepted invalid reference")
			}
		})
	}

	_, err = c.CreateSecurityConfiguration(
		ctx,
		&sdk.CreateSecurityConfigurationInput{Name: new("invalid"), SecurityConfiguration: new("not JSON")},
	)
	wantEMRError(t, err, "ValidationException")
	for i := range 51 {
		_, err = c.CreateSecurityConfiguration(
			ctx,
			&sdk.CreateSecurityConfigurationInput{
				Name:                  new(fmt.Sprintf("page-%02d", i)),
				SecurityConfiguration: new("{}"),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.ListSecurityConfigurations(ctx, &sdk.ListSecurityConfigurationsInput{})
	if err != nil || len(first.SecurityConfigurations) != 50 || first.Marker == nil {
		t.Fatal(first, err)
	}

	second, err := c.ListSecurityConfigurations(ctx, &sdk.ListSecurityConfigurationsInput{Marker: first.Marker})
	if err != nil || len(second.SecurityConfigurations) != 2 || second.Marker != nil {
		t.Fatal(second, err)
	}

	for i := range 48 {
		_, e := c.RunJobFlow(ctx, create(fmt.Sprintf("page-%02d", i), true))
		if e != nil {
			t.Fatal(e)
		}
	}

	clusterPage, e := c.ListClusters(ctx, &sdk.ListClustersInput{})
	if e != nil || len(clusterPage.Clusters) != 50 || clusterPage.Marker == nil {
		t.Fatal(clusterPage, e)
	}

	clusterTail, e := c.ListClusters(ctx, &sdk.ListClustersInput{Marker: clusterPage.Marker})
	if e != nil || len(clusterTail.Clusters) != 1 || clusterTail.Marker != nil {
		t.Fatal(clusterTail, e)
	}

	_, e = c.ListClusters(ctx, &sdk.ListClustersInput{Marker: new("invalid!")})
	wantEMRError(t, e, "ValidationException")

	_, err = c.DeleteSecurityConfiguration(ctx, &sdk.DeleteSecurityConfigurationInput{Name: new("secure")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeSecurityConfiguration(ctx, &sdk.DescribeSecurityConfigurationInput{Name: new("secure")})
	wantEMRError(t, err, "ValidationException")
}

func TestEMRScalingAndBlockPublicAccess(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)

	// Block Public Access: default when never set
	bpaDefault, err := c.GetBlockPublicAccessConfiguration(ctx, &sdk.GetBlockPublicAccessConfigurationInput{})
	if err != nil {
		t.Fatal(err)
	}

	if bpaDefault.BlockPublicAccessConfiguration == nil ||
		!aws.ToBool(bpaDefault.BlockPublicAccessConfiguration.BlockPublicSecurityGroupRules) {
		t.Fatalf("expected BlockPublicSecurityGroupRules to be true by default, got %v", bpaDefault)
	}

	ranges := bpaDefault.BlockPublicAccessConfiguration.PermittedPublicSecurityGroupRuleRanges
	if len(ranges) != 1 || aws.ToInt32(ranges[0].MinRange) != 22 || aws.ToInt32(ranges[0].MaxRange) != 22 {
		t.Fatalf("expected permitted range 22-22, got %v", ranges)
	}

	// Block Public Access: validation errors
	_, err = c.PutBlockPublicAccessConfiguration(ctx, &sdk.PutBlockPublicAccessConfigurationInput{
		BlockPublicAccessConfiguration: &types.BlockPublicAccessConfiguration{
			BlockPublicSecurityGroupRules: aws.Bool(true),
			PermittedPublicSecurityGroupRuleRanges: []types.PortRange{
				{
					MinRange: aws.Int32(100),
					MaxRange: aws.Int32(50),
				},
			},
		},
	})
	wantEMRError(t, err, "InvalidRequestException")

	_, err = c.PutBlockPublicAccessConfiguration(ctx, &sdk.PutBlockPublicAccessConfigurationInput{
		BlockPublicAccessConfiguration: &types.BlockPublicAccessConfiguration{
			BlockPublicSecurityGroupRules: aws.Bool(true),
			PermittedPublicSecurityGroupRuleRanges: []types.PortRange{
				{
					MinRange: aws.Int32(-5),
					MaxRange: aws.Int32(80),
				},
			},
		},
	})
	wantEMRError(t, err, "InvalidRequestException")

	_, err = c.PutBlockPublicAccessConfiguration(ctx, &sdk.PutBlockPublicAccessConfigurationInput{
		BlockPublicAccessConfiguration: &types.BlockPublicAccessConfiguration{
			BlockPublicSecurityGroupRules: aws.Bool(true),
			PermittedPublicSecurityGroupRuleRanges: []types.PortRange{
				{
					MinRange: aws.Int32(80),
					MaxRange: aws.Int32(70000),
				},
			},
		},
	})
	wantEMRError(t, err, "InvalidRequestException")

	// Block Public Access: valid Put and round trip Get
	_, err = c.PutBlockPublicAccessConfiguration(ctx, &sdk.PutBlockPublicAccessConfigurationInput{
		BlockPublicAccessConfiguration: &types.BlockPublicAccessConfiguration{
			BlockPublicSecurityGroupRules: aws.Bool(true),
			PermittedPublicSecurityGroupRuleRanges: []types.PortRange{
				{
					MinRange: aws.Int32(80),
					MaxRange: aws.Int32(80),
				},
				{
					MinRange: aws.Int32(443),
					MaxRange: aws.Int32(443),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	bpaUpdated, err := c.GetBlockPublicAccessConfiguration(ctx, &sdk.GetBlockPublicAccessConfigurationInput{})
	if err != nil {
		t.Fatal(err)
	}

	if bpaUpdated.BlockPublicAccessConfiguration == nil ||
		!aws.ToBool(bpaUpdated.BlockPublicAccessConfiguration.BlockPublicSecurityGroupRules) ||
		len(bpaUpdated.BlockPublicAccessConfiguration.PermittedPublicSecurityGroupRuleRanges) != 2 {
		t.Fatalf("unexpected updated bpa configuration: %v", bpaUpdated.BlockPublicAccessConfiguration)
	}

	if bpaUpdated.BlockPublicAccessConfigurationMetadata == nil ||
		bpaUpdated.BlockPublicAccessConfigurationMetadata.CreationDateTime == nil ||
		aws.ToString(bpaUpdated.BlockPublicAccessConfigurationMetadata.CreatedByArn) == "" {
		t.Fatalf("expected bpa metadata to be populated, got %v", bpaUpdated.BlockPublicAccessConfigurationMetadata)
	}

	// Setup IAM, Network, and S3 for clusters
	identity := iamsdk.New(iamsdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	role, err := identity.CreateRole(
		ctx,
		&iamsdk.CreateRoleInput{
			RoleName: new("emr-scaling-role"),
			AssumeRolePolicyDocument: new(
				`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
					`"Principal":{"Service":["elasticmapreduce.amazonaws.com","ec2.amazonaws.com"]},"Action":"sts:AssumeRole"}]}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = identity.CreateInstanceProfile(
		ctx,
		&iamsdk.CreateInstanceProfileInput{InstanceProfileName: new("emr-scaling-profile")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = identity.AddRoleToInstanceProfile(
		ctx,
		&iamsdk.AddRoleToInstanceProfileInput{
			InstanceProfileName: new("emr-scaling-profile"),
			RoleName:            role.Role.RoleName,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	network := ec2sdk.New(ec2sdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	vpc, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.60.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(
		ctx,
		&ec2sdk.CreateSubnetInput{VpcId: vpc.Vpc.VpcId, CidrBlock: new("10.60.1.0/24")},
	)
	if err != nil {
		t.Fatal(err)
	}

	sg, err := network.CreateSecurityGroup(
		ctx,
		&ec2sdk.CreateSecurityGroupInput{
			VpcId:       vpc.Vpc.VpcId,
			GroupName:   new("emr-scaling-sg"),
			Description: new("EMR scaling sg"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	bucket := s3sdk.New(
		s3sdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	_, err = bucket.CreateBucket(ctx, &s3sdk.CreateBucketInput{Bucket: new("emr-scaling-logs")})
	if err != nil {
		t.Fatal(err)
	}

	// Create cluster with instance groups
	clusterOut, err := c.RunJobFlow(ctx, &sdk.RunJobFlowInput{
		Name:         new("scaling-cluster"),
		ReleaseLabel: new("emr-7.12.0"),
		ServiceRole:  role.Role.Arn,
		JobFlowRole:  new("emr-scaling-profile"),
		LogUri:       new("s3://emr-scaling-logs/cluster"),
		Applications: []types.Application{{Name: new("Spark")}},
		Instances: &types.JobFlowInstancesConfig{
			Ec2SubnetId:                   subnet.Subnet.SubnetId,
			EmrManagedMasterSecurityGroup: sg.GroupId,
			KeepJobFlowAliveWhenNoSteps:   new(true),
			InstanceGroups: []types.InstanceGroupConfig{
				{
					InstanceRole:  types.InstanceRoleTypeMaster,
					InstanceType:  new("m5.xlarge"),
					InstanceCount: new(int32(1)),
				},
				{
					InstanceRole:  types.InstanceRoleTypeCore,
					InstanceType:  new("m5.xlarge"),
					InstanceCount: new(int32(2)),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	clusterId := clusterOut.JobFlowId

	// Create cluster with instance fleets
	fleetClusterOut, err := c.RunJobFlow(ctx, &sdk.RunJobFlowInput{
		Name:         new("fleet-cluster"),
		ReleaseLabel: new("emr-7.12.0"),
		ServiceRole:  role.Role.Arn,
		JobFlowRole:  new("emr-scaling-profile"),
		LogUri:       new("s3://emr-scaling-logs/cluster"),
		Applications: []types.Application{{Name: new("Spark")}},
		Instances: &types.JobFlowInstancesConfig{
			Ec2SubnetId:                   subnet.Subnet.SubnetId,
			EmrManagedMasterSecurityGroup: sg.GroupId,
			KeepJobFlowAliveWhenNoSteps:   new(true),
			InstanceFleets: []types.InstanceFleetConfig{
				{
					InstanceFleetType:      types.InstanceFleetTypeMaster,
					TargetOnDemandCapacity: new(int32(1)),
					InstanceTypeConfigs: []types.InstanceTypeConfig{
						{InstanceType: new("m5.xlarge")},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	fleetClusterId := fleetClusterOut.JobFlowId

	// Get instance group ID for auto-scaling policy
	groupsOut, err := c.ListInstanceGroups(ctx, &sdk.ListInstanceGroupsInput{ClusterId: clusterId})
	if err != nil || len(groupsOut.InstanceGroups) < 2 {
		t.Fatalf("expected instance groups, got %v, err: %v", groupsOut, err)
	}

	coreGroup := groupsOut.InstanceGroups[1]
	igId := coreGroup.Id

	// Managed Scaling Policy: initial state is empty
	mspInit, err := c.GetManagedScalingPolicy(ctx, &sdk.GetManagedScalingPolicyInput{ClusterId: clusterId})
	if err != nil {
		t.Fatal(err)
	}

	if mspInit.ManagedScalingPolicy != nil {
		t.Fatalf("expected nil managed scaling policy initially, got %v", mspInit.ManagedScalingPolicy)
	}

	// Managed Scaling Policy: validation errors
	_, err = c.GetManagedScalingPolicy(ctx, &sdk.GetManagedScalingPolicyInput{ClusterId: aws.String("j-MISSING")})
	wantEMRError(t, err, "ValidationException")

	_, err = c.PutManagedScalingPolicy(ctx, &sdk.PutManagedScalingPolicyInput{
		ClusterId: clusterId,
		ManagedScalingPolicy: &types.ManagedScalingPolicy{
			ComputeLimits: &types.ComputeLimits{
				UnitType:             types.ComputeLimitsUnitType("INVALID"),
				MinimumCapacityUnits: aws.Int32(1),
				MaximumCapacityUnits: aws.Int32(10),
			},
		},
	})
	wantEMRError(t, err, "ValidationException")

	_, err = c.PutManagedScalingPolicy(ctx, &sdk.PutManagedScalingPolicyInput{
		ClusterId: clusterId,
		ManagedScalingPolicy: &types.ManagedScalingPolicy{
			ComputeLimits: &types.ComputeLimits{
				UnitType:             types.ComputeLimitsUnitTypeInstances,
				MinimumCapacityUnits: aws.Int32(20),
				MaximumCapacityUnits: aws.Int32(10),
			},
		},
	})
	wantEMRError(t, err, "ValidationException")

	_, err = c.PutManagedScalingPolicy(ctx, &sdk.PutManagedScalingPolicyInput{
		ClusterId: clusterId,
		ManagedScalingPolicy: &types.ManagedScalingPolicy{
			ComputeLimits: &types.ComputeLimits{
				UnitType:                     types.ComputeLimitsUnitTypeInstances,
				MinimumCapacityUnits:         aws.Int32(1),
				MaximumCapacityUnits:         aws.Int32(10),
				MaximumOnDemandCapacityUnits: aws.Int32(15),
			},
		},
	})
	wantEMRError(t, err, "ValidationException")

	// Managed Scaling Policy: put, get and remove
	_, err = c.PutManagedScalingPolicy(ctx, &sdk.PutManagedScalingPolicyInput{
		ClusterId: clusterId,
		ManagedScalingPolicy: &types.ManagedScalingPolicy{
			ComputeLimits: &types.ComputeLimits{
				UnitType:                     types.ComputeLimitsUnitTypeInstances,
				MinimumCapacityUnits:         aws.Int32(1),
				MaximumCapacityUnits:         aws.Int32(10),
				MaximumOnDemandCapacityUnits: aws.Int32(5),
				MaximumCoreCapacityUnits:     aws.Int32(4),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	mspGet, err := c.GetManagedScalingPolicy(ctx, &sdk.GetManagedScalingPolicyInput{ClusterId: clusterId})
	if err != nil {
		t.Fatal(err)
	}

	if mspGet.ManagedScalingPolicy == nil || mspGet.ManagedScalingPolicy.ComputeLimits == nil {
		t.Fatalf("expected managed scaling policy, got %v", mspGet)
	}

	limits := mspGet.ManagedScalingPolicy.ComputeLimits
	if limits.UnitType != types.ComputeLimitsUnitTypeInstances ||
		aws.ToInt32(limits.MinimumCapacityUnits) != 1 ||
		aws.ToInt32(limits.MaximumCapacityUnits) != 10 ||
		aws.ToInt32(limits.MaximumOnDemandCapacityUnits) != 5 ||
		aws.ToInt32(limits.MaximumCoreCapacityUnits) != 4 {
		t.Fatalf("unexpected compute limits: %v", limits)
	}

	_, err = c.RemoveManagedScalingPolicy(ctx, &sdk.RemoveManagedScalingPolicyInput{ClusterId: clusterId})
	if err != nil {
		t.Fatal(err)
	}

	mspAfterRemove, err := c.GetManagedScalingPolicy(ctx, &sdk.GetManagedScalingPolicyInput{ClusterId: clusterId})
	if err != nil {
		t.Fatal(err)
	}

	if mspAfterRemove.ManagedScalingPolicy != nil {
		t.Fatalf("expected nil managed scaling policy after remove, got %v", mspAfterRemove.ManagedScalingPolicy)
	}

	// Auto Scaling Policy: validation errors
	validRule := types.ScalingRule{
		Name: aws.String("scale-out-cpu"),
		Action: &types.ScalingAction{
			SimpleScalingPolicyConfiguration: &types.SimpleScalingPolicyConfiguration{
				AdjustmentType:    types.AdjustmentTypeChangeInCapacity,
				ScalingAdjustment: aws.Int32(1),
				CoolDown:          aws.Int32(300),
			},
		},
		Trigger: &types.ScalingTrigger{
			CloudWatchAlarmDefinition: &types.CloudWatchAlarmDefinition{
				ComparisonOperator: types.ComparisonOperatorGreaterThan,
				MetricName:         aws.String("CPUUtilization"),
				Period:             aws.Int32(300),
				Threshold:          aws.Float64(80.0),
			},
		},
	}

	validPolicy := &types.AutoScalingPolicy{
		Constraints: &types.ScalingConstraints{
			MinCapacity: aws.Int32(1),
			MaxCapacity: aws.Int32(5),
		},
		Rules: []types.ScalingRule{validRule},
	}

	// Non-existent cluster
	_, err = c.PutAutoScalingPolicy(ctx, &sdk.PutAutoScalingPolicyInput{
		ClusterId:         aws.String("j-MISSING"),
		InstanceGroupId:   igId,
		AutoScalingPolicy: validPolicy,
	})
	wantEMRError(t, err, "ValidationException")

	// Non-existent instance group
	_, err = c.PutAutoScalingPolicy(ctx, &sdk.PutAutoScalingPolicyInput{
		ClusterId:         clusterId,
		InstanceGroupId:   aws.String("ig-MISSING"),
		AutoScalingPolicy: validPolicy,
	})
	wantEMRError(t, err, "ValidationException")

	// Fleet cluster rejection
	_, err = c.PutAutoScalingPolicy(ctx, &sdk.PutAutoScalingPolicyInput{
		ClusterId:         fleetClusterId,
		InstanceGroupId:   aws.String("ig-dummy"),
		AutoScalingPolicy: validPolicy,
	})
	wantEMRError(t, err, "ValidationException")

	_, err = c.RemoveAutoScalingPolicy(ctx, &sdk.RemoveAutoScalingPolicyInput{
		ClusterId:       fleetClusterId,
		InstanceGroupId: aws.String("ig-dummy"),
	})
	wantEMRError(t, err, "ValidationException")

	// MinCapacity > MaxCapacity
	_, err = c.PutAutoScalingPolicy(ctx, &sdk.PutAutoScalingPolicyInput{
		ClusterId:       clusterId,
		InstanceGroupId: igId,
		AutoScalingPolicy: &types.AutoScalingPolicy{
			Constraints: &types.ScalingConstraints{
				MinCapacity: aws.Int32(10),
				MaxCapacity: aws.Int32(2),
			},
			Rules: []types.ScalingRule{validRule},
		},
	})
	wantEMRError(t, err, "ValidationException")

	// Rule validation: missing name
	badRuleName := validRule
	badRuleName.Name = aws.String("   ")
	_, err = c.PutAutoScalingPolicy(ctx, &sdk.PutAutoScalingPolicyInput{
		ClusterId:       clusterId,
		InstanceGroupId: igId,
		AutoScalingPolicy: &types.AutoScalingPolicy{
			Constraints: validPolicy.Constraints,
			Rules:       []types.ScalingRule{badRuleName},
		},
	})
	wantEMRError(t, err, "ValidationException")

	// Rule validation: missing configuration
	badRuleAction := validRule
	badRuleAction.Action = &types.ScalingAction{
		SimpleScalingPolicyConfiguration: &types.SimpleScalingPolicyConfiguration{
			AdjustmentType:    types.AdjustmentType(""),
			ScalingAdjustment: aws.Int32(1),
		},
	}
	_, err = c.PutAutoScalingPolicy(ctx, &sdk.PutAutoScalingPolicyInput{
		ClusterId:       clusterId,
		InstanceGroupId: igId,
		AutoScalingPolicy: &types.AutoScalingPolicy{
			Constraints: validPolicy.Constraints,
			Rules:       []types.ScalingRule{badRuleAction},
		},
	})
	wantEMRError(t, err, "ValidationException")

	// Rule validation: empty metric name
	badRuleTrigger := validRule
	badRuleTrigger.Trigger = &types.ScalingTrigger{
		CloudWatchAlarmDefinition: &types.CloudWatchAlarmDefinition{
			MetricName:         aws.String("   "),
			ComparisonOperator: types.ComparisonOperatorGreaterThan,
			Period:             aws.Int32(300),
			Threshold:          aws.Float64(80.0),
		},
	}
	_, err = c.PutAutoScalingPolicy(ctx, &sdk.PutAutoScalingPolicyInput{
		ClusterId:       clusterId,
		InstanceGroupId: igId,
		AutoScalingPolicy: &types.AutoScalingPolicy{
			Constraints: validPolicy.Constraints,
			Rules:       []types.ScalingRule{badRuleTrigger},
		},
	})
	wantEMRError(t, err, "ValidationException")

	// Auto Scaling Policy: put, list and remove
	putOut, err := c.PutAutoScalingPolicy(ctx, &sdk.PutAutoScalingPolicyInput{
		ClusterId:         clusterId,
		InstanceGroupId:   igId,
		AutoScalingPolicy: validPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(putOut.ClusterId) != aws.ToString(clusterId) ||
		aws.ToString(putOut.InstanceGroupId) != aws.ToString(igId) ||
		putOut.ClusterArn == nil {
		t.Fatalf("unexpected putAutoScalingPolicy output: %v", putOut)
	}

	if putOut.AutoScalingPolicy == nil ||
		putOut.AutoScalingPolicy.Status == nil ||
		putOut.AutoScalingPolicy.Status.State != types.AutoScalingPolicyStateAttached {
		t.Fatalf("expected attached status, got %v", putOut.AutoScalingPolicy)
	}

	// Verify policy in ListInstanceGroups
	listGroupsAfterPut, err := c.ListInstanceGroups(ctx, &sdk.ListInstanceGroupsInput{ClusterId: clusterId})
	if err != nil {
		t.Fatal(err)
	}

	var foundGroup *types.InstanceGroup
	for i := range listGroupsAfterPut.InstanceGroups {
		if aws.ToString(listGroupsAfterPut.InstanceGroups[i].Id) == aws.ToString(igId) {
			foundGroup = &listGroupsAfterPut.InstanceGroups[i]
			break
		}
	}

	if foundGroup == nil || foundGroup.AutoScalingPolicy == nil ||
		foundGroup.AutoScalingPolicy.Status.State != types.AutoScalingPolicyStateAttached {
		t.Fatalf("expected attached auto scaling policy on instance group, got %v", foundGroup)
	}

	// Remove auto-scaling policy
	_, err = c.RemoveAutoScalingPolicy(ctx, &sdk.RemoveAutoScalingPolicyInput{
		ClusterId:       clusterId,
		InstanceGroupId: igId,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify policy cleared in ListInstanceGroups
	listGroupsAfterRemove, err := c.ListInstanceGroups(ctx, &sdk.ListInstanceGroupsInput{ClusterId: clusterId})
	if err != nil {
		t.Fatal(err)
	}

	foundGroup = nil
	for i := range listGroupsAfterRemove.InstanceGroups {
		if aws.ToString(listGroupsAfterRemove.InstanceGroups[i].Id) == aws.ToString(igId) {
			foundGroup = &listGroupsAfterRemove.InstanceGroups[i]
			break
		}
	}

	if foundGroup == nil || foundGroup.AutoScalingPolicy != nil {
		t.Fatalf("expected nil auto scaling policy after remove, got %v", foundGroup)
	}

	// A terminated cluster's scaling policies cannot change.
	if _, err = c.TerminateJobFlows(ctx, &sdk.TerminateJobFlowsInput{JobFlowIds: []string{*clusterId}}); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutManagedScalingPolicy(ctx, &sdk.PutManagedScalingPolicyInput{
		ClusterId: clusterId,
		ManagedScalingPolicy: &types.ManagedScalingPolicy{ComputeLimits: &types.ComputeLimits{
			UnitType: types.ComputeLimitsUnitTypeInstances, MinimumCapacityUnits: aws.Int32(1),
			MaximumCapacityUnits: aws.Int32(2),
		}},
	})
	wantEMRError(t, err, "ValidationException")
}
