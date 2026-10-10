package servicediscovery

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicediscovery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkDNS(config *api.DnsConfig) error {
	if config == nil {
		return nil
	}

	if len(config.DnsRecords) == 0 {
		return invalid("DnsRecords required")
	}

	routing := common.Value(config.RoutingPolicy)
	if routing != "" && routing != "MULTIVALUE" && routing != "WEIGHTED" {
		return invalid("Invalid RoutingPolicy")
	}

	for _, r := range config.DnsRecords {
		if !slices.Contains([]string{"A", "AAAA", "SRV", "CNAME"}, common.Value(r.Type)) || r.TTL == nil || *r.TTL < 0 {
			return invalid("DNS records require a supported Type and nonnegative TTL")
		}
	}

	return nil
}

func (s *Service) CreateService(ctx context.Context, in *api.CreateServiceRequest) (*api.CreateServiceResponse, error) {
	if checkErr := validName(common.Value(in.Name)); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkDNS(in.DnsConfig); checkErr != nil {
		return nil, checkErr
	}

	if in.HealthCheckCustomConfig != nil && in.HealthCheckCustomConfig.FailureThreshold != nil &&
		*in.HealthCheckCustomConfig.FailureThreshold != 1 {
		return nil, invalid("Custom FailureThreshold must be 1")
	}

	typ := common.Value(in.Type)
	if typ != "" && typ != httpType {
		return nil, invalid("Type must be HTTP when explicitly supplied")
	}

	nsID := common.Value(in.NamespaceId)
	if in.DnsConfig != nil && in.DnsConfig.NamespaceId != nil {
		if nsID != "" && nsID != *in.DnsConfig.NamespaceId {
			return nil, invalid("NamespaceId conflicts with DnsConfig.NamespaceId")
		}

		nsID = *in.DnsConfig.NamespaceId
	}

	ts, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.CreateServiceResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, txErr := replay(tx, scope, "CreateService", in.CreatorRequestId, in)
		if txErr != nil {
			return txErr
		}

		if previous != "" {
			r, readErr := read(tx, scope, kindService, previous)
			if readErr != nil {
				return readErr
			}

			out.Service = r.Service
			return nil
		}

		ns, txErr := read(tx, scope, kindNamespace, nsID)
		if txErr != nil {
			return txErr
		}

		items, txErr := storage.Records[record](tx, scope.Prefix(serviceName, kindService))
		if txErr != nil {
			return txErr
		}

		for _, x := range items {
			if common.Value(x.Service.NamespaceId) == nsID && common.Value(x.Service.Name) == *in.Name {
				return awserror.New("ServiceAlreadyExists", "Service already exists")
			}
		}

		if common.Value(ns.Namespace.Type) == httpType && in.DnsConfig != nil {
			return invalid("HTTP namespaces cannot have DNS configuration")
		}

		if typ == "" {
			typ = httpType
			if in.DnsConfig != nil {
				typ = "DNS_HTTP"
			}
		}

		id := "srv-" + common.RandomID(16)
		r := &record{
			ID:        id,
			Kind:      kindService,
			Tags:      ts,
			Instances: map[string]*api.Instance{},
			Health:    map[string]string{},
			Revision:  1,
			Service: &api.Service{
				Id:                      new(id),
				Arn:                     new(resourceARN(scope, kindService, id)),
				Name:                    in.Name,
				NamespaceId:             new(nsID),
				Description:             in.Description,
				Type:                    new(typ),
				DnsConfig:               in.DnsConfig,
				HealthCheckCustomConfig: in.HealthCheckCustomConfig,
				CreateDate:              common.SecondsMilli(time.Now()),
				CreatorRequestId:        in.CreatorRequestId,
				InstanceCount:           new(int64(0)),
			},
		}

		out.Service = r.Service
		ns.Namespace.ServiceCount = new(common.Value(ns.Namespace.ServiceCount) + 1)
		if checkErr := storage.Write(tx, scope.Key(serviceName, kindNamespace, nsID), ns); checkErr != nil {
			return checkErr
		}

		if checkErr := storage.Write(tx, scope.Key(serviceName, kindService, id), r); checkErr != nil {
			return checkErr
		}

		return remember(tx, scope, "CreateService", in.CreatorRequestId, in, id)
	})
	return out, err
}

func (s *Service) GetService(ctx context.Context, in *api.GetServiceRequest) (*api.GetServiceResponse, error) {
	out := &api.GetServiceResponse{}
	err := s.view(
		ctx,
		kindService,
		common.Value(in.Id),
		func(r *record, _ storage.ReadTx, _ request.Scope) error { out.Service = r.Service; return nil },
	)
	return out, err
}

