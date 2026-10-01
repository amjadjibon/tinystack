package ecs

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	running = "RUNNING"
	stopped = "STOPPED"
	pending = "PENDING"
	ec2     = "EC2"
	// fargatePlatform is the platform version LATEST names.
	fargatePlatform = "1.4.0"
	// stoppedFor is how long a stopped task is still listed.
	stoppedFor = time.Hour
)

// task is a task's record. It settles in its desired status at once.
type task struct {
	Created          time.Time
	Stopped          time.Time `json:",omitzero"`
	Overrides        *api.TaskOverride
	Network          *api.NetworkConfiguration
	ID               string
	Cluster          string
	Definition       string
	Family           string
	Group            string
	StartedBy        string
	LaunchType       string
	PlatformVersion  string
	CapacityProvider string
	Desired          string
	Last             string
	StopCode         string
	StoppedReason    string
	Cpu              string
	Memory           string
	Zone             string
	// Service is the name of the service that started the task, if any.
	Service        string
	ClientToken    string
	Containers     []api.Container
	Tags           []api.Tag
	Version        int64
	ExecuteCommand bool
}

func taskPrefix(scope request.Scope, clusterName string) string {
	return prefix(scope, kindTask) + clusterName + "/"
}

func taskKey(scope request.Scope, clusterName, id string) string {
	return taskPrefix(scope, clusterName) + id
}

func taskARN(scope request.Scope, clusterName, id string) string {
	return arn(scope, "task/"+clusterName+"/"+id)
}

func (t *task) output(scope request.Scope, include []string) api.Task {
	out := api.Task{
		TaskArn: new(taskARN(scope, t.Cluster, t.ID)), ClusterArn: new(clusterARN(scope, t.Cluster)),
		TaskDefinitionArn: new(t.Definition), AvailabilityZone: new(t.Zone), Connectivity: new("CONNECTED"),
		ConnectivityAt: seconds(t.Created), Containers: t.Containers, CreatedAt: seconds(t.Created),
		DesiredStatus: new(t.Desired), LastStatus: new(t.Last), EnableExecuteCommand: new(t.ExecuteCommand),
		Group: new(t.Group), HealthStatus: new(taskHealth(t)), LaunchType: new(t.LaunchType),
		Overrides: t.Overrides, PullStartedAt: seconds(t.Created), PullStoppedAt: seconds(t.Created),
		StartedAt: seconds(t.Created), Version: new(t.Version),
	}
	if out.Overrides == nil {
		out.Overrides = &api.TaskOverride{ContainerOverrides: []api.ContainerOverride{}}
	}

	for _, field := range []struct {
		to   **string
		from string
	}{
		{&out.Cpu, t.Cpu},
		{&out.Memory, t.Memory},
		{&out.StartedBy, t.StartedBy},
		{&out.CapacityProviderName, t.CapacityProvider},
		{&out.PlatformVersion, t.PlatformVersion},
	} {
		if field.from != "" {
			*field.to = new(field.from)
		}
	}

	if t.PlatformVersion != "" {
		out.PlatformFamily = new("Linux")
	}

	if t.Last == stopped {
		out.StopCode, out.StoppedReason = new(t.StopCode), new(t.StoppedReason)
		out.StoppingAt, out.StoppedAt, out.ExecutionStoppedAt = seconds(t.Stopped), seconds(t.Stopped),
			seconds(t.Stopped)
	}

	if slices.Contains(include, fieldTags) {
		out.Tags = append([]api.Tag{}, t.Tags...)
	}

	return out
}

// taskHealth is HEALTHY when every container has a health check, which
// always passes, and UNKNOWN otherwise.
func taskHealth(t *task) string {
	for _, c := range t.Containers {
		if value(c.HealthStatus) != "HEALTHY" {
			return "UNKNOWN"
		}
	}

	return "HEALTHY"
}

// launch is how a task starts: from RunTask, or from a service.
type launch struct {
	Network          *api.NetworkConfiguration
	Overrides        *api.TaskOverride
	LaunchType       string
	PlatformVersion  string
	CapacityProvider string
	Group            string
	StartedBy        string
	Service          string
	ClientToken      string
	Strategy         []api.CapacityProviderStrategyItem
	Tags             []api.Tag
	ExecuteCommand   bool
}

