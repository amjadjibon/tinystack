package globalaccelerator

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/globalaccelerator"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkGroup(tx storage.ReadTx, scope request.Scope, r *record) error {
	g := r.EndpointGroup
	if n := common.Value(g.HealthCheckIntervalSeconds); n != 10 && n != 30 {
		return invalid("HealthCheckIntervalSeconds must be 10 or 30")
	}

	listener, err := read(tx, scope, r.Parent, kindListener)
	if err != nil {
		return err
	}

	seen := map[int64]bool{}

	for _, override := range g.PortOverrides {
		if override.ListenerPort == nil || override.EndpointPort == nil {
			return invalid("PortOverrides require ListenerPort and EndpointPort")
		}

		port := *override.ListenerPort
		if seen[port] {
			return invalid("Duplicate listener port override")
		}

		seen[port] = true
		found := false
		for _, ports := range listener.Listener.PortRanges {
			if port >= *ports.FromPort && port <= *ports.ToPort {
				found = true
			}
		}

		if !found {
			return invalid("PortOverride listener port must be in a listener port range")
		}
	}

	return nil
}

// CreateEndpointGroup adds one endpoint group per listener and Region.
func (s *Service) CreateEndpointGroup(
	ctx context.Context,
	in *api.CreateEndpointGroupRequest,
) (*api.CreateEndpointGroupResponse, error) {
	endpoints, err := s.endpoints(ctx, common.Value(in.EndpointGroupRegion), in.EndpointConfigurations)
	if err != nil {
		return nil, err
	}

	g := &api.EndpointGroup{
		EndpointGroupRegion:        in.EndpointGroupRegion,
		EndpointDescriptions:       endpoints,
		TrafficDialPercentage:      in.TrafficDialPercentage,
		HealthCheckPort:            in.HealthCheckPort,
		HealthCheckProtocol:        in.HealthCheckProtocol,
		HealthCheckPath:            in.HealthCheckPath,
		HealthCheckIntervalSeconds: in.HealthCheckIntervalSeconds,
		ThresholdCount:             in.ThresholdCount,
		PortOverrides:              in.PortOverrides,
	}

	if g.TrafficDialPercentage == nil {
		g.TrafficDialPercentage = new(float64(100))
	}

	if g.HealthCheckProtocol == nil {
		g.HealthCheckProtocol = new("TCP")
	}

	if g.HealthCheckPath == nil {
		g.HealthCheckPath = new("/")
	}

	if g.HealthCheckIntervalSeconds == nil {
		g.HealthCheckIntervalSeconds = new(int64(30))
	}

	if g.ThresholdCount == nil {
		g.ThresholdCount = new(int64(3))
	}

	r := &record{
		Kind:   kindEndpointGroup,
		Parent: common.Value(in.ListenerArn),

		EndpointGroup: g,
	}

	r, err = s.insert(
		ctx,
		r,
		in.IdempotencyToken,
		in,
		func(tx storage.WriteTx, scope request.Scope, r *record, all []*record) error {
			listener, e := read(tx, scope, r.Parent, kindListener)
			if e != nil {
				return e
			}

			for _, old := range all {
				if old.Parent == r.Parent &&
					common.Value(old.EndpointGroup.EndpointGroupRegion) == common.Value(g.EndpointGroupRegion) {
					return awserror.New(
						"EndpointGroupAlreadyExistsException",
						"One endpoint group per Region per listener is allowed",
					)
				}
			}

			if g.HealthCheckPort == nil {
				g.HealthCheckPort = listener.Listener.PortRanges[0].FromPort
			}

			r.ARN = r.Parent + "/endpoint-group/" + common.RandomID(8, "0123456789abcdef")
			g.EndpointGroupArn = new(r.ARN)
			return checkGroup(tx, scope, r)
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateEndpointGroupResponse{EndpointGroup: r.EndpointGroup}, nil
}

// DescribeEndpointGroup returns the stored endpoint group.
func (s *Service) DescribeEndpointGroup(
	ctx context.Context,
	in *api.DescribeEndpointGroupRequest,
) (*api.DescribeEndpointGroupResponse, error) {
	r, err := s.get(ctx, in.EndpointGroupArn, kindEndpointGroup)
	if err != nil {
		return nil, err
	}

	return &api.DescribeEndpointGroupResponse{EndpointGroup: r.EndpointGroup}, nil
}

// ListEndpointGroups pages the endpoint groups of a listener.
func (s *Service) ListEndpointGroups(
	ctx context.Context,
	in *api.ListEndpointGroupsRequest,
) (*api.ListEndpointGroupsResponse, error) {
	records, next, err := s.list(ctx, kindEndpointGroup, common.Value(in.ListenerArn), in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.ListEndpointGroupsResponse{EndpointGroups: []api.EndpointGroup{}, NextToken: next}

	for _, r := range records {
		out.EndpointGroups = append(out.EndpointGroups, *r.EndpointGroup)
	}

	return out, nil
}

// UpdateEndpointGroup updates group configuration and validates endpoint references.
func (s *Service) UpdateEndpointGroup(
	ctx context.Context,
	in *api.UpdateEndpointGroupRequest,
) (*api.UpdateEndpointGroupResponse, error) {
	current, err := s.get(ctx, in.EndpointGroupArn, kindEndpointGroup)
	if err != nil {
		return nil, err
	}

	var endpoints []api.EndpointDescription
	if in.EndpointConfigurations != nil {
		endpoints, err = s.endpoints(
			ctx,
			common.Value(current.EndpointGroup.EndpointGroupRegion),
			in.EndpointConfigurations,
		)
		if err != nil {
			return nil, err
		}
	}

	r, err := s.change(
		ctx,
		in.EndpointGroupArn,
		kindEndpointGroup,
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			g := r.EndpointGroup
			if in.EndpointConfigurations != nil {
				g.EndpointDescriptions = endpoints
			}

			if in.TrafficDialPercentage != nil {
				g.TrafficDialPercentage = in.TrafficDialPercentage
			}

			if in.HealthCheckPort != nil {
				g.HealthCheckPort = in.HealthCheckPort
			}

			if in.HealthCheckProtocol != nil {
				g.HealthCheckProtocol = in.HealthCheckProtocol
			}

			if in.HealthCheckPath != nil {
				g.HealthCheckPath = in.HealthCheckPath
			}

			if in.HealthCheckIntervalSeconds != nil {
				g.HealthCheckIntervalSeconds = in.HealthCheckIntervalSeconds
			}

			if in.ThresholdCount != nil {
				g.ThresholdCount = in.ThresholdCount
			}

			if in.PortOverrides != nil {
				g.PortOverrides = in.PortOverrides
			}

			return checkGroup(tx, scope, r)
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateEndpointGroupResponse{EndpointGroup: r.EndpointGroup}, nil
}

// DeleteEndpointGroup removes the group without modifying its endpoint resources.
func (s *Service) DeleteEndpointGroup(
	ctx context.Context,
	in *api.DeleteEndpointGroupRequest,
) (*api.DeleteEndpointGroupOutput, error) {
	if err := s.remove(ctx, in.EndpointGroupArn, kindEndpointGroup); err != nil {
		return nil, err
	}

	return &api.DeleteEndpointGroupOutput{}, nil
}

// AddEndpoints adds existing local resources as unprobed endpoints.
func (s *Service) AddEndpoints(ctx context.Context, in *api.AddEndpointsRequest) (*api.AddEndpointsResponse, error) {
	current, err := s.get(ctx, in.EndpointGroupArn, kindEndpointGroup)
	if err != nil {
		return nil, err
	}

	endpoints, err := s.endpoints(
		ctx,
		common.Value(current.EndpointGroup.EndpointGroupRegion),
		in.EndpointConfigurations,
	)
	if err != nil {
		return nil, err
	}

	r, err := s.change(
		ctx,
		in.EndpointGroupArn,
		kindEndpointGroup,
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			for _, endpoint := range endpoints {
				for _, old := range r.EndpointGroup.EndpointDescriptions {
					if common.Value(endpoint.EndpointId) == common.Value(old.EndpointId) {
						return invalid("Endpoint already exists in this endpoint group")
					}
				}
			}

			r.EndpointGroup.EndpointDescriptions = append(r.EndpointGroup.EndpointDescriptions, endpoints...)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.AddEndpointsResponse{EndpointGroupArn: new(r.ARN), EndpointDescriptions: endpoints}, nil
}

// RemoveEndpoints removes endpoint membership without deleting the underlying resources.
func (s *Service) RemoveEndpoints(
	ctx context.Context,
	in *api.RemoveEndpointsRequest,
) (*api.RemoveEndpointsOutput, error) {
	_, err := s.change(
		ctx,
		in.EndpointGroupArn,
		kindEndpointGroup,
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			remove := map[string]bool{}

			for _, endpoint := range in.EndpointIdentifiers {
				id := common.Value(endpoint.EndpointId)
				found := false
				for _, old := range r.EndpointGroup.EndpointDescriptions {
					if id == common.Value(old.EndpointId) {
						if endpoint.ClientIPPreservationEnabled != nil &&
							common.Value(old.ClientIPPreservationEnabled) != *endpoint.ClientIPPreservationEnabled {
							return invalid("Endpoint client IP preservation setting does not match")
						}

						found = true
					}
				}

				if !found {
					return invalid("Endpoint %s does not exist in this group", id)
				}

				remove[id] = true
			}

			kept := []api.EndpointDescription{}

			for _, endpoint := range r.EndpointGroup.EndpointDescriptions {
				if !remove[common.Value(endpoint.EndpointId)] {
					kept = append(kept, endpoint)
				}
			}

			r.EndpointGroup.EndpointDescriptions = kept
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.RemoveEndpointsOutput{}, nil
}
