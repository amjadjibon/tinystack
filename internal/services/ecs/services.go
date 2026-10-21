package ecs

import (
	"context"
	"reflect"
	"slices"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	replica   = "REPLICA"
	daemon    = "DAEMON"
	draining  = "DRAINING"
	primary   = "PRIMARY"
	maxEvents = 100
)

// ecsService is a service's record. Its only deployment is the primary one:
// a new deployment replaces the service's tasks at once.
type ecsService struct {
	Created        time.Time
	Configuration  *api.DeploymentConfiguration
	Network        *api.NetworkConfiguration
	GracePeriod    *int64
	Name           string
	Cluster        string
	Status         string
	Definition     string
	LaunchType     string
	Platform       string
	Scheduling     string
	PropagateTags  string
	Rebalancing    string
	ClientToken    string
	CreatedBy      string
	Strategy       []api.CapacityProviderStrategyItem
	Tags           []api.Tag
	Deployments    []api.Deployment
	Events         []api.ServiceEvent
	Desired        int64
	ManagedTags    bool
	ExecuteCommand bool
}

func serviceKey(scope request.Scope, clusterName, name string) string {
	return scope.Prefix(service, kindService) + clusterName + "/" + name
}

func serviceARN(scope request.Scope, clusterName, name string) string {
	return arn(scope, "service/"+clusterName+"/"+name)
}

func serviceNotFound() error {
	return awserror.New("ServiceNotFoundException", "Service not found.")
}

// loadService returns the service ref (a name or an ARN) names in c.
func loadService(tx storage.ReadTx, scope request.Scope, c *cluster, ref *string) (*ecsService, error) {
	name, ok := resourceName(scope, common.Value(ref), "service/"+c.Name+"/")
	if !ok {
		return nil, serviceNotFound()
	}

	svc, found, err := storage.Read[ecsService](tx, serviceKey(scope, c.Name, name))
	if err != nil || !found {
		return nil, common.ErrorsOr(err, serviceNotFound())
	}

	return svc, nil
}

// defaultConfiguration fills in the deployment settings ECS defaults.
func defaultConfiguration(in *api.DeploymentConfiguration) (*api.DeploymentConfiguration, error) {
	out := &api.DeploymentConfiguration{
		MaximumPercent: new(int64(200)), MinimumHealthyPercent: new(int64(100)),
		DeploymentCircuitBreaker: &api.DeploymentCircuitBreaker{Enable: new(false), Rollback: new(false)},
	}
	if in == nil {
		return out, nil
	}

	if in.MaximumPercent != nil {
		out.MaximumPercent = in.MaximumPercent
	}

	if in.MinimumHealthyPercent != nil {
		out.MinimumHealthyPercent = in.MinimumHealthyPercent
	}

	if in.DeploymentCircuitBreaker != nil {
		out.DeploymentCircuitBreaker = in.DeploymentCircuitBreaker
	}

	if *out.MinimumHealthyPercent < 0 || *out.MinimumHealthyPercent > 100 || *out.MaximumPercent < 100 ||
		*out.MaximumPercent > 200 {
		return nil, invalidParameter("minimumHealthyPercent must be 0 to 100 and maximumPercent 100 to 200.")
	}

	return out, nil
}

func (svc *ecsService) launch(def *taskDef) *launch {
	l := &launch{
		LaunchType: svc.LaunchType, Strategy: svc.Strategy, Network: svc.Network, PlatformVersion: svc.Platform,
		Group: "service:" + svc.Name, Service: svc.Name, ExecuteCommand: svc.ExecuteCommand,
	}
	switch svc.PropagateTags {
	case "SERVICE":
		l.Tags = svc.Tags
	case "TASK_DEFINITION":
		l.Tags = def.Tags
	}

	if svc.ManagedTags {
		l.Tags = mergeTags(l.Tags, []api.Tag{
			{Key: new("aws:ecs:clusterName"), Value: new(svc.Cluster)},
			{Key: new("aws:ecs:serviceName"), Value: new(svc.Name)},
		})
	}

	return l
}

