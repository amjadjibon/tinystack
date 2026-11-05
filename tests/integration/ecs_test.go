package integration_test

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

func ecsClient(s *httptest.Server, region string) *ecs.Client {
	return ecs.New(ecs.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// webTask registers a Fargate task definition of family with one container.
func webTask(ctx context.Context, t *testing.T, c *ecs.Client, family, image string) *ecstypes.TaskDefinition {
	t.Helper()
	out, err := c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(family),
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		Cpu:                     aws.String("256"),
		Memory:                  aws.String("512"),
		Tags:                    []ecstypes.Tag{{Key: aws.String("source"), Value: aws.String("definition")}},
		ContainerDefinitions: []ecstypes.ContainerDefinition{{
			Name:         aws.String("web"),
			Image:        aws.String(image),
			PortMappings: []ecstypes.PortMapping{{ContainerPort: aws.Int32(8080)}},
			Environment:  []ecstypes.KeyValuePair{{Name: aws.String("MODE"), Value: aws.String("test")}},
			HealthCheck:  &ecstypes.HealthCheck{Command: []string{"CMD", "true"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.TaskDefinition
}

func awsvpc() *ecstypes.NetworkConfiguration {
	return &ecstypes.NetworkConfiguration{AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
		Subnets: []string{"subnet-0123456789abcdef0"}, SecurityGroups: []string{"sg-0123456789abcdef0"},
	}}
}

func TestECSClusters(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ecsClient(s, "us-east-1")

	created, err := c.CreateCluster(ctx, &ecs.CreateClusterInput{
		ClusterName: aws.String("apps"),
		Tags:        []ecstypes.Tag{{Key: aws.String("team"), Value: aws.String("web")}},
		Settings: []ecstypes.ClusterSetting{
			{Name: ecstypes.ClusterSettingNameContainerInsights, Value: aws.String("enabled")},
		},
		CapacityProviders: []string{"FARGATE", "FARGATE_SPOT"},
		DefaultCapacityProviderStrategy: []ecstypes.CapacityProviderStrategyItem{
			{CapacityProvider: aws.String("FARGATE"), Weight: 1, Base: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cl := created.Cluster
	if aws.ToString(cl.ClusterArn) != "arn:aws:ecs:us-east-1:000000000000:cluster/apps" ||
		aws.ToString(cl.Status) != "ACTIVE" || len(cl.CapacityProviders) != 2 || len(cl.Tags) != 1 ||
		aws.ToString(cl.Settings[0].Value) != "enabled" {
		t.Fatalf("cluster = %+v", cl)
	}

	if _, err = c.CreateCluster(ctx, &ecs.CreateClusterInput{}); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateCluster(ctx, &ecs.CreateClusterInput{
		ClusterName: aws.String("ec2"), CapacityProviders: []string{"my-asg"},
	})
	code(t, err, "InvalidParameterException")
	_, err = c.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("bad name")})
	code(t, err, "InvalidParameterException")

	described, err := c.DescribeClusters(ctx, &ecs.DescribeClustersInput{
		Clusters: []string{"apps", "missing", "arn:aws:ecs:us-east-1:000000000000:cluster/default"},
		Include:  []ecstypes.ClusterField{ecstypes.ClusterFieldTags},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(described.Clusters) != 2 || len(described.Failures) != 1 ||
		aws.ToString(described.Failures[0].Reason) != "MISSING" || len(described.Clusters[0].Tags) != 1 ||
		described.Clusters[0].Settings != nil {
		t.Fatalf("described = %+v", described)
	}

	listed, err := c.ListClusters(ctx, &ecs.ListClustersInput{MaxResults: aws.Int32(1)})
	if err != nil || len(listed.ClusterArns) != 1 || listed.NextToken == nil {
		t.Fatalf("first page = %+v, %v", listed, err)
	}

	rest, err := c.ListClusters(ctx, &ecs.ListClustersInput{NextToken: listed.NextToken})
	if err != nil || len(rest.ClusterArns) != 1 || rest.NextToken != nil ||
		rest.ClusterArns[0] == listed.ClusterArns[0] {
		t.Fatalf("second page = %+v, %v", rest, err)
	}

	updated, err := c.UpdateClusterSettings(ctx, &ecs.UpdateClusterSettingsInput{
		Cluster: aws.String("apps"),
		Settings: []ecstypes.ClusterSetting{
			{Name: ecstypes.ClusterSettingNameContainerInsights, Value: aws.String("disabled")},
		},
	})
	if err != nil || aws.ToString(updated.Cluster.Settings[0].Value) != "disabled" {
		t.Fatalf("settings = %+v, %v", updated, err)
	}

	_, err = c.PutClusterCapacityProviders(ctx, &ecs.PutClusterCapacityProvidersInput{
		Cluster:           aws.String("apps"),
		CapacityProviders: []string{"FARGATE"},
		DefaultCapacityProviderStrategy: []ecstypes.CapacityProviderStrategyItem{
			{CapacityProvider: aws.String("FARGATE_SPOT")},
		},
	})
	code(t, err, "InvalidParameterException")

	// Clusters are regional.
	west, err := ecsClient(s, "us-west-2").ListClusters(ctx, &ecs.ListClustersInput{})
	if err != nil || len(west.ClusterArns) != 0 {
		t.Fatalf("us-west-2 = %+v, %v", west, err)
	}

	webTask(ctx, t, c, "web", "nginx")
	if _, err = c.CreateService(ctx, &ecs.CreateServiceInput{
		Cluster: aws.String("apps"), ServiceName: aws.String("web"), TaskDefinition: aws.String("web"),
		DesiredCount: aws.Int32(1), NetworkConfiguration: awsvpc(),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteCluster(ctx, &ecs.DeleteClusterInput{Cluster: aws.String("apps")})
	code(t, err, "ClusterContainsServicesException")
	if _, err = c.DeleteService(ctx, &ecs.DeleteServiceInput{
		Cluster: aws.String("apps"), Service: aws.String("web"), Force: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := c.DeleteCluster(ctx, &ecs.DeleteClusterInput{Cluster: aws.String("apps")})
	if err != nil || aws.ToString(deleted.Cluster.Status) != "INACTIVE" {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}

	gone, err := c.DescribeClusters(ctx, &ecs.DescribeClustersInput{Clusters: []string{"apps"}})
	if err != nil || aws.ToString(gone.Clusters[0].Status) != "INACTIVE" {
		t.Fatalf("deleted cluster = %+v, %v", gone, err)
	}

	_, err = c.ListServices(ctx, &ecs.ListServicesInput{Cluster: aws.String("apps")})
	code(t, err, "ClusterNotFoundException")
	if _, err = c.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("apps")}); err != nil {
		t.Fatal(err)
	}
}

func TestECSTaskDefinitions(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ecsClient(s, "us-east-1")

	first := webTask(ctx, t, c, "web", "nginx:1")
	if aws.ToString(first.TaskDefinitionArn) != "arn:aws:ecs:us-east-1:000000000000:task-definition/web:1" ||
		first.Revision != 1 || first.Status != ecstypes.TaskDefinitionStatusActive ||
		!slices.Contains(first.Compatibilities, ecstypes.CompatibilityFargate) || first.RegisteredAt == nil {
		t.Fatalf("first = %+v", first)
	}

	container := first.ContainerDefinitions[0]
	if !aws.ToBool(container.Essential) || container.PortMappings[0].Protocol != ecstypes.TransportProtocolTcp ||
		aws.ToInt32(container.PortMappings[0].HostPort) != 8080 {
		t.Fatalf("container defaults = %+v", container)
	}

	second := webTask(ctx, t, c, "web", "nginx:2")
	webTask(ctx, t, c, "api", "api:1")
	if second.Revision != 2 {
		t.Fatalf("second revision = %d", second.Revision)
	}

	for name, in := range map[string]*ecs.RegisterTaskDefinitionInput{
		"no memory": {Family: aws.String("x"), ContainerDefinitions: []ecstypes.ContainerDefinition{
			{Name: aws.String("a"), Image: aws.String("a")},
		}},
		"fargate size": {
			Family: aws.String("x"), NetworkMode: ecstypes.NetworkModeAwsvpc, Cpu: aws.String("256"),
			Memory: aws.String("4096"), RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
			ContainerDefinitions: []ecstypes.ContainerDefinition{{Name: aws.String("a"), Image: aws.String("a")}},
		},
		"fargate bridge": {
			Family: aws.String("x"), NetworkMode: ecstypes.NetworkModeBridge, Cpu: aws.String("256"),
			Memory: aws.String("512"), RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
			ContainerDefinitions: []ecstypes.ContainerDefinition{{Name: aws.String("a"), Image: aws.String("a")}},
		},
		"volume": {Family: aws.String("x"), Memory: aws.String("512"), ContainerDefinitions: []ecstypes.ContainerDefinition{
			{Name: aws.String("a"), Image: aws.String("a"), MountPoints: []ecstypes.MountPoint{
				{SourceVolume: aws.String("data"), ContainerPath: aws.String("/data")},
			}},
		}},
		"no essential": {
			Family: aws.String("x"), Memory: aws.String("512"), ContainerDefinitions: []ecstypes.ContainerDefinition{
				{Name: aws.String("a"), Image: aws.String("a"), Essential: aws.Bool(false)},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.RegisterTaskDefinition(ctx, in)
			code(t, err, "ClientException")
		})
	}

	// "1 vCPU" and "2 GB" become units.
	sized, err := c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("sized"), NetworkMode: ecstypes.NetworkModeAwsvpc, Cpu: aws.String("1 vCPU"),
		Memory: aws.String("2 GB"), RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		ContainerDefinitions: []ecstypes.ContainerDefinition{{Name: aws.String("a"), Image: aws.String("a")}},
	})
	if err != nil || aws.ToString(sized.TaskDefinition.Cpu) != "1024" ||
		aws.ToString(sized.TaskDefinition.Memory) != "2048" {
		t.Fatalf("sized = %+v, %v", sized, err)
	}

	latest, err := c.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String("web"), Include: []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
	})
	if err != nil || latest.TaskDefinition.Revision != 2 || len(latest.Tags) != 1 {
		t.Fatalf("latest = %+v, %v", latest, err)
	}

	_, err = c.DeregisterTaskDefinition(ctx, &ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String("web")})
	code(t, err, "ClientException")
	gone, err := c.DeregisterTaskDefinition(ctx, &ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: second.TaskDefinitionArn,
	})
	if err != nil || gone.TaskDefinition.Status != ecstypes.TaskDefinitionStatusInactive ||
		gone.TaskDefinition.DeregisteredAt == nil {
		t.Fatalf("deregister = %+v, %v", gone, err)
	}

	// The family now names its latest active revision.
	latest, err = c.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("web")})
	if err != nil || latest.TaskDefinition.Revision != 1 {
		t.Fatalf("latest active = %+v, %v", latest, err)
	}

	arns, err := c.ListTaskDefinitions(ctx, &ecs.ListTaskDefinitionsInput{Sort: ecstypes.SortOrderDesc})
	if err != nil || !slices.Equal(arns.TaskDefinitionArns, []string{
		"arn:aws:ecs:us-east-1:000000000000:task-definition/web:1",
		"arn:aws:ecs:us-east-1:000000000000:task-definition/sized:1",
		"arn:aws:ecs:us-east-1:000000000000:task-definition/api:1",
	}) {
		t.Fatalf("definitions = %+v, %v", arns, err)
	}

	paged, err := c.ListTaskDefinitions(ctx, &ecs.ListTaskDefinitionsInput{
		Sort: ecstypes.SortOrderDesc, MaxResults: aws.Int32(2),
	})
	if err != nil || len(paged.TaskDefinitionArns) != 2 {
		t.Fatalf("page = %+v, %v", paged, err)
	}

	rest, err := c.ListTaskDefinitions(ctx, &ecs.ListTaskDefinitionsInput{
		Sort: ecstypes.SortOrderDesc, NextToken: paged.NextToken,
	})
	if err != nil || !slices.Equal(rest.TaskDefinitionArns, arns.TaskDefinitionArns[2:]) {
		t.Fatalf("rest = %+v, %v", rest, err)
	}

	inactive, err := c.ListTaskDefinitions(ctx, &ecs.ListTaskDefinitionsInput{
		Status: ecstypes.TaskDefinitionStatusInactive, FamilyPrefix: aws.String("we"),
	})
	if err != nil || len(inactive.TaskDefinitionArns) != 1 {
		t.Fatalf("inactive = %+v, %v", inactive, err)
	}

	families, err := c.ListTaskDefinitionFamilies(ctx, &ecs.ListTaskDefinitionFamiliesInput{})
	if err != nil || !slices.Equal(families.Families, []string{"api", "sized", "web"}) {
		t.Fatalf("families = %+v, %v", families, err)
	}

	deleted, err := c.DeleteTaskDefinitions(ctx, &ecs.DeleteTaskDefinitionsInput{
		TaskDefinitions: []string{"web:2", "web:1", "web:9"},
	})
	if err != nil || len(deleted.TaskDefinitions) != 1 || len(deleted.Failures) != 2 ||
		deleted.TaskDefinitions[0].Status != ecstypes.TaskDefinitionStatusDeleteInProgress {
		t.Fatalf("deleted = %+v, %v", deleted, err)
	}

	// Revisions are never reused.
	if third := webTask(ctx, t, c, "web", "nginx:3"); third.Revision != 3 {
		t.Fatalf("third revision = %d", third.Revision)
	}
}

func TestECSServicesAndTasks(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ecsClient(s, "us-east-1")
	cluster := aws.String("apps")
	if _, err := c.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: cluster}); err != nil {
		t.Fatal(err)
	}

	first := webTask(ctx, t, c, "web", "nginx:1")

	t.Run("tasks", func(t *testing.T) {
		_, err := c.RunTask(ctx, &ecs.RunTaskInput{
			Cluster: cluster, TaskDefinition: aws.String("web"), LaunchType: ecstypes.LaunchTypeFargate,
		})
		code(t, err, "InvalidParameterException")
		_, err = c.RunTask(ctx, &ecs.RunTaskInput{TaskDefinition: aws.String("web"), NetworkConfiguration: awsvpc()})
		code(t, err, "ClusterNotFoundException")

		runInput := &ecs.RunTaskInput{
			Cluster: cluster, TaskDefinition: aws.String("web"), LaunchType: ecstypes.LaunchTypeFargate,
			Count: aws.Int32(2), NetworkConfiguration: awsvpc(), StartedBy: aws.String("tester"),
			ClientToken: aws.String("once"),
			Overrides: &ecstypes.TaskOverride{ContainerOverrides: []ecstypes.ContainerOverride{
				{Name: aws.String("web"), Command: []string{"serve"}},
			}},
		}
		run, err := c.RunTask(ctx, runInput)
		if err != nil {
			t.Fatal(err)
		}

		task := run.Tasks[0]
		if len(run.Tasks) != 2 || aws.ToString(task.LastStatus) != "RUNNING" ||
			task.LaunchType != ecstypes.LaunchTypeFargate ||
			aws.ToString(task.PlatformVersion) != "1.4.0" || aws.ToString(task.Group) != "family:web" ||
			aws.ToString(task.Containers[0].LastStatus) != "RUNNING" ||
			task.HealthStatus != ecstypes.HealthStatusUnknown ||
			aws.ToString(task.Cpu) != "256" {
			t.Fatalf("task = %+v", task)
		}

		again, err := c.RunTask(ctx, runInput)
		if err != nil || len(again.Tasks) != 2 || aws.ToString(again.Tasks[0].TaskArn) != aws.ToString(task.TaskArn) &&
			aws.ToString(again.Tasks[1].TaskArn) != aws.ToString(task.TaskArn) {
			t.Fatalf("repeated RunTask = %+v, %v", again, err)
		}

		runInput.Count = new(int32(1))
		_, mismatchErr := c.RunTask(ctx, runInput)
		code(t, mismatchErr, "InvalidParameterException")

		_, err = c.RunTask(ctx, &ecs.RunTaskInput{
			Cluster: cluster, TaskDefinition: aws.String("web"), LaunchType: ecstypes.LaunchTypeFargate,
			NetworkConfiguration: awsvpc(), Overrides: &ecstypes.TaskOverride{
				ContainerOverrides: []ecstypes.ContainerOverride{{Name: aws.String("sidecar")}},
			},
		})
		code(t, err, "InvalidParameterException")

		stopped, err := c.StopTask(
			ctx,
			&ecs.StopTaskInput{Cluster: cluster, Task: task.TaskArn, Reason: aws.String("done")},
		)
		if err != nil || aws.ToString(stopped.Task.LastStatus) != "STOPPED" ||
			aws.ToString(stopped.Task.StoppedReason) != "done" ||
			stopped.Task.StopCode != ecstypes.TaskStopCodeUserInitiated {
			t.Fatalf("stopped = %+v, %v", stopped, err)
		}

		runningTasks, err := c.ListTasks(ctx, &ecs.ListTasksInput{Cluster: cluster, StartedBy: aws.String("tester")})
		if err != nil || len(runningTasks.TaskArns) != 1 {
			t.Fatalf("running = %+v, %v", runningTasks, err)
		}

		stoppedTasks, err := c.ListTasks(
			ctx,
			&ecs.ListTasksInput{Cluster: cluster, DesiredStatus: ecstypes.DesiredStatusStopped},
		)
		if err != nil || len(stoppedTasks.TaskArns) != 1 || stoppedTasks.TaskArns[0] != aws.ToString(task.TaskArn) {
			t.Fatalf("stopped tasks = %+v, %v", stoppedTasks, err)
		}

		described, err := c.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: cluster, Tasks: []string{aws.ToString(task.TaskArn), "0123456789abcdef0123456789abcdef"},
		})
		if err != nil || len(described.Tasks) != 1 || len(described.Failures) != 1 {
			t.Fatalf("described = %+v, %v", described, err)
		}

		// The cluster counts the task that still runs.
		clusters, err := c.DescribeClusters(ctx, &ecs.DescribeClustersInput{Clusters: []string{"apps"}})
		if err != nil || clusters.Clusters[0].RunningTasksCount != 1 {
			t.Fatalf("cluster = %+v, %v", clusters, err)
		}

		if _, err = c.StopTask(
			ctx,
			&ecs.StopTaskInput{Cluster: cluster, Task: aws.String(runningTasks.TaskArns[0])},
		); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("services", func(t *testing.T) {
		created, err := c.CreateService(ctx, &ecs.CreateServiceInput{
			Cluster: cluster, ServiceName: aws.String("web"), TaskDefinition: first.TaskDefinitionArn,
			DesiredCount: aws.Int32(2), LaunchType: ecstypes.LaunchTypeFargate, NetworkConfiguration: awsvpc(),
			PropagateTags: ecstypes.PropagateTagsService, EnableECSManagedTags: true,
			Tags: []ecstypes.Tag{{Key: aws.String("app"), Value: aws.String("web")}},
		})
		if err != nil {
			t.Fatal(err)
		}

		svc := created.Service
		if aws.ToString(svc.ServiceArn) != "arn:aws:ecs:us-east-1:000000000000:service/apps/web" ||
			aws.ToString(svc.Status) != "ACTIVE" || svc.RunningCount != 2 || len(svc.Deployments) != 1 ||
			aws.ToString(
				svc.Deployments[0].Status,
			) != "PRIMARY" || svc.Deployments[0].RolloutState != ecstypes.DeploymentRolloutStateCompleted ||
			aws.ToString(
				svc.PlatformVersion,
			) != "LATEST" || aws.ToInt32(svc.DeploymentConfiguration.MaximumPercent) != 200 {
			t.Fatalf("service = %+v", svc)
		}

		_, err = c.CreateService(ctx, &ecs.CreateServiceInput{
			Cluster: cluster, ServiceName: aws.String("web"), TaskDefinition: aws.String("web"),
			NetworkConfiguration: awsvpc(), LaunchType: ecstypes.LaunchTypeFargate,
		})
		code(t, err, "InvalidParameterException")

		serviceTasks := func(status ecstypes.DesiredStatus) []string {
			t.Helper()
			out, listErr := c.ListTasks(ctx, &ecs.ListTasksInput{
				Cluster: cluster, ServiceName: aws.String("web"), DesiredStatus: status,
			})
			if listErr != nil {
				t.Fatal(listErr)
			}

			return out.TaskArns
		}

		original := serviceTasks(ecstypes.DesiredStatusRunning)
		if len(original) != 2 {
			t.Fatalf("service tasks = %v", original)
		}

		described, err := c.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: cluster, Tasks: original[:1], Include: []ecstypes.TaskField{ecstypes.TaskFieldTags},
		})
		if err != nil {
			t.Fatal(err)
		}

		var keys []string
		for _, tag := range described.Tasks[0].Tags {
			keys = append(keys, aws.ToString(tag.Key))
		}

		slices.Sort(keys)
		if !slices.Equal(keys, []string{"app", "aws:ecs:clusterName", "aws:ecs:serviceName"}) ||
			aws.ToString(described.Tasks[0].Group) != "service:web" {
			t.Fatalf("task tags = %v, group %s", keys, aws.ToString(described.Tasks[0].Group))
		}

		// A stopped task is replaced.
		if _, err = c.StopTask(ctx, &ecs.StopTaskInput{Cluster: cluster, Task: aws.String(original[0])}); err != nil {
			t.Fatal(err)
		}

		if replaced := serviceTasks(
			ecstypes.DesiredStatusRunning,
		); len(replaced) != 2 ||
			slices.Contains(replaced, original[0]) {
			t.Fatalf("after stop = %v", replaced)
		}

		scaled, err := c.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: cluster, Service: aws.String("web"), DesiredCount: aws.Int32(3),
		})
		if err != nil || scaled.Service.RunningCount != 3 ||
			aws.ToString(scaled.Service.Deployments[0].Id) != aws.ToString(svc.Deployments[0].Id) {
			t.Fatalf("scaled = %+v, %v", scaled, err)
		}

		second := webTask(ctx, t, c, "web", "nginx:2")
		deployed, err := c.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: cluster, Service: svc.ServiceArn, TaskDefinition: aws.String("web"),
		})
		if err != nil {
			t.Fatal(err)
		}

		if aws.ToString(deployed.Service.TaskDefinition) != aws.ToString(second.TaskDefinitionArn) ||
			aws.ToString(deployed.Service.Deployments[0].Id) == aws.ToString(svc.Deployments[0].Id) {
			t.Fatalf("deployed = %+v", deployed.Service)
		}

		current := serviceTasks(ecstypes.DesiredStatusRunning)
		tasks, err := c.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: cluster, Tasks: current})
		if err != nil || len(tasks.Tasks) != 3 {
			t.Fatalf("current = %+v, %v", tasks, err)
		}

		for _, task := range tasks.Tasks {
			if aws.ToString(task.TaskDefinitionArn) != aws.ToString(second.TaskDefinitionArn) {
				t.Fatalf("task runs %s", aws.ToString(task.TaskDefinitionArn))
			}
		}

		_, err = c.DeleteService(ctx, &ecs.DeleteServiceInput{Cluster: cluster, Service: aws.String("web")})
		code(t, err, "InvalidParameterException")
		deleted, err := c.DeleteService(ctx, &ecs.DeleteServiceInput{
			Cluster: cluster, Service: aws.String("web"), Force: aws.Bool(true),
		})
		if err != nil || aws.ToString(deleted.Service.Status) != "DRAINING" {
			t.Fatalf("deleted = %+v, %v", deleted, err)
		}

		gone, err := c.DescribeServices(
			ctx,
			&ecs.DescribeServicesInput{Cluster: cluster, Services: []string{"web", "other"}},
		)
		if err != nil || aws.ToString(gone.Services[0].Status) != "INACTIVE" || len(gone.Failures) != 1 {
			t.Fatalf("gone = %+v, %v", gone, err)
		}

		if left := serviceTasks(ecstypes.DesiredStatusRunning); len(left) != 0 {
			t.Fatalf("tasks left = %v", left)
		}

		_, err = c.UpdateService(ctx, &ecs.UpdateServiceInput{Cluster: cluster, Service: aws.String("web")})
		code(t, err, "ServiceNotActiveException")
		_, err = c.UpdateService(ctx, &ecs.UpdateServiceInput{Cluster: cluster, Service: aws.String("nope")})
		code(t, err, "ServiceNotFoundException")

		listed, err := c.ListServices(ctx, &ecs.ListServicesInput{Cluster: cluster})
		if err != nil || len(listed.ServiceArns) != 0 {
			t.Fatalf("listed = %+v, %v", listed, err)
		}
	})

	t.Run("tags", func(t *testing.T) {
		for _, arn := range []string{
			"arn:aws:ecs:us-east-1:000000000000:cluster/apps", aws.ToString(first.TaskDefinitionArn),
		} {
			if _, err := c.TagResource(ctx, &ecs.TagResourceInput{
				ResourceArn: aws.String(arn), Tags: []ecstypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
			}); err != nil {
				t.Fatal(err)
			}

			tags, err := c.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(arn)})
			if err != nil ||
				!slices.ContainsFunc(tags.Tags, func(tag ecstypes.Tag) bool { return aws.ToString(tag.Key) == "env" }) {
				t.Fatalf("%s tags = %+v, %v", arn, tags, err)
			}

			if _, err = c.UntagResource(ctx, &ecs.UntagResourceInput{
				ResourceArn: aws.String(arn), TagKeys: []string{"env"},
			}); err != nil {
				t.Fatal(err)
			}
		}

		_, err := c.TagResource(ctx, &ecs.TagResourceInput{
			ResourceArn: aws.String("arn:aws:ecs:us-east-1:000000000000:cluster/apps"),
			Tags:        []ecstypes.Tag{{Key: aws.String("aws:reserved"), Value: aws.String("x")}},
		})
		code(t, err, "InvalidParameterException")
		_, err = c.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{
			ResourceArn: aws.String("arn:aws:ecs:us-east-1:000000000000:service/apps/missing"),
		})
		code(t, err, "ResourceNotFoundException")
	})
}
