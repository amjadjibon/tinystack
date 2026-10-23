package globalaccelerator

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/globalaccelerator"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkListener(tx storage.ReadTx, scope request.Scope, r *record) error {
	ranges := r.Listener.PortRanges
	for i, a := range ranges {
		// Bounds are generated model validations; the model does not express the ordering or presence of both ports.
		if a.FromPort == nil || a.ToPort == nil || *a.FromPort > *a.ToPort {
			return awserror.New("InvalidPortRangeException", "Port ranges require FromPort <= ToPort")
		}

		for _, b := range ranges[:i] {
			if *a.FromPort <= *b.ToPort && *b.FromPort <= *a.ToPort {
				return awserror.New("InvalidPortRangeException", "Listener port ranges overlap")
			}
		}
	}

	all, err := storage.Records[record](tx, scope.Prefix(serviceName, kindListener))
	if err != nil {
		return err
	}

	for _, other := range all {
		if other.ARN == r.ARN || other.Parent != r.Parent ||
			common.Value(other.Listener.Protocol) != common.Value(r.Listener.Protocol) {
			continue
		}

		for _, a := range ranges {
			for _, b := range other.Listener.PortRanges {
				if *a.FromPort <= *b.ToPort && *b.FromPort <= *a.ToPort {
					return awserror.New(
						"InvalidPortRangeException",
						"Port ranges overlap another listener with the same protocol",
					)
				}
			}
		}
	}

	return nil
}

// CreateListener adds a listener with nonoverlapping port ranges.
func (s *Service) CreateListener(
	ctx context.Context,
	in *api.CreateListenerRequest,
) (*api.CreateListenerResponse, error) {
	r := &record{
		Kind: kindListener, Parent: common.Value(in.AcceleratorArn), Token: common.Value(in.IdempotencyToken),
		Listener: &api.Listener{PortRanges: in.PortRanges, Protocol: in.Protocol, ClientAffinity: in.ClientAffinity},
	}

	if r.Listener.ClientAffinity == nil {
		r.Listener.ClientAffinity = new("NONE")
	}

	r, err := s.insert(ctx, r, in, func(tx storage.WriteTx, scope request.Scope, r *record, _ []*record) error {
		if _, e := read(tx, scope, r.Parent, kindAccelerator); e != nil {
			return e
		}

		r.ARN = r.Parent + "/listener/" + common.RandomID(8, "0123456789abcdef")
		r.Listener.ListenerArn = new(r.ARN)
		return checkListener(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateListenerResponse{Listener: r.Listener}, nil
}

// DescribeListener returns the stored listener.
func (s *Service) DescribeListener(
	ctx context.Context,
	in *api.DescribeListenerRequest,
) (*api.DescribeListenerResponse, error) {
	r, err := s.get(ctx, in.ListenerArn, kindListener)
	if err != nil {
		return nil, err
	}

	return &api.DescribeListenerResponse{Listener: r.Listener}, nil
}

// ListListeners pages the listeners of an accelerator.
func (s *Service) ListListeners(ctx context.Context, in *api.ListListenersRequest) (*api.ListListenersResponse, error) {
	records, next, err := s.list(ctx, kindListener, common.Value(in.AcceleratorArn), in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.ListListenersResponse{Listeners: []api.Listener{}, NextToken: next}

	for _, r := range records {
		out.Listeners = append(out.Listeners, *r.Listener)
	}

	return out, nil
}

// UpdateListener updates listener settings after checking port conflicts.
func (s *Service) UpdateListener(
	ctx context.Context,
	in *api.UpdateListenerRequest,
) (*api.UpdateListenerResponse, error) {
	r, err := s.change(
		ctx,
		in.ListenerArn,
		kindListener,
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			if in.PortRanges != nil {
				r.Listener.PortRanges = in.PortRanges
			}

			if in.Protocol != nil {
				r.Listener.Protocol = in.Protocol
			}

			if in.ClientAffinity != nil {
				r.Listener.ClientAffinity = in.ClientAffinity
			}

			return checkListener(tx, scope, r)
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateListenerResponse{Listener: r.Listener}, nil
}

// DeleteListener removes a listener that has no endpoint groups.
func (s *Service) DeleteListener(
	ctx context.Context,
	in *api.DeleteListenerRequest,
) (*api.DeleteListenerOutput, error) {
	if err := s.remove(ctx, in.ListenerArn, kindListener); err != nil {
		return nil, err
	}

	return &api.DeleteListenerOutput{}, nil
}
