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
