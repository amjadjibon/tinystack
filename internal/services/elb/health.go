package elb

import (
	"context"
	"regexp"
	"slices"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxTargetLength = 1024

// targetPattern is PROTOCOL:PORT with an optional path, which only HTTP and HTTPS take.
var targetPattern = regexp.MustCompile(`^(TCP|SSL|HTTP|HTTPS):(\d+)(/.*)?$`)

// defaultHealthCheck is the health check a new load balancer has: a TCP check of the first listener's instance port.
func defaultHealthCheck(listeners []api.Listener) *api.HealthCheck {
	port := common.Value(listeners[0].InstancePort)
	return &api.HealthCheck{
		Target:   new(protocolTCP + ":" + strconv.FormatInt(port, 10)),
		Interval: new(int64(30)), Timeout: new(int64(5)),
		UnhealthyThreshold: new(int64(2)), HealthyThreshold: new(int64(10)),
	}
}

// checkHealthCheck checks a complete health check: the Target format, each setting's range, and Timeout < Interval.
func checkHealthCheck(h *api.HealthCheck) error {
	target := common.Value(h.Target)
	match := targetPattern.FindStringSubmatch(target)
	if match == nil || len(target) > maxTargetLength {
		return invalid("Target must be PROTOCOL:PORT, or HTTP:PORT/PATH or HTTPS:PORT/PATH, of at most %d characters",
			maxTargetLength)
	}

	protocol, path := match[1], match[3]
	if port, err := strconv.Atoi(match[2]); err != nil || !portInRange(int64(port)) {
		return invalid("Target port must be between 1 and %d", maxPort)
	}

	if application(protocol) != (path != "") {
		return invalid("Target must include a ping path for HTTP and HTTPS, and none for TCP and SSL")
	}

	interval, timeout := common.Value(h.Interval), common.Value(h.Timeout)
	switch {
	case interval < 5 || interval > 300:
		return invalid("Interval must be between 5 and 300")
	case timeout < 2 || timeout > 60:
		return invalid("Timeout must be between 2 and 60")
	case timeout >= interval:
		return invalid("Timeout must be less than Interval")
	case common.Value(h.UnhealthyThreshold) < 2 || common.Value(h.UnhealthyThreshold) > 10:
		return invalid("UnhealthyThreshold must be between 2 and 10")
	case common.Value(h.HealthyThreshold) < 2 || common.Value(h.HealthyThreshold) > 10:
		return invalid("HealthyThreshold must be between 2 and 10")
	}

	return nil
}

func (s *Service) ConfigureHealthCheck(
	ctx context.Context,
	in *api.ConfigureHealthCheckInput,
) (*api.ConfigureHealthCheckOutput, error) {
	if in == nil || in.HealthCheck == nil {
		return nil, invalid("HealthCheck is required")
	}

	in = common.Clone(in)
	if err := checkHealthCheck(in.HealthCheck); err != nil {
		return nil, err
	}

	b, err := s.update(ctx, in.LoadBalancerName, func(_ storage.WriteTx, _ request.Scope, b *balancer) error {
		b.Description.HealthCheck = in.HealthCheck
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ConfigureHealthCheckOutput{HealthCheck: b.Description.HealthCheck}, nil
}

// instanceIDs returns the IDs of the instances, each required.
func instanceIDs(instances []api.Instance) ([]string, error) {
	ids := make([]string, 0, len(instances))
	for _, i := range instances {
		if common.Value(i.InstanceId) == "" {
			return nil, invalid("InstanceId is required")
		}

		ids = append(ids, *i.InstanceId)
	}

	return ids, nil
}

func invalidInstance(id, reason string) error {
	return awserror.New("InvalidEndPointException", "Instance "+id+" "+reason)
}

func registered(b *balancer, id string) bool {
	return slices.ContainsFunc(b.Description.Instances, func(i api.Instance) bool {
		return common.Value(i.InstanceId) == id
	})
}

// RegisterInstancesWithLoadBalancer registers EC2 instances that exist and are not terminated, and returns the
// instances registered afterwards. Registering one again changes nothing.
func (s *Service) RegisterInstancesWithLoadBalancer(
	ctx context.Context,
	in *api.RegisterEndPointsInput,
) (*api.RegisterEndPointsOutput, error) {
	if in == nil || len(in.Instances) == 0 {
		return nil, invalid("Instances is required")
	}

	ids, err := instanceIDs(in.Instances)
	if err != nil {
		return nil, err
	}

	b, err := s.update(ctx, in.LoadBalancerName, func(tx storage.WriteTx, scope request.Scope, b *balancer) error {
		for _, id := range ids {
			i, e := s.network.MountInstance(tx, scope, id)
			if e != nil {
				return remap(e, "InvalidEndPointException")
			}

			if i.State != nil && common.Value(i.State.Name) == "terminated" {
				return invalidInstance(id, "is terminated")
			}

			if !registered(b, id) {
				b.Description.Instances = append(b.Description.Instances, api.Instance{InstanceId: new(id)})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RegisterEndPointsOutput{Instances: b.Description.Instances}, nil
}

// DeregisterInstancesFromLoadBalancer removes registered instances and returns the instances still registered.
// Every instance must be registered, or nothing changes.
func (s *Service) DeregisterInstancesFromLoadBalancer(
	ctx context.Context,
	in *api.DeregisterEndPointsInput,
) (*api.DeregisterEndPointsOutput, error) {
	if in == nil || len(in.Instances) == 0 {
		return nil, invalid("Instances is required")
	}

	ids, err := instanceIDs(in.Instances)
	if err != nil {
		return nil, err
	}

	b, err := s.update(ctx, in.LoadBalancerName, func(_ storage.WriteTx, _ request.Scope, b *balancer) error {
		for _, id := range ids {
			if !registered(b, id) {
				return invalidInstance(id, "is not registered with the load balancer")
			}
		}

		b.Description.Instances = slices.DeleteFunc(b.Description.Instances, func(i api.Instance) bool {
			return slices.Contains(ids, *i.InstanceId)
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeregisterEndPointsOutput{Instances: b.Description.Instances}, nil
}

// DescribeInstanceHealth reports Unknown for registered instances, because nothing checks their health. An instance
// that exists but is not registered is OutOfService, and one EC2 does not know is invalid.
func (s *Service) DescribeInstanceHealth(
	ctx context.Context,
	in *api.DescribeEndPointStateInput,
) (*api.DescribeEndPointStateOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	ids, err := instanceIDs(in.Instances)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeEndPointStateOutput{InstanceStates: []api.InstanceState{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		b, e := read(tx, scope, common.Value(in.LoadBalancerName))
		if e != nil {
			return e
		}

		if len(ids) == 0 {
			for _, i := range b.Description.Instances {
				ids = append(ids, *i.InstanceId)
			}
		}

		for _, id := range ids {
			state := api.InstanceState{
				InstanceId: new(id), State: new("Unknown"), ReasonCode: new("N/A"), Description: new("N/A"),
			}
			if !registered(b, id) {
				if _, e = s.network.MountInstance(tx, scope, id); e != nil {
					return remap(e, "InvalidEndPointException")
				}

				state.State, state.ReasonCode = new("OutOfService"), new("Instance")
				state.Description = new("Instance is not currently registered with the LoadBalancer.")
			}

			out.InstanceStates = append(out.InstanceStates, state)
		}

		return nil
	})

	return out, err
}
