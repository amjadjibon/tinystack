package codeconnections

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeconnections"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type connectionRecord struct {
	tagSet
	ID           string
	Name         string
	ProviderType string
	HostArn      string
}

func (r *connectionRecord) output(scope request.Scope) api.Connection {
	return api.Connection{
		ConnectionName:   new(r.Name),
		ConnectionArn:    new(arn(scope, kindConnection, r.ID)),
		ProviderType:     new(r.ProviderType),
		OwnerAccountId:   new(scope.AccountID),
		ConnectionStatus: new(statusPending),
		HostArn:          optional(r.HostArn),
	}
}

// newID returns a UUID-shaped identifier, as AWS gives connections and hosts.
func newID() string {
	return common.RandomID(8) + "-" + common.RandomID(4) + "-4" + common.RandomID(3) + "-a" + common.RandomID(3) + "-" +
		common.RandomID(12)
}

func (s *Service) CreateConnection(
	ctx context.Context,
	in *api.CreateConnectionInput,
) (*api.CreateConnectionOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := checkLength("ConnectionName", in.ConnectionName, 32)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	provider := common.Value(in.ProviderType)
	if in.HostArn == nil && provider == "" {
		return nil, awserror.Validation("ProviderType is required when HostArn is not given.")
	}

	if provider != "" {
		if e := checkProvider(provider); e != nil {
			return nil, e
		}
	}

	r := &connectionRecord{ID: newID(), Name: name, ProviderType: provider, tagSet: tagSet{Tags: tags}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.HostArn != nil {
			id, e := expect(scope, kindHost, in.HostArn)
			if e != nil {
				return e
			}

			host, found, e := storage.Read[hostRecord](tx, key(scope, kindHost, id))
			if e != nil {
				return e
			}

			if !found {
				return notFound(kindHost, *in.HostArn)
			}

			if provider != "" && provider != host.ProviderType {
				return awserror.Validation(
					"ProviderType %s does not match the host's provider %s.", provider, host.ProviderType)
			}

			r.ProviderType, r.HostArn = host.ProviderType, *in.HostArn
		}

		return storage.Write(tx, key(scope, kindConnection, r.ID), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateConnectionOutput{ConnectionArn: new(arn(scope, kindConnection, r.ID)), Tags: tagList(r.Tags)}, nil
}

func (s *Service) GetConnection(ctx context.Context, in *api.GetConnectionInput) (*api.GetConnectionOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := expect(scope, kindConnection, in.ConnectionArn)
	if err != nil {
		return nil, err
	}

	var r *connectionRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var e error
		if r, found, e = storage.Read[connectionRecord](tx, key(scope, kindConnection, id)); e != nil {
			return e
		}

		if !found {
			return notFound(kindConnection, *in.ConnectionArn)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := r.output(scope)

	return &api.GetConnectionOutput{Connection: &out}, nil
}

func (s *Service) DeleteConnection(
	ctx context.Context,
	in *api.DeleteConnectionInput,
) (*api.DeleteConnectionOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := expect(scope, kindConnection, in.ConnectionArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(key(scope, kindConnection, id)); !found {
			return notFound(kindConnection, *in.ConnectionArn)
		}

		tx.Delete(key(scope, kindConnection, id))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteConnectionOutput{}, nil
}

func (s *Service) ListConnections(
	ctx context.Context,
	in *api.ListConnectionsInput,
) (*api.ListConnectionsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if in.ProviderTypeFilter != nil {
		if e := checkProvider(*in.ProviderTypeFilter); e != nil {
			return nil, e
		}
	}

	var items []*connectionRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := storage.Records[connectionRecord](tx, scope.Prefix(serviceName, kindConnection))
		for _, r := range all {
			if in.ProviderTypeFilter != nil && r.ProviderType != *in.ProviderTypeFilter ||
				in.HostArnFilter != nil && r.HostArn != *in.HostArnFilter {
				continue
			}

			items = append(items, r)
		}

		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items, func(r *connectionRecord) string { return r.ID }, in.NextToken, limit, invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListConnectionsOutput{NextToken: next, Connections: []api.Connection{}}
	for _, r := range page {
		out.Connections = append(out.Connections, r.output(scope))
	}

	return out, nil
}
