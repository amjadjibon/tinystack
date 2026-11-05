// Package globalaccelerator implements an account-global, records-only control plane.
// Mutations return IN_PROGRESS snapshots and atomically store DEPLOYED. No traffic
// is routed, DNS records published, flow logs delivered, or health checks performed.
package globalaccelerator

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/globalaccelerator"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/elbv2"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName       = "globalaccelerator"
	kindAccelerator   = "accelerator"
	kindListener      = "listener"
	kindEndpointGroup = "endpointgroup"
)

// Service stores Global Accelerator configuration and checks local endpoint references.
type Service struct {
	store     storage.Store
	network   *ec2.Service
	balancers *elbv2.Service
	buckets   *s3.Service
}

// New returns a records-only Global Accelerator service.
func New(store storage.Store, network *ec2.Service, balancers *elbv2.Service, buckets *s3.Service) *Service {
	return &Service{store: store, network: network, balancers: balancers, buckets: buckets}
}

type record struct {
	Tags          map[string]string
	Accelerator   *api.Accelerator
	Attributes    *api.AcceleratorAttributes
	Listener      *api.Listener
	EndpointGroup *api.EndpointGroup
	ARN           string
	Kind          string
	Parent        string
	Slot          int
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidArgumentException", fmt.Sprintf(format, args...))
}

func missing(kind string) error {
	names := map[string]string{
		kindAccelerator: "Accelerator", kindListener: "Listener", kindEndpointGroup: "EndpointGroup",
	}

	name := names[kind]
	return awserror.New(name+"NotFoundException", name+" does not exist in this account")
}

func read(tx storage.ReadTx, scope request.Scope, arn, kind string) (*record, error) {
	if !strings.HasPrefix(arn, "arn:aws:globalaccelerator::"+scope.AccountID+":accelerator/") {
		return nil, missing(kind)
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, arn))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing(kind)
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ARN), r)
}

func (s *Service) get(ctx context.Context, arn *string, kind string) (*record, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = read(tx, scope, common.Value(arn), kind)
		return e
	})
	return r, err
}

func (s *Service) change(ctx context.Context, arn *string, kind string,
	change func(storage.WriteTx, request.Scope, *record) error,
) (*record, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, common.Value(arn), kind)
		if e != nil {
			return e
		}

		if e := change(tx, scope, r); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
	return r, err
}

func (s *Service) insert(ctx context.Context, r *record, token *string, input any,
	check func(storage.WriteTx, request.Scope, *record, []*record) error,
) (*record, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	key := idempotency.Key(scope, serviceName, r.Kind+"/"+r.Parent, token)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		all, e := storage.Records[record](tx, scope.Prefix(serviceName, r.Kind))
		if e != nil {
			return e
		}

		previous, found, replayErr := idempotency.Replay[*record](
			tx,
			key,
			input,
			invalid("Idempotency token was used with different parameters"),
		)
		if replayErr != nil {
			return replayErr
		}

		if found {
			r = previous
			return nil
		}

		if e := check(tx, scope, r, all); e != nil {
			return e
		}

		if writeErr := save(tx, scope, r); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, key, input, r)
	})
	return r, err
}

func (s *Service) list(
	ctx context.Context,
	kind, parent string,
	maxResults *int64,
	token *string,
) ([]*record, *string, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	var items []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if parent != "" {
			parentKind := kindAccelerator
			if kind == kindEndpointGroup {
				parentKind = kindListener
			}

			if _, e := read(tx, scope, parent, parentKind); e != nil {
				return e
			}
		}

		all, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return e
		}

		for _, r := range all {
			if parent == "" || r.Parent == parent {
				items = append(items, r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	limit := 100
	if maxResults != nil {
		limit = int(*maxResults)
	}

	return common.Page(items, func(r *record) string { return r.ARN }, token, limit, invalid("Invalid NextToken"))
}

func (s *Service) remove(ctx context.Context, arn *string, kind string) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, common.Value(arn), kind)
		if e != nil {
			return e
		}

		if r.Accelerator != nil && common.Value(r.Accelerator.Enabled) {
			return awserror.New("AcceleratorNotDisabledException", "Disable the accelerator before deleting it")
		}

		childKind, code := kindListener, "AssociatedListenerFoundException"
		if kind == kindListener {
			childKind, code = kindEndpointGroup, "AssociatedEndpointGroupFoundException"
		}

		if kind != kindEndpointGroup {
			children, e := storage.Records[record](tx, scope.Prefix(serviceName, childKind))
			if e != nil {
				return e
			}

			for _, child := range children {
				if child.Parent == r.ARN {
					return awserror.New(code, "Remove dependent resources before deleting")
				}
			}
		}

		tx.Delete(scope.Key(serviceName, kind, r.ARN))
		return nil
	})
}

var (
	_ api.API           = (*Service)(nil)
	_ catalog.Inventory = (*Service)(nil)
)