// deploy makes a new primary deployment of the service's settings.
func (s *Service) deploy(svc *ecsService, l *launch) {
	now := s.now().UTC()
	id := "ecs-svc/" + digits(19)
	d := api.Deployment{
		Id: new(id), Status: new(primary), TaskDefinition: new(svc.Definition), DesiredCount: new(svc.Desired),
		PendingCount: new(int64(0)), RunningCount: new(int64(0)), FailedTasks: new(int64(0)),
		CreatedAt: seconds(now), UpdatedAt: seconds(now), NetworkConfiguration: svc.Network,
		RolloutState: new("COMPLETED"), RolloutStateReason: new("ECS deployment " + id + " completed."),
	}
	if l.LaunchType != "" {
		d.LaunchType = new(l.LaunchType)
	}

	if len(l.Strategy) > 0 {
		d.CapacityProviderStrategy = l.Strategy
	}

	if l.PlatformVersion != "" {
		d.PlatformVersion, d.PlatformFamily = new(l.PlatformVersion), new("Linux")
	}

	svc.Deployments = []api.Deployment{d}
}

// reconcile runs the primary deployment's tasks up or down to the desired
// count and stops the tasks of earlier deployments.
func (s *Service) reconcile(tx storage.WriteTx, scope request.Scope, c *cluster, svc *ecsService) error {
	if err := s.purge(tx, scope, c.Name); err != nil {
		return err
	}

	tasks, err := storage.Records[task](tx, taskPrefix(scope, c.Name))
	if err != nil {
		return err
	}

	d := &svc.Deployments[0]
	var current []*task
	for _, t := range tasks {
		switch {
		case t.Service != svc.Name || t.Desired != running:
		case t.StartedBy == *d.Id && int64(len(current)) < svc.Desired:
			current = append(current, t)
		default:
			reason := "Scaling activity initiated by (deployment " + *d.Id + ")"
			if stopErr := s.stop(tx, scope, t, "ServiceSchedulerInitiated", reason); stopErr != nil {
				return stopErr
			}
		}
	}

	def, err := resolveDef(tx, scope, &svc.Definition)
	if err != nil {
		return err
	}

	l := svc.launch(def)
	l.StartedBy = *d.Id
	if placeErr := placement(c, def, l); placeErr != nil {
		return placeErr
	}

	for int64(len(current)) < svc.Desired {
		t, startErr := s.start(tx, scope, c, def, l)
		if startErr != nil {
			return startErr
		}

		current = append(current, t)
	}

	now := s.now().UTC()
	d.DesiredCount, d.RunningCount, d.UpdatedAt = new(svc.Desired), new(int64(len(current))), seconds(now)
	svc.Events = append([]api.ServiceEvent{
		{
			Id:        new(uuid.NewV4().String()),
			CreatedAt: seconds(now),
			Message:   new("(service " + svc.Name + ") has reached a steady state."),
		},
	}, svc.Events...)
	svc.Events = svc.Events[:min(len(svc.Events), maxEvents)]
	return storage.Write(tx, serviceKey(scope, c.Name, svc.Name), svc)
}

// output describes svc with its running and pending task counts.
func (svc *ecsService) output(tx storage.ReadTx, scope request.Scope, include []string) (api.Service, error) {
	tasks, err := storage.Records[task](tx, taskPrefix(scope, svc.Cluster))
	if err != nil {
		return api.Service{}, err
	}

	var runningTasks, pendingTasks int64
	for _, t := range tasks {
		switch {
		case t.Service != svc.Name || svc.Status != active:
		case t.Last == running:
			runningTasks++
		case t.Last == pending:
			pendingTasks++
		}
	}

	out := api.Service{
		ServiceArn:              new(serviceARN(scope, svc.Cluster, svc.Name)),
		ServiceName:             new(svc.Name),
		ClusterArn:              new(clusterARN(scope, svc.Cluster)),
		Status:                  new(svc.Status),
		DesiredCount:            new(svc.Desired),
		RunningCount:            new(runningTasks),
		PendingCount:            new(pendingTasks),
		TaskDefinition:          new(svc.Definition),
		DeploymentConfiguration: svc.Configuration,
		Deployments:             svc.Deployments,
		Events:                  svc.Events,
		CreatedAt: seconds(
			svc.Created,
		),
		NetworkConfiguration:          svc.Network,
		HealthCheckGracePeriodSeconds: svc.GracePeriod,
		SchedulingStrategy:            new(svc.Scheduling),
		DeploymentController:          &api.DeploymentController{Type: new("ECS")},
		CreatedBy:                     new(svc.CreatedBy),
		EnableECSManagedTags:          new(svc.ManagedTags),
		PropagateTags:                 new(svc.PropagateTags),
		EnableExecuteCommand:          new(svc.ExecuteCommand),
		AvailabilityZoneRebalancing:   new(svc.Rebalancing),
	}
	if svc.LaunchType != "" {
		out.LaunchType = new(svc.LaunchType)
	}

	if len(svc.Strategy) > 0 {
		out.CapacityProviderStrategy = svc.Strategy
	}

	// Fargate services report the platform version they asked for.
	if platform := svc.Platform; platform != "" || svc.LaunchType == fargate || len(svc.Strategy) > 0 {
		if platform == "" {
			platform = "LATEST"
		}

		out.PlatformVersion, out.PlatformFamily = new(platform), new("Linux")
	}

	if slices.Contains(include, fieldTags) {
		out.Tags = append([]api.Tag{}, svc.Tags...)
	}

	return out, nil
}

