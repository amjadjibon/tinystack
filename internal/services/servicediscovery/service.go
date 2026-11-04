// Package servicediscovery implements regional Cloud Map records and HTTP discovery.
// DNS namespace/service settings are records only; no hosted zones or DNS records are created.
package servicediscovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicediscovery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName   = "servicediscovery"
	kindNamespace = "namespace"
	kindService   = "service"
	kindOperation = "operation"
	healthy       = "HEALTHY"
	unhealthy     = "UNHEALTHY"
)

// Records: <account>/<region>/servicediscovery/<namespace|service|operation>/<id>.
// Instances and custom health are embedded in the service record; tags in each resource record.
type record struct {
	Tags      map[string]string
	Namespace *api.Namespace
	Service   *api.Service
	Operation *api.Operation
	Instances map[string]*api.Instance
	Health    map[string]string
	ID        string
	Kind      string
	VPC       string
	Revision  int64
}

type Service struct{ store storage.Store }

var _ api.API = (*Service)(nil)

func New(store storage.Store) *Service { return &Service{store: store} }
func invalid(message string) error     { return awserror.New("InvalidInput", message) }

func duplicateRequest() error {
	return awserror.New("DuplicateRequest", "Token already used with different input")
}

func resourceARN(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:servicediscovery:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

func read(tx storage.ReadTx, scope request.Scope, kind, id string) (*record, error) {
	r, found, err := storage.Read[record](tx, scope.Key(serviceName, kind, id))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, awserror.New(
			map[string]string{
				kindNamespace: "NamespaceNotFound",
				kindService:   "ServiceNotFound",
				kindOperation: "OperationNotFound",
			}[kind],
			"Resource not found: "+id,
		)
	}

	return r, nil
}

func (s *Service) view(
	ctx context.Context,
	kind, id string,
	f func(*record, storage.ReadTx, request.Scope) error,
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		r, txErr := read(tx, scope, kind, id)
		if txErr != nil {
			return txErr
		}

		return f(r, tx, scope)
	})
}

func (s *Service) change(
	ctx context.Context,
	kind, id string,
	f func(*record, storage.WriteTx, request.Scope) error,
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, txErr := read(tx, scope, kind, id)
		if txErr != nil {
			return txErr
		}

		if checkErr := f(r, tx, scope); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, scope.Key(serviceName, kind, id), r)
	})
}

func page[T any](items []T, id func(T) string, token *string, maximum *int64) (selected []T, next *string, err error) {
	limit := int(common.Value(common.OrElse(maximum, new(int64(100)))))
	if limit < 1 || limit > 100 {
		return nil, nil, invalid("MaxResults must be 1 through 100")
	}

	return common.Page(items, id, token, limit, invalid("Invalid NextToken"))
}

func tags(in []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		in,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tags need Key and Value"),
		func(string) error { return invalid("Duplicate tag key") },
	)
	if err != nil {
		return nil, err
	}

	if checkErr := tagging.CheckSet(out); checkErr != nil {
		return nil, invalid(checkErr.Error())
	}

	return out, nil
}

func operation(tx storage.WriteTx, scope request.Scope, typ string, targets map[string]string) (*string, error) {
	id := common.RandomID(32)
	now := common.SecondsMilli(time.Now())
	r := &record{
		ID:   id,
		Kind: kindOperation,
		Operation: &api.Operation{
			Id:           new(id),
			OwnerAccount: new(scope.AccountID),
			Type:         new(typ),
			Status:       new("SUCCESS"),
			CreateDate:   now,
			UpdateDate:   now,
			Targets:      targets,
		},
	}

	return new(id), storage.Write(tx, scope.Key(serviceName, kindOperation, id), r)
}

func validName(name string) error {
	if strings.TrimSpace(name) == "" || len(name) > 1024 {
		return invalid("Name is required and must be at most 1024 characters")
	}

	return nil
}

func (s *Service) createNamespace(
	ctx context.Context,
	name, description, token *string,
	typ, vpc string,
	inTags []api.Tag,
) (*string, error) {
	if checkErr := validName(common.Value(name)); checkErr != nil {
		return nil, checkErr
	}

	if typ == "DNS_PRIVATE" && vpc == "" {
		return nil, invalid("Vpc required")
	}

	ts, err := tags(inTags)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var op *string
	input := map[string]any{"Name": name, "Description": description, "Type": typ, "Vpc": vpc, "Tags": ts}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, replayed, txErr := idempotency.Replay[string](
			tx, idempotency.Key(scope, serviceName, "CreateNamespace", token), input, duplicateRequest(),
		)
		if txErr != nil {
			return txErr
		}

		if replayed {
			op = new(previous)
			return nil
		}

		items, txErr := storage.Records[record](tx, scope.Prefix(serviceName, kindNamespace))
		if txErr != nil {
			return txErr
		}

		for _, x := range items {
			if common.Value(x.Namespace.Name) == *name {
				return awserror.New("NamespaceAlreadyExists", "Namespace already exists")
			}
		}

		id := "ns-" + common.RandomID(16)
		r := &record{
			ID:   id,
			Kind: kindNamespace,
			Tags: ts,
			VPC:  vpc,
			Namespace: &api.Namespace{
				Id:               new(id),
				Arn:              new(resourceARN(scope, kindNamespace, id)),
				Name:             name,
				Description:      description,
				Type:             new(typ),
				ServiceCount:     new(int64(0)),
				CreateDate:       common.SecondsMilli(time.Now()),
				CreatorRequestId: token,
				Properties:       &api.NamespaceProperties{HttpProperties: &api.HttpProperties{HttpName: name}},
			},
		}

		if checkErr := storage.Write(tx, scope.Key(serviceName, kindNamespace, id), r); checkErr != nil {
			return checkErr
		}

		op, txErr = operation(tx, scope, "CREATE_NAMESPACE", map[string]string{targetNamespace: id})
		if txErr != nil {
			return txErr
		}

		return idempotency.Remember(tx, idempotency.Key(scope, serviceName, "CreateNamespace", token), input, *op)
	})
	return op, err
}