// placement checks that def can run in c as l describes, and settles l's
// launch type, capacity provider, and platform version.
func placement(c *cluster, def *taskDef, l *launch) error {
	switch {
	case l.LaunchType != "" && len(l.Strategy) > 0:
		return invalidParameter("Specifying both a launch type and capacity provider strategy is not supported.")
	case l.LaunchType == "" && len(l.Strategy) == 0 && len(c.Strategy) > 0:
		l.Strategy = c.Strategy
	case l.LaunchType == "" && len(l.Strategy) == 0:
		l.LaunchType = ec2
	}

	if l.LaunchType != "" && l.LaunchType != ec2 && l.LaunchType != fargate && l.LaunchType != "EXTERNAL" {
		return invalidParameter("launchType must be EC2, FARGATE, or EXTERNAL.")
	}

	if len(l.Strategy) > 0 {
		if err := checkStrategy(c.CapacityProviders, l.Strategy); err != nil {
			return err
		}

		l.CapacityProvider = value(l.Strategy[0].CapacityProvider)
		for _, item := range l.Strategy {
			if value(item.Base) > 0 {
				l.CapacityProvider = value(item.CapacityProvider)
			}
		}
	}

	onFargate := l.LaunchType == fargate || l.CapacityProvider != ""
	switch {
	case onFargate && !slices.Contains(def.Def.Compatibilities, fargate):
		return clientError("Task definition does not support launch_type FARGATE.")
	case onFargate && l.PlatformVersion == "", onFargate && l.PlatformVersion == "LATEST":
		l.PlatformVersion = fargatePlatform
	case onFargate && l.PlatformVersion != fargatePlatform && l.PlatformVersion != "1.3.0":
		return awserror.New("PlatformUnknownException", "The specified platform version does not exist.")
	case !onFargate && l.PlatformVersion != "":
		return invalidParameter("A platform version can only be specified for the Fargate launch type.")
	}

	return checkNetwork(def, l.Network)
}

func checkNetwork(def *taskDef, network *api.NetworkConfiguration) error {
	vpc := network != nil && network.AwsvpcConfiguration != nil
	if value(def.Def.NetworkMode) != awsvpc {
		if vpc {
			return invalidParameter("Network Configuration is not valid for the given networkMode of this task " +
				"definition.")
		}

		return nil
	}

	if !vpc || len(network.AwsvpcConfiguration.Subnets) == 0 {
		return invalidParameter("Network Configuration must be provided when networkMode 'awsvpc' is specified.")
	}

	c := network.AwsvpcConfiguration
	if len(c.Subnets) > 16 || len(c.SecurityGroups) > 5 || !oneOf(c.AssignPublicIp, "ENABLED", "DISABLED") {
		return invalidParameter("awsvpcConfiguration takes up to 16 subnets, 5 security groups, and assignPublicIp " +
			"ENABLED or DISABLED.")
	}

	for _, id := range c.Subnets {
		if !strings.HasPrefix(id, "subnet-") {
			return invalidParameter("Error retrieving subnet information for [%s]: The subnet ID '%s' does not "+
				"exist", id, id)
		}
	}

	for _, id := range c.SecurityGroups {
		if !strings.HasPrefix(id, "sg-") {
			return invalidParameter("Error retrieving security group information for [%s]: The security group "+
				"'%s' does not exist", id, id)
		}
	}

	return nil
}

func checkOverrides(def *taskDef, o *api.TaskOverride) error {
	if o == nil {
		return nil
	}

	for _, c := range o.ContainerOverrides {
		if !slices.ContainsFunc(def.Def.ContainerDefinitions, func(d api.ContainerDefinition) bool {
			return value(d.Name) == value(c.Name)
		}) {
			return invalidParameter("Override for container named %s is not a container in the TaskDefinition.",
				value(c.Name))
		}
	}

	for _, size := range []*string{o.Cpu, o.Memory} {
		if size != nil {
			if _, ok := units(*size); !ok {
				return invalidParameter("Task overrides need numeric cpu and memory.")
			}
		}
	}

	return nil
}

