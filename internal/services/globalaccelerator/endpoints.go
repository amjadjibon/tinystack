package globalaccelerator

import (
	"context"
	"strings"

	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	elbapi "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/globalaccelerator"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// endpoints resolves resources through the owning services' exported APIs outside transactions.
func (s *Service) endpoints(
	ctx context.Context,
	region string,
	configs []api.EndpointConfiguration,
) ([]api.EndpointDescription, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	m.Region = region
	ctx = request.WithMetadata(ctx, m)
	out := make([]api.EndpointDescription, 0, len(configs))
	seen := map[string]bool{}

	for _, config := range configs {
		id := common.Value(config.EndpointId)
		if id == "" || seen[id] {
			return nil, invalid("EndpointId must identify a unique existing endpoint")
		}

		seen[id] = true
		preservation := false
		switch {
		case strings.HasPrefix(id, "arn:aws:elasticloadbalancing:"+region+":"+m.AccountID+":loadbalancer/"):
			if s.balancers == nil {
				return nil, invalid("ELBv2 service is unavailable")
			}

			result, e := s.balancers.DescribeLoadBalancers(
				ctx,
				&elbapi.DescribeLoadBalancersInput{LoadBalancerArns: []string{id}},
			)
			if e != nil {
				return nil, invalid("Endpoint load balancer must exist in the endpoint group Region: %s", e)
			}

			if len(result.LoadBalancers) != 1 {
				return nil, invalid("Endpoint load balancer does not exist")
			}

			lb := result.LoadBalancers[0]
			kind := common.Value(lb.Type)
			if kind != "application" && kind != "network" {
				return nil, invalid("Only application and network load balancers are supported")
			}

			preservation = kind == "application"
		case strings.HasPrefix(id, "eipalloc-"):
			if s.network == nil {
				return nil, invalid("EC2 service is unavailable")
			}

			if _, e := s.network.DescribeAddresses(
				ctx,
				&ec2api.DescribeAddressesRequest{AllocationIds: []string{id}},
			); e != nil {
				return nil, invalid("Endpoint Elastic IP must exist in the endpoint group Region: %s", e)
			}
		case strings.HasPrefix(id, "i-"):
			if s.network == nil {
				return nil, invalid("EC2 service is unavailable")
			}

			result, e := s.network.DescribeInstances(ctx, &ec2api.DescribeInstancesRequest{InstanceIds: []string{id}})
			if e != nil {
				return nil, invalid("Endpoint instance must exist in the endpoint group Region: %s", e)
			}

			if len(result.Reservations) == 0 {
				return nil, invalid("Endpoint instance does not exist")
			}

			for _, reservation := range result.Reservations {
				for i := range reservation.Instances {
					instance := reservation.Instances[i]
					if instance.State != nil && common.Value(instance.State.Name) != "running" {
						return nil, invalid("Endpoint instance must be running")
					}
				}
			}
		default:
			return nil, invalid(
				"Endpoint must be a local ALB/NLB ARN, EC2 instance ID or Elastic IP allocation ID in the endpoint group Region",
			)
		}

		if config.ClientIPPreservationEnabled != nil {
			preservation = *config.ClientIPPreservationEnabled
		}

		if preservation && strings.HasPrefix(id, "eipalloc-") {
			return nil, invalid("Elastic IP endpoints cannot preserve client IP addresses")
		}

		weight := int64(128)
		if config.Weight != nil {
			weight = *config.Weight
		}

		out = append(out, api.EndpointDescription{
			EndpointId: new(id), Weight: new(weight),
			ClientIPPreservationEnabled: new(preservation), HealthState: new("INITIAL"),
		})
	}

	return out, nil
}
