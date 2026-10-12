package elb

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	defaultDrainingTimeout = 300
	defaultIdleTimeout     = 60
	defaultEmitInterval    = 60
	maxTimeout             = 3600
)

func defaultAttributes() api.LoadBalancerAttributes {
	return api.LoadBalancerAttributes{
		CrossZoneLoadBalancing: &api.CrossZoneLoadBalancing{Enabled: new(false)},
		AccessLog:              &api.AccessLog{Enabled: new(false)},
		ConnectionDraining: &api.ConnectionDraining{
			Enabled: new(false),
			Timeout: new(int64(defaultDrainingTimeout)),
		},
		ConnectionSettings: &api.ConnectionSettings{IdleTimeout: new(int64(defaultIdleTimeout))},
	}
}

// mergeAttributes returns current with each attribute group in change applied. A group that is switched off drops the
// settings that depend on it, so no stale value remains.
func mergeAttributes(current, change api.LoadBalancerAttributes) (api.LoadBalancerAttributes, error) {
	if c := change.CrossZoneLoadBalancing; c != nil {
		if c.Enabled == nil {
			return current, invalid("CrossZoneLoadBalancing.Enabled is required")
		}

		current.CrossZoneLoadBalancing = c
	}

	if l := change.AccessLog; l != nil {
		if l.Enabled == nil {
			return current, invalid("AccessLog.Enabled is required")
		}

		if !*l.Enabled {
			l = &api.AccessLog{Enabled: l.Enabled}
		} else if common.Value(l.S3BucketName) == "" {
			return current, configuration("AccessLog.S3BucketName is required to enable access logs")
		} else if l.EmitInterval = common.OrElse(l.EmitInterval, new(int64(defaultEmitInterval))); *l.EmitInterval != 5 &&
			*l.EmitInterval != 60 {
			return current, invalid("AccessLog.EmitInterval must be 5 or 60")
		}

		current.AccessLog = l
	}

	if d := change.ConnectionDraining; d != nil {
		if d.Enabled == nil {
			return current, invalid("ConnectionDraining.Enabled is required")
		}

		if !*d.Enabled {
			d = &api.ConnectionDraining{Enabled: d.Enabled, Timeout: new(int64(defaultDrainingTimeout))}
		} else if d.Timeout = common.OrElse(d.Timeout, new(int64(defaultDrainingTimeout))); *d.Timeout < 1 ||
			*d.Timeout > maxTimeout {
			return current, invalid("ConnectionDraining.Timeout must be between 1 and %d", maxTimeout)
		}

		current.ConnectionDraining = d
	}

	if c := change.ConnectionSettings; c != nil {
		if timeout := common.Value(c.IdleTimeout); timeout < 1 || timeout > maxTimeout {
			return current, invalid("ConnectionSettings.IdleTimeout must be between 1 and %d", maxTimeout)
		}

		current.ConnectionSettings = c
	}

	return current, nil
}

func (s *Service) DescribeLoadBalancerAttributes(
	ctx context.Context,
	in *api.DescribeLoadBalancerAttributesInput,
) (*api.DescribeLoadBalancerAttributesOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	var b *balancer
	err = s.store.View(ctx, func(tx storage.ReadTx) (e error) {
		b, e = read(tx, scope, common.Value(in.LoadBalancerName))
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeLoadBalancerAttributesOutput{LoadBalancerAttributes: &b.Attributes}, nil
}

func (s *Service) ModifyLoadBalancerAttributes(
	ctx context.Context,
	in *api.ModifyLoadBalancerAttributesInput,
) (*api.ModifyLoadBalancerAttributesOutput, error) {
	if in == nil || in.LoadBalancerAttributes == nil {
		return nil, invalid("LoadBalancerAttributes is required")
	}

	in = common.Clone(in)
	b, err := s.update(ctx, in.LoadBalancerName, func(_ storage.WriteTx, _ request.Scope, b *balancer) (e error) {
		b.Attributes, e = mergeAttributes(b.Attributes, *in.LoadBalancerAttributes)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyLoadBalancerAttributesOutput{
		LoadBalancerName: in.LoadBalancerName, LoadBalancerAttributes: &b.Attributes,
	}, nil
}