// start stores a running task of def in c.
func (s *Service) start(tx storage.WriteTx, scope request.Scope, c *cluster, def *taskDef, l *launch) (*task, error) {
	now := s.now().UTC()
	t := &task{
		Created: now, ID: newID(), Cluster: c.Name, Definition: *def.Def.TaskDefinitionArn, Family: *def.Def.Family,
		Group: l.Group, StartedBy: l.StartedBy, LaunchType: l.LaunchType, PlatformVersion: l.PlatformVersion,
		CapacityProvider: l.CapacityProvider, Desired: running, Last: running, Zone: scope.Region + "a",
		Service: l.Service, ClientToken: l.ClientToken, Overrides: l.Overrides, Network: l.Network, Tags: l.Tags,
		Version: 1, ExecuteCommand: l.ExecuteCommand, Cpu: value(def.Def.Cpu), Memory: value(def.Def.Memory),
	}
	if t.Group == "" {
		t.Group = "family:" + t.Family
	}

	if t.CapacityProvider != "" {
		t.LaunchType = fargate
	}

	if o := l.Overrides; o != nil {
		t.Cpu, t.Memory = overridden(o.Cpu, t.Cpu), overridden(o.Memory, t.Memory)
	}

	// With a runtime, the runner starts the task's containers.
	if s.runtime != nil {
		t.Last = pending
		tx.Put(indexKey(scope, c.Name, t.ID), nil)
	}

	for i := range def.Def.ContainerDefinitions {
		t.Containers = append(t.Containers, container(scope, t, &def.Def.ContainerDefinitions[i]))
	}

	return t, write(tx, taskKey(scope, c.Name, t.ID), t)
}

func overridden(override *string, current string) string {
	if override == nil {
		return current
	}

	n, _ := units(*override)
	return strconv.FormatInt(n, 10)
}

// container describes a container of d in t, in t's status. A pending
// container has no runtime ID or network bindings until the runner starts it.
func container(scope request.Scope, t *task, d *api.ContainerDefinition) api.Container {
	c := api.Container{
		ContainerArn: new(arn(scope, "container/"+t.Cluster+"/"+t.ID+"/"+newUUID())),
		TaskArn:      new(taskARN(scope, t.Cluster, t.ID)), Name: d.Name, Image: d.Image,
		LastStatus: new(t.Last), HealthStatus: new("UNKNOWN"), Cpu: new(strconv.FormatInt(value(d.Cpu), 10)),
		NetworkBindings: []api.NetworkBinding{},
	}
	if d.HealthCheck != nil {
		c.HealthStatus = new("HEALTHY")
	}

	if d.Memory != nil {
		c.Memory = new(strconv.FormatInt(*d.Memory, 10))
	}

	if d.MemoryReservation != nil {
		c.MemoryReservation = new(strconv.FormatInt(*d.MemoryReservation, 10))
	}

	if t.Last == pending {
		return c
	}

	c.RuntimeId = new(t.ID + "-" + digits(10))
	// Tasks on awsvpc networks get addresses, not host bindings.
	if t.Network != nil {
		return c
	}

	for _, p := range d.PortMappings {
		// A bridge-mode port without a host port gets one from the
		// ephemeral range.
		hostPort := value(p.HostPort)
		if hostPort == 0 {
			n, _ := strconv.ParseInt(digits(4), 10, 64)
			hostPort = 32768 + n
		}

		c.NetworkBindings = append(c.NetworkBindings, api.NetworkBinding{
			BindIP: new("0.0.0.0"), ContainerPort: p.ContainerPort, HostPort: new(hostPort), Protocol: p.Protocol,
		})
	}

	return c
}

// stop stops t now, if it runs.
func (s *Service) stop(tx storage.WriteTx, scope request.Scope, t *task, code, reason string) error {
	if t.Last == stopped {
		return nil
	}

	t.Desired, t.Last, t.StopCode, t.StoppedReason = stopped, stopped, code, reason
	t.Stopped = s.now().UTC()
	t.Version++
	for i := range t.Containers {
		c := &t.Containers[i]
		// Containers that never started, or whose exit the runner recorded,
		// keep their exit code.
		if value(c.LastStatus) == running {
			c.ExitCode = new(int64(0))
		}

		c.LastStatus = new(stopped)
	}

	return write(tx, taskKey(scope, t.Cluster, t.ID), t)
}