func (s *Service) CreateHttpNamespace(
	ctx context.Context,
	in *api.CreateHttpNamespaceRequest,
) (*api.CreateHttpNamespaceResponse, error) {
	op, err := s.createNamespace(ctx, in.Name, in.Description, in.CreatorRequestId, httpType, "", in.Tags)
	return &api.CreateHttpNamespaceResponse{OperationId: op}, err
}

func (s *Service) CreatePrivateDnsNamespace(
	ctx context.Context,
	in *api.CreatePrivateDnsNamespaceRequest,
) (*api.CreatePrivateDnsNamespaceResponse, error) {
	op, err := s.createNamespace(
		ctx,
		in.Name,
		in.Description,
		in.CreatorRequestId,
		"DNS_PRIVATE",
		common.Value(in.Vpc),
		in.Tags,
	)
	return &api.CreatePrivateDnsNamespaceResponse{OperationId: op}, err
}

func (s *Service) CreatePublicDnsNamespace(
	ctx context.Context,
	in *api.CreatePublicDnsNamespaceRequest,
) (*api.CreatePublicDnsNamespaceResponse, error) {
	op, err := s.createNamespace(ctx, in.Name, in.Description, in.CreatorRequestId, "DNS_PUBLIC", "", in.Tags)
	return &api.CreatePublicDnsNamespaceResponse{OperationId: op}, err
}

func (s *Service) GetNamespace(ctx context.Context, in *api.GetNamespaceRequest) (*api.GetNamespaceResponse, error) {
	out := &api.GetNamespaceResponse{}
	err := s.view(
		ctx,
		kindNamespace,
		common.Value(in.Id),
		func(r *record, _ storage.ReadTx, _ request.Scope) error { out.Namespace = r.Namespace; return nil },
	)
	return out, err
}

func (s *Service) GetOperation(ctx context.Context, in *api.GetOperationRequest) (*api.GetOperationResponse, error) {
	out := &api.GetOperationResponse{}
	err := s.view(
		ctx,
		kindOperation,
		common.Value(in.OperationId),
		func(r *record, _ storage.ReadTx, _ request.Scope) error { out.Operation = r.Operation; return nil },
	)
	return out, err
}

func (s *Service) DeleteNamespace(
	ctx context.Context,
	in *api.DeleteNamespaceRequest,
) (*api.DeleteNamespaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DeleteNamespaceResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, txErr := read(tx, scope, kindNamespace, common.Value(in.Id))
		if txErr != nil {
			return txErr
		}

		if common.Value(r.Namespace.ServiceCount) > 0 {
			return awserror.New("ResourceInUse", "Namespace has services")
		}

		tx.Delete(scope.Key(serviceName, kindNamespace, r.ID))
		out.OperationId, txErr = operation(tx, scope, "DELETE_NAMESPACE", map[string]string{targetNamespace: r.ID})
		return txErr
	})
	return out, err
}

func (s *Service) UpdateHttpNamespace(
	ctx context.Context,
	in *api.UpdateHttpNamespaceRequest,
) (*api.UpdateHttpNamespaceResponse, error) {
	if in.Namespace == nil {
		return nil, invalid("Namespace required")
	}

	out := &api.UpdateHttpNamespaceResponse{}
	err := s.change(
		ctx,
		kindNamespace,
		common.Value(in.Id),
		func(r *record, tx storage.WriteTx, scope request.Scope) error {
			previous, replayed, txErr := idempotency.Replay[string](
				tx,
				idempotency.Key(scope, serviceName, "UpdateHttpNamespace", in.UpdaterRequestId),
				in,
				duplicateRequest(),
			)
			if txErr != nil {
				return txErr
			}

			if replayed {
				out.OperationId = new(previous)
				return nil
			}

			if common.Value(r.Namespace.Type) != httpType {
				return invalid("Namespace is not HTTP")
			}

			if in.Namespace.Description != nil {
				r.Namespace.Description = in.Namespace.Description
			}

			out.OperationId, txErr = operation(tx, scope, "UPDATE_NAMESPACE", map[string]string{targetNamespace: r.ID})
			if txErr != nil {
				return txErr
			}
			return idempotency.Remember(
				tx,
				idempotency.Key(scope, serviceName, "UpdateHttpNamespace", in.UpdaterRequestId),
				in,
				*out.OperationId,
			)
		},
	)
	return out, err
}

const (
	targetService   = "SERVICE"
	targetNamespace = "NAMESPACE"
	httpType        = "HTTP"
	healthyOrAll    = "HEALTHY_OR_ELSE_ALL"
)