func (s *Service) UpdateService(ctx context.Context, in *api.UpdateServiceRequest) (*api.UpdateServiceResponse, error) {
	if in.Service == nil {
		return nil, invalid("Service required")
	}

	out := &api.UpdateServiceResponse{}
	err := s.change(
		ctx,
		kindService,
		common.Value(in.Id),
		func(r *record, tx storage.WriteTx, scope request.Scope) error {
			if in.Service.Description != nil {
				r.Service.Description = in.Service.Description
			}

			if in.Service.DnsConfig != nil {
				if r.Service.DnsConfig == nil {
					return invalid("Service has no DNS configuration")
				}

				if checkErr := checkDNS(&api.DnsConfig{DnsRecords: in.Service.DnsConfig.DnsRecords}); checkErr != nil {
					return checkErr
				}

				if len(in.Service.DnsConfig.DnsRecords) != len(r.Service.DnsConfig.DnsRecords) {
					return invalid("Cannot change DNS record types")
				}

				for i, d := range in.Service.DnsConfig.DnsRecords {
					if common.Value(d.Type) != common.Value(r.Service.DnsConfig.DnsRecords[i].Type) {
						return invalid("Cannot change DNS record types")
					}
				}

				r.Service.DnsConfig.DnsRecords = in.Service.DnsConfig.DnsRecords
			}

			var txErr error
			out.OperationId, txErr = operation(tx, scope, "UPDATE_SERVICE", map[string]string{targetService: r.ID})
			return txErr
		},
	)
	return out, err
}

func (s *Service) DeleteService(ctx context.Context, in *api.DeleteServiceRequest) (*api.DeleteServiceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, txErr := read(tx, scope, kindService, common.Value(in.Id))
		if txErr != nil {
			return txErr
		}

		if len(r.Instances) > 0 {
			return awserror.New("ResourceInUse", "Service has instances")
		}

		ns, txErr := read(tx, scope, kindNamespace, common.Value(r.Service.NamespaceId))
		if txErr != nil {
			return txErr
		}

		ns.Namespace.ServiceCount = new(common.Value(ns.Namespace.ServiceCount) - 1)
		if checkErr := storage.Write(tx, scope.Key(serviceName, kindNamespace, ns.ID), ns); checkErr != nil {
			return checkErr
		}

		tx.Delete(scope.Key(serviceName, kindService, r.ID))
		return nil
	})
	return &api.DeleteServiceResponse{}, err
}

// Filters are explicit: unknown names/conditions fail rather than broadening a result.
func match(value string, values []string, condition *string) (bool, error) {
	c := common.Value(common.OrElse(condition, new("EQ")))
	if len(values) != 1 {
		return false, invalid("Filters require exactly one value")
	}

	switch c {
	case "EQ":
		return value == values[0], nil
	case "BEGINS_WITH":
		return strings.HasPrefix(value, values[0]), nil
	case "IN":
		return value == values[0], nil
	default:
		return false, awserror.New("NotImplemented", "Unsupported filter condition: "+c)
	}
}

func (s *Service) ListNamespaces(
	ctx context.Context,
	in *api.ListNamespacesRequest,
) (*api.ListNamespacesResponse, error) {
	for _, f := range in.Filters {
		if !slices.Contains([]string{filterType, "NAME", "HTTP_NAME"}, common.Value(f.Name)) {
			return nil, awserror.New("NotImplemented", "Unsupported filter name")
		}
		if _, err := match("", f.Values, f.Condition); err != nil {
			return nil, err
		}
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListNamespacesResponse{Namespaces: []api.NamespaceSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		items, txErr := storage.Records[record](tx, scope.Prefix(serviceName, kindNamespace))
		if txErr != nil {
			return txErr
		}

		kept := []*record{}
		for _, r := range items {
			ok := true
			for _, f := range in.Filters {
				value := ""
				switch common.Value(f.Name) {
				case filterType:
					value = common.Value(r.Namespace.Type)
				case "NAME":
					value = common.Value(r.Namespace.Name)
				case "HTTP_NAME":
					value = common.Value(r.Namespace.Properties.HttpProperties.HttpName)
				default:
					return awserror.New("NotImplemented", "Unsupported namespace filter")
				}

				matches, e := match(value, f.Values, f.Condition)
				if e != nil {
					return e
				}

				ok = ok && matches
			}

			if ok {
				kept = append(kept, r)
			}
		}

		kept, out.NextToken, txErr = page(kept, func(r *record) string { return r.ID }, in.NextToken, in.MaxResults)
		if txErr != nil {
			return txErr
		}

		for _, r := range kept {
			n := r.Namespace
			out.Namespaces = append(
				out.Namespaces,
				api.NamespaceSummary{
					Id:           n.Id,
					Arn:          n.Arn,
					Name:         n.Name,
					Type:         n.Type,
					Description:  n.Description,
					ServiceCount: n.ServiceCount,
					Properties:   n.Properties,
					CreateDate:   n.CreateDate,
				},
			)
		}

		return nil
	})
	return out, err
}