func checkServiceSettings(propagate, rebalancing *string) error {
	if !oneOf(propagate, "TASK_DEFINITION", "SERVICE", "NONE") || !oneOf(rebalancing, "ENABLED", "DISABLED") {
		return invalidParameter("propagateTags must be TASK_DEFINITION, SERVICE, or NONE, and " +
			"availabilityZoneRebalancing ENABLED or DISABLED.")
	}

	return nil
}

func (s *Service) CreateService(ctx context.Context, in *api.CreateServiceRequest) (*api.CreateServiceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	svc, err := newService(in)
	if err != nil {
		return nil, err
	}

	svc.CreatedBy = request.CallerARN(ctx)

	var out api.Service
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.Cluster)
		if loadErr != nil {
			return loadErr
		}

		svc.Cluster, svc.Created = c.Name, s.now().UTC()
		existing, ok, readErr := storage.Read[ecsService](tx, serviceKey(scope, c.Name, svc.Name))
		switch {
		case readErr != nil:
			return readErr
		case ok && existing.Status != inactive && svc.ClientToken != "" && existing.ClientToken == svc.ClientToken:
			out, readErr = existing.output(tx, scope, []string{fieldTags})
			return readErr
		case ok && existing.Status != inactive:
			return invalidParameter("Creation of service was not idempotent.")
		}

		def, defErr := activeDef(tx, scope, in.TaskDefinition)
		if defErr != nil {
			return defErr
		}

		svc.Definition = *def.Def.TaskDefinitionArn
		l := svc.launch(def)
		if placeErr := placement(c, def, l); placeErr != nil {
			return placeErr
		}

		if svc.Scheduling == daemon && l.LaunchType != ec2 {
			return invalidParameter("The daemon scheduling strategy is only supported for the EC2 launch type.")
		}

		s.deploy(svc, l)
		if reconcileErr := s.reconcile(tx, scope, c, svc); reconcileErr != nil {
			return reconcileErr
		}

		out, readErr = svc.output(tx, scope, []string{fieldTags})
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateServiceResponse{Service: &out}, nil
}