// purge deletes the tasks of a cluster that stopped more than an hour ago.
func (s *Service) purge(tx storage.WriteTx, scope request.Scope, clusterName string) error {
	tasks, err := records[task](tx, taskPrefix(scope, clusterName))
	if err != nil {
		return err
	}

	cutoff := s.now().UTC().Add(-stoppedFor)
	for _, t := range tasks {
		if t.Last == stopped && t.Stopped.Before(cutoff) {
			tx.Delete(taskKey(scope, clusterName, t.ID))
		}
	}

	return nil
}

// replace lets the service that started t, if any, replace it.
func (s *Service) replace(tx storage.WriteTx, scope request.Scope, c *cluster, t *task) error {
	if t.Service == "" {
		return nil
	}

	svc, ok, err := read[ecsService](tx, serviceKey(scope, c.Name, t.Service))
	if err != nil || !ok || svc.Status != active {
		return err
	}

	return s.reconcile(tx, scope, c, svc)
}

func (s *Service) RunTask(ctx context.Context, in *api.RunTaskRequest) (*api.RunTaskResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	count := value(in.Count)
	if in.Count == nil {
		count = 1
	}

	switch {
	case count < 1 || count > 10:
		return nil, invalidParameter("count must be between 1 and 10.")
	case len(value(in.StartedBy)) > 128 || len(value(in.Group)) > 255:
		return nil, invalidParameter("startedBy can have up to 128 characters and group up to 255.")
	case !oneOf(in.PropagateTags, "TASK_DEFINITION", "NONE"):
		return nil, invalidParameter("RunTask propagates tags from TASK_DEFINITION or NONE.")
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	out := &api.RunTaskResponse{Tasks: []api.Task{}, Failures: []api.Failure{}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.Cluster)
		if loadErr != nil {
			return loadErr
		}

		if purgeErr := s.purge(tx, scope, c.Name); purgeErr != nil {
			return purgeErr
		}

		if reused, found, reuseErr := tokenTasks(tx, scope, c.Name, value(in.ClientToken)); reuseErr != nil || found {
			for _, t := range reused {
				out.Tasks = append(out.Tasks, t.output(scope, []string{fieldTags}))
			}

			return reuseErr
		}

		def, defErr := activeDef(tx, scope, in.TaskDefinition)
		if defErr != nil {
			return defErr
		}

		l := &launch{
			LaunchType: value(in.LaunchType), Strategy: in.CapacityProviderStrategy, Network: in.NetworkConfiguration,
			PlatformVersion: value(in.PlatformVersion), Overrides: in.Overrides, Group: value(in.Group),
			StartedBy: value(in.StartedBy), ClientToken: value(in.ClientToken), Tags: in.Tags,
			ExecuteCommand: value(in.EnableExecuteCommand),
		}
		if value(in.PropagateTags) == "TASK_DEFINITION" {
			l.Tags = mergeTags(def.Tags, in.Tags)
		}

		if value(in.EnableECSManagedTags) {
			l.Tags = mergeTags(l.Tags, []api.Tag{{Key: new("aws:ecs:clusterName"), Value: new(c.Name)}})
		}

		if placeErr := placement(c, def, l); placeErr != nil {
			return placeErr
		}

		if overrideErr := checkOverrides(def, in.Overrides); overrideErr != nil {
			return overrideErr
		}

		for range count {
			t, startErr := s.start(tx, scope, c, def, l)
			if startErr != nil {
				return startErr
			}

			out.Tasks = append(out.Tasks, t.output(scope, []string{fieldTags}))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// tokenTasks returns the tasks RunTask started with a client token, which
// repeating the call returns.
func tokenTasks(tx storage.ReadTx, scope request.Scope, clusterName, token string) ([]*task, bool, error) {
	if token == "" {
		return nil, false, nil
	}

	tasks, err := records[task](tx, taskPrefix(scope, clusterName))
	if err != nil {
		return nil, false, err
	}

	tasks = slices.DeleteFunc(tasks, func(t *task) bool { return t.ClientToken != token })
	return tasks, len(tasks) > 0, nil
}

// taskRef returns the ID of the task ref (an ID or an ARN) names.
func taskRef(scope request.Scope, clusterName, ref string) string {
	if id, ok := strings.CutPrefix(ref, arn(scope, "task/"+clusterName+"/")); ok {
		return id
	}

	// Tasks also have the old ARN form without the cluster.
	if id, ok := strings.CutPrefix(ref, arn(scope, "task/")); ok {
		return id
	}

	return ref
}

func checkTaskFields(include []string) error {
	for _, field := range include {
		if field != fieldTags {
			return invalidParameter("include value must be TAGS.")
		}
	}

	return nil
}

func (s *Service) DescribeTasks(ctx context.Context, in *api.DescribeTasksRequest) (*api.DescribeTasksResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Tasks) < 1 || len(in.Tasks) > 100 {
		return nil, invalidParameter("tasks must name 1 to 100 tasks.")
	}

	if fieldErr := checkTaskFields(in.Include); fieldErr != nil {
		return nil, fieldErr
	}

	out := &api.DescribeTasksResponse{Tasks: []api.Task{}, Failures: []api.Failure{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.Cluster)
		if loadErr != nil {
			return loadErr
		}

		for _, ref := range in.Tasks {
			id := taskRef(scope, c.Name, ref)
			t, ok, readErr := read[task](tx, taskKey(scope, c.Name, id))
			if readErr != nil {
				return readErr
			}

			if !ok || strings.Contains(id, "/") {
				out.Failures = append(
					out.Failures,
					api.Failure{Arn: new(taskARN(scope, c.Name, id)), Reason: new(missing)},
				)
				continue
			}

			out.Tasks = append(out.Tasks, t.output(scope, in.Include))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListTasks(ctx context.Context, in *api.ListTasksRequest) (*api.ListTasksResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if !oneOf(in.DesiredStatus, running, pending, stopped) {
		return nil, invalidParameter("desiredStatus must be RUNNING, PENDING, or STOPPED.")
	}

	if in.Family != nil && in.ServiceName != nil {
		return nil, invalidParameter("family and serviceName cannot be combined.")
	}

	n, err := limit(in.MaxResults, 100)
	if err != nil {
		return nil, err
	}

	status := running
	if in.DesiredStatus != nil {
		status = *in.DesiredStatus
	}

	out := &api.ListTasksResponse{TaskArns: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.Cluster)
		if loadErr != nil {
			return loadErr
		}

		tasks, readErr := records[task](tx, taskPrefix(scope, c.Name))
		if readErr != nil {
			return readErr
		}

		tasks = slices.DeleteFunc(tasks, func(t *task) bool {
			return t.Desired != status || in.Family != nil && t.Family != *in.Family ||
				in.ServiceName != nil && t.Service != *in.ServiceName ||
				in.StartedBy != nil && t.StartedBy != *in.StartedBy ||
				in.LaunchType != nil && t.LaunchType != *in.LaunchType
		})
		tasks, out.NextToken, readErr = page(tasks, func(t *task) string { return t.ID }, in.NextToken, n)
		for _, t := range tasks {
			out.TaskArns = append(out.TaskArns, taskARN(scope, c.Name, t.ID))
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) StopTask(ctx context.Context, in *api.StopTaskRequest) (*api.StopTaskResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if len(value(in.Reason)) > 1024 {
		return nil, invalidParameter("reason can have up to 1024 characters.")
	}

	var out api.Task
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.Cluster)
		if loadErr != nil {
			return loadErr
		}

		t, ok, readErr := read[task](tx, taskKey(scope, c.Name, taskRef(scope, c.Name, value(in.Task))))
		if readErr != nil || !ok {
			return errorsOr(readErr, invalidParameter("The referenced task was not found."))
		}

		reason := value(in.Reason)
		if reason == "" {
			reason = "Task stopped by user"
		}

		if stopErr := s.stop(tx, scope, t, "UserInitiated", reason); stopErr != nil {
			return stopErr
		}

		out = t.output(scope, []string{fieldTags})
		return s.replace(tx, scope, c, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.StopTaskResponse{Task: &out}, nil
}