func (s *Service) ListServices(ctx context.Context, in *api.ListServicesRequest) (*api.ListServicesResponse, error) {
	for _, f := range in.Filters {
		if !slices.Contains([]string{filterNamespaceID}, common.Value(f.Name)) {
			return nil, awserror.New("NotImplemented", "Unsupported filter name")
		}
		if _, err := match("", f.Values, f.Condition); err != nil {
			return nil, err
		}
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListServicesResponse{Services: []api.ServiceSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		items, txErr := storage.Records[record](tx, scope.Prefix(serviceName, kindService))
		if txErr != nil {
			return txErr
		}

		kept := []*record{}
		for _, r := range items {
			ok := true
			for _, f := range in.Filters {
				if common.Value(f.Name) != filterNamespaceID {
					return awserror.New("NotImplemented", "Only NAMESPACE_ID service filter supported")
				}

				m, e := match(common.Value(r.Service.NamespaceId), f.Values, f.Condition)
				if e != nil {
					return e
				}

				ok = ok && m
			}

			if ok {
				kept = append(kept, r)
			}
		}

		kept, out.NextToken, txErr = page(kept, func(r *record) string { return r.ID }, in.NextToken, in.MaxResults)
		if txErr != nil {
			return txErr
		}

		for _, r := range kept {
			v := r.Service
			out.Services = append(
				out.Services,
				api.ServiceSummary{
					Id:                      v.Id,
					Arn:                     v.Arn,
					Name:                    v.Name,
					Type:                    v.Type,
					Description:             v.Description,
					InstanceCount:           v.InstanceCount,
					DnsConfig:               v.DnsConfig,
					HealthCheckCustomConfig: v.HealthCheckCustomConfig,
					CreateDate:              v.CreateDate,
				},
			)
		}

		return nil
	})
	return out, err
}

func (s *Service) ListOperations(
	ctx context.Context,
	in *api.ListOperationsRequest,
) (*api.ListOperationsResponse, error) {
	for _, f := range in.Filters {
		if !slices.Contains([]string{"STATUS", filterType, filterNamespaceID, "SERVICE_ID"}, common.Value(f.Name)) {
			return nil, awserror.New("NotImplemented", "Unsupported filter name")
		}
		if _, err := match("", f.Values, f.Condition); err != nil {
			return nil, err
		}
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListOperationsResponse{Operations: []api.OperationSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		items, txErr := storage.Records[record](tx, scope.Prefix(serviceName, kindOperation))
		if txErr != nil {
			return txErr
		}

		kept := []*record{}
		for _, r := range items {
			ok := true
			for _, f := range in.Filters {
				value := ""
				switch common.Value(f.Name) {
				case "STATUS":
					value = common.Value(r.Operation.Status)
				case filterType:
					value = common.Value(r.Operation.Type)
				case filterNamespaceID:
					value = r.Operation.Targets[targetNamespace]
				case "SERVICE_ID":
					value = r.Operation.Targets[targetService]
				default:
					return awserror.New("NotImplemented", "Unsupported operation filter")
				}

				m, e := match(value, f.Values, f.Condition)
				if e != nil {
					return e
				}

				ok = ok && m
			}

			if ok {
				kept = append(kept, r)
			}
		}

		kept, out.NextToken, txErr = page(kept, func(r *record) string { return r.ID }, in.NextToken, in.MaxResults)
		if txErr != nil {
			return txErr
		}

		for _, r := range kept {
			out.Operations = append(
				out.Operations,
				api.OperationSummary{Id: r.Operation.Id, Status: r.Operation.Status},
			)
		}

		return nil
	})
	return out, err
}

const (
	filterType        = "TYPE"
	filterNamespaceID = "NAMESPACE_ID"
)
