package codeconnections

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeconnections"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type hostRecord struct {
	tagSet
	Vpc          *api.VpcConfiguration
	ID           string
	Name         string
	ProviderType string
	Endpoint     string
}

func (r *hostRecord) output(scope request.Scope) api.Host {
	return api.Host{
		Name:             new(r.Name),
		HostArn:          new(arn(scope, kindHost, r.ID)),
		ProviderType:     new(r.ProviderType),
		ProviderEndpoint: new(r.Endpoint),
		VpcConfiguration: common.Clone(r.Vpc),
		Status:           new(statusPending),
	}
}

func checkVpc(vpc *api.VpcConfiguration) error {
	if vpc == nil {
		return nil
	}

	if vpc.VpcId == nil || len(vpc.SubnetIds) == 0 || len(vpc.SubnetIds) > 10 || len(vpc.SecurityGroupIds) == 0 ||
		len(vpc.SecurityGroupIds) > 10 {
		return awserror.Validation("VpcConfiguration needs a VpcId, 1 to 10 SubnetIds and 1 to 10 SecurityGroupIds.")
	}

	return nil
}

func (s *Service) CreateHost(ctx context.Context, in *api.CreateHostInput) (*api.CreateHostOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := checkLength("Name", in.Name, 64)
	if err != nil {
		return nil, err
	}

	endpoint, err := checkLength("ProviderEndpoint", in.ProviderEndpoint, 512)
	if err != nil {
		return nil, err
	}

	if common.Value(in.ProviderType) != hostProvider {
		return nil, awserror.Validation("ProviderType must be %s for a host.", hostProvider)
	}

	if e := checkVpc(in.VpcConfiguration); e != nil {
		return nil, e
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &hostRecord{
		ID: newID(), Name: name, ProviderType: hostProvider, Endpoint: endpoint,
		Vpc: common.Clone(in.VpcConfiguration), tagSet: tagSet{Tags: tags},
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key(scope, kindHost, r.ID), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateHostOutput{HostArn: new(arn(scope, kindHost, r.ID)), Tags: tagList(r.Tags)}, nil
}

func (s *Service) GetHost(ctx context.Context, in *api.GetHostInput) (*api.GetHostOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := expect(scope, kindHost, in.HostArn)
	if err != nil {
		return nil, err
	}

	var r *hostRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var e error
		if r, found, e = storage.Read[hostRecord](tx, key(scope, kindHost, id)); e != nil {
			return e
		}

		if !found {
			return notFound(kindHost, *in.HostArn)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	h := r.output(scope)

	return &api.GetHostOutput{
		Name: h.Name, Status: h.Status, ProviderType: h.ProviderType, ProviderEndpoint: h.ProviderEndpoint,
		VpcConfiguration: h.VpcConfiguration,
	}, nil
}

func (s *Service) UpdateHost(ctx context.Context, in *api.UpdateHostInput) (*api.UpdateHostOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := expect(scope, kindHost, in.HostArn)
	if err != nil {
		return nil, err
	}

	if in.ProviderEndpoint == nil && in.VpcConfiguration == nil {
		return nil, awserror.Validation("ProviderEndpoint or VpcConfiguration is required.")
	}

	if e := checkVpc(in.VpcConfiguration); e != nil {
		return nil, e
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[hostRecord](tx, key(scope, kindHost, id))
		if e != nil {
			return e
		}

		if !found {
			return notFound(kindHost, *in.HostArn)
		}

		if in.ProviderEndpoint != nil {
			if r.Endpoint, e = checkLength("ProviderEndpoint", in.ProviderEndpoint, 512); e != nil {
				return e
			}
		}

		if in.VpcConfiguration != nil {
			r.Vpc = common.Clone(in.VpcConfiguration)
		}

		return storage.Write(tx, key(scope, kindHost, id), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateHostOutput{}, nil
}

func (s *Service) DeleteHost(ctx context.Context, in *api.DeleteHostInput) (*api.DeleteHostOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := expect(scope, kindHost, in.HostArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(key(scope, kindHost, id)); !found {
			return notFound(kindHost, *in.HostArn)
		}

		connections, e := storage.Records[connectionRecord](tx, scope.Prefix(serviceName, kindConnection))
		if e != nil {
			return e
		}

		if slices.ContainsFunc(connections, func(c *connectionRecord) bool { return c.HostArn == *in.HostArn }) {
			return fault("ResourceUnavailableException", "Delete the connections of host %s first.", *in.HostArn)
		}

		tx.Delete(key(scope, kindHost, id))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteHostOutput{}, nil
}

func (s *Service) ListHosts(ctx context.Context, in *api.ListHostsInput) (*api.ListHostsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var items []*hostRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		items, e = storage.Records[hostRecord](tx, scope.Prefix(serviceName, kindHost))
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items, func(r *hostRecord) string { return r.ID }, in.NextToken, limit, invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListHostsOutput{NextToken: next, Hosts: []api.Host{}}
	for _, r := range page {
		out.Hosts = append(out.Hosts, r.output(scope))
	}

	return out, nil
}