// newService checks a CreateService request and returns the service's record.
func newService(in *api.CreateServiceRequest) (*ecsService, error) {
	if err := checkName(in.ServiceName, "serviceName"); err != nil {
		return nil, err
	}

	switch {
	case in.TaskDefinition == nil:
		return nil, invalidParameter("TaskDefinition must be specified.")
	case in.DeploymentController != nil && common.Value(in.DeploymentController.Type) != "ECS":
		return nil, invalidParameter("TinyStack supports only the ECS deployment controller.")
	case !oneOf(in.SchedulingStrategy, replica, daemon):
		return nil, invalidParameter("schedulingStrategy must be REPLICA or DAEMON.")
	case common.Value(in.SchedulingStrategy) == daemon && common.Value(in.DesiredCount) > 0:
		return nil, invalidParameter("The daemon scheduling strategy does not support a desired count for services.")
	case common.Value(in.DesiredCount) < 0 || common.Value(in.HealthCheckGracePeriodSeconds) < 0:
		return nil, invalidParameter("desiredCount and healthCheckGracePeriodSeconds cannot be negative.")
	case len(in.VpcLatticeConfigurations) > 0:
		return nil, invalidParameter("TinyStack does not support VPC Lattice configurations.")
	}

	if err := checkServiceSettings(in.PropagateTags, in.AvailabilityZoneRebalancing); err != nil {
		return nil, err
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	configuration, err := defaultConfiguration(in.DeploymentConfiguration)
	if err != nil {
		return nil, err
	}

	svc := &ecsService{
		Name:          *in.ServiceName,
		Status:        active,
		Desired:       common.Value(in.DesiredCount),
		LaunchType:    common.Value(in.LaunchType),
		Strategy:      in.CapacityProviderStrategy,
		Platform:      common.Value(in.PlatformVersion),
		Configuration: configuration,
		Network:       in.NetworkConfiguration,
		GracePeriod:   in.HealthCheckGracePeriodSeconds,
		Scheduling:    replica,
		Tags:          in.Tags,
		ManagedTags:   common.Value(in.EnableECSManagedTags),
		PropagateTags: "NONE",
		ExecuteCommand: common.Value(
			in.EnableExecuteCommand,
		),
		Rebalancing: "DISABLED",
		ClientToken: common.Value(in.ClientToken),
	}
	if in.SchedulingStrategy != nil {
		svc.Scheduling = *in.SchedulingStrategy
	}

	if in.PropagateTags != nil {
		svc.PropagateTags = *in.PropagateTags
	}

	if in.AvailabilityZoneRebalancing != nil {
		svc.Rebalancing = *in.AvailabilityZoneRebalancing
	}

	return svc, nil
}

func (s *Service) DescribeServices(
	ctx context.Context,
	in *api.DescribeServicesRequest,
) (*api.DescribeServicesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Services) < 1 || len(in.Services) > 10 {
		return nil, invalidParameter("services must name 1 to 10 services.")
	}

	if fieldErr := checkTaskFields(in.Include); fieldErr != nil {
		return nil, fieldErr
	}

	out := &api.DescribeServicesResponse{Services: []api.Service{}, Failures: []api.Failure{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.Cluster)
		if loadErr != nil {
			return loadErr
		}

		for _, ref := range in.Services {
			svc, findErr := loadService(tx, scope, c, &ref)
			if findErr != nil {
				name, _ := resourceName(scope, ref, "service/"+c.Name+"/")
				out.Failures = append(out.Failures, api.Failure{
					Arn: new(serviceARN(scope, c.Name, name)), Reason: new(missing),
				})
				continue
			}

			described, outErr := svc.output(tx, scope, in.Include)
			if outErr != nil {
				return outErr
			}

			out.Services = append(out.Services, described)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// changeService runs fn on an active service and reconciles its tasks.
func (s *Service) changeService(
	ctx context.Context,
	clusterRef, ref *string,
	fn func(storage.WriteTx, request.Scope, *cluster, *ecsService) error,
) (api.Service, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return api.Service{}, err
	}

	var out api.Service
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, clusterRef)
		if loadErr != nil {
			return loadErr
		}

		svc, findErr := loadService(tx, scope, c, ref)
		if findErr != nil {
			return findErr
		}

		if svc.Status != active {
			return awserror.New("ServiceNotActiveException", "Service was not ACTIVE.")
		}

		if fnErr := fn(tx, scope, c, svc); fnErr != nil {
			return fnErr
		}

		if svc.Status == active {
			if reconcileErr := s.reconcile(tx, scope, c, svc); reconcileErr != nil {
				return reconcileErr
			}
		}

		out, findErr = svc.output(tx, scope, []string{fieldTags})
		return findErr
	})

	return out, err
}

func (s *Service) UpdateService(ctx context.Context, in *api.UpdateServiceRequest) (*api.UpdateServiceResponse, error) {
	if common.Value(in.DesiredCount) < 0 || common.Value(in.HealthCheckGracePeriodSeconds) < 0 {
		return nil, invalidParameter("desiredCount and healthCheckGracePeriodSeconds cannot be negative.")
	}

	if len(in.VpcLatticeConfigurations) > 0 {
		return nil, invalidParameter("TinyStack does not support VPC Lattice configurations.")
	}

	if err := checkServiceSettings(in.PropagateTags, in.AvailabilityZoneRebalancing); err != nil {
		return nil, err
	}

	out, err := s.changeService(ctx, in.Cluster, in.Service,
		func(tx storage.WriteTx, scope request.Scope, c *cluster, svc *ecsService) error {
			return s.update(tx, scope, c, svc, in)
		})
	if err != nil {
		return nil, err
	}

	return &api.UpdateServiceResponse{Service: &out}, nil
}

// update applies an UpdateService request to svc, deploying again when its
// tasks would change.
func (s *Service) update(
	tx storage.WriteTx,
	scope request.Scope,
	c *cluster,
	svc *ecsService,
	in *api.UpdateServiceRequest,
) error {
	redeploy := common.Value(in.ForceNewDeployment)
	if in.DesiredCount != nil {
		if svc.Scheduling == daemon && *in.DesiredCount > 0 {
			return invalidParameter("The daemon scheduling strategy does not support a desired count for services.")
		}

		svc.Desired = *in.DesiredCount
	}

	if in.TaskDefinition != nil {
		def, err := activeDef(tx, scope, in.TaskDefinition)
		if err != nil {
			return err
		}

		redeploy = redeploy || *def.Def.TaskDefinitionArn != svc.Definition
		svc.Definition = *def.Def.TaskDefinitionArn
	}

	if in.NetworkConfiguration != nil {
		redeploy = redeploy || !reflect.DeepEqual(in.NetworkConfiguration, svc.Network)
		svc.Network = in.NetworkConfiguration
	}

	if in.PlatformVersion != nil {
		redeploy = redeploy || *in.PlatformVersion != svc.Platform
		svc.Platform = *in.PlatformVersion
	}

	if in.CapacityProviderStrategy != nil {
		if svc.LaunchType != "" {
			return invalidParameter("A service with a launch type cannot switch to a capacity provider strategy.")
		}

		redeploy = redeploy || !reflect.DeepEqual(in.CapacityProviderStrategy, svc.Strategy)
		svc.Strategy = in.CapacityProviderStrategy
	}

	if in.DeploymentConfiguration != nil {
		configuration, err := defaultConfiguration(in.DeploymentConfiguration)
		if err != nil {
			return err
		}

		svc.Configuration = configuration
	}

	svc.GracePeriod = common.OrElse(in.HealthCheckGracePeriodSeconds, svc.GracePeriod)
	svc.ExecuteCommand = common.Value(common.OrElse(in.EnableExecuteCommand, &svc.ExecuteCommand))
	svc.ManagedTags = common.Value(common.OrElse(in.EnableECSManagedTags, &svc.ManagedTags))
	svc.PropagateTags = common.Value(common.OrElse(in.PropagateTags, &svc.PropagateTags))
	svc.Rebalancing = common.Value(common.OrElse(in.AvailabilityZoneRebalancing, &svc.Rebalancing))

	def, err := resolveDef(tx, scope, &svc.Definition)
	if err != nil {
		return err
	}

	l := svc.launch(def)
	if placeErr := placement(c, def, l); placeErr != nil {
		return placeErr
	}

	if redeploy {
		s.deploy(svc, l)
	}

	return nil
}

// DeleteService stops a service's tasks and makes it INACTIVE; it reports
// DRAINING, as AWS does while the tasks stop.
func (s *Service) DeleteService(ctx context.Context, in *api.DeleteServiceRequest) (*api.DeleteServiceResponse, error) {
	out, err := s.changeService(ctx, in.Cluster, in.Service,
		func(tx storage.WriteTx, scope request.Scope, c *cluster, svc *ecsService) error {
			if svc.Desired > 0 && svc.Scheduling == replica && !common.Value(in.Force) {
				return invalidParameter("The service cannot be stopped while it is scaled above 0.")
			}

			svc.Desired, svc.Status = 0, inactive
			tasks, err := storage.Records[task](tx, taskPrefix(scope, c.Name))
			if err != nil {
				return err
			}

			for _, t := range tasks {
				if t.Service == svc.Name {
					if stopErr := s.stop(tx, scope, t, "ServiceSchedulerInitiated", "Service deleted"); stopErr != nil {
						return stopErr
					}
				}
			}

			return storage.Write(tx, serviceKey(scope, c.Name, svc.Name), svc)
		})
	if err != nil {
		return nil, err
	}

	out.Status = new(draining)
	return &api.DeleteServiceResponse{Service: &out}, nil
}

func (s *Service) ListServices(ctx context.Context, in *api.ListServicesRequest) (*api.ListServicesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !oneOf(in.SchedulingStrategy, replica, daemon) {
		return nil, invalidParameter("schedulingStrategy must be REPLICA or DAEMON.")
	}

	n, err := limit(in.MaxResults, 10)
	if err != nil {
		return nil, err
	}

	out := &api.ListServicesResponse{ServiceArns: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.Cluster)
		if loadErr != nil {
			return loadErr
		}

		services, readErr := storage.Records[ecsService](tx, scope.Prefix(service, kindService)+c.Name+"/")
		if readErr != nil {
			return readErr
		}

		services = slices.DeleteFunc(services, func(svc *ecsService) bool {
			return svc.Status != active || in.LaunchType != nil && svc.LaunchType != *in.LaunchType ||
				in.SchedulingStrategy != nil && svc.Scheduling != *in.SchedulingStrategy
		})
		services, out.NextToken, readErr = common.Page(services, func(svc *ecsService) string { return svc.Name },
			in.NextToken, n, errPageToken)
		for _, svc := range services {
			out.ServiceArns = append(out.ServiceArns, serviceARN(scope, c.Name, svc.Name))
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
