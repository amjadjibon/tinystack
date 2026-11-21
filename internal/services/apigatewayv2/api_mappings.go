package apigatewayv2

import (
	"context"
	"regexp"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindAPIMapping = "apimapping"

var mappingKeyPattern = regexp.MustCompile(`^[A-Za-z0-9$_.+!*'()-]+(/[A-Za-z0-9$_.+!*'()-]+)*$`)

func (s *Service) CreateApiMapping(
	ctx context.Context, in *api.CreateApiMappingRequest,
) (*api.CreateApiMappingResponse, error) {
	mapping, err := s.changeMapping(ctx, in.DomainName, nil, func(mapping *api.ApiMapping) bool {
		mapping.ApiId, mapping.ApiMappingKey, mapping.Stage = in.ApiId, in.ApiMappingKey, in.Stage
		return true
	})
	if err != nil {
		return nil, err
	}

	return (*api.CreateApiMappingResponse)(mapping), nil
}

func (s *Service) UpdateApiMapping(
	ctx context.Context, in *api.UpdateApiMappingRequest,
) (*api.UpdateApiMappingResponse, error) {
	mapping, err := s.changeMapping(ctx, in.DomainName, in.ApiMappingId, func(mapping *api.ApiMapping) bool {
		mapping.ApiId = common.OrElse(in.ApiId, mapping.ApiId)
		mapping.ApiMappingKey = common.OrElse(in.ApiMappingKey, mapping.ApiMappingKey)
		mapping.Stage = common.OrElse(in.Stage, mapping.Stage)
		return true
	})
	if err != nil {
		return nil, err
	}

	return (*api.UpdateApiMappingResponse)(mapping), nil
}

func (s *Service) DeleteApiMapping(
	ctx context.Context, in *api.DeleteApiMappingRequest,
) (*api.DeleteApiMappingOutput, error) {
	if _, err := s.changeMapping(ctx, in.DomainName, in.ApiMappingId, func(*api.ApiMapping) bool {
		return false
	}); err != nil {
		return nil, err
	}

	return &api.DeleteApiMappingOutput{}, nil
}

func (s *Service) changeMapping(
	ctx context.Context, domainName, id *string, change func(*api.ApiMapping) bool,
) (*api.ApiMapping, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var mapping *api.ApiMapping
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(domainName))
		if loadErr != nil {
			return loadErr
		}

		name := *d.DomainName
		if id == nil {
			mapping = &api.ApiMapping{ApiMappingId: new(common.RandomID(6))}
		} else {
			mapping, loadErr = loadMapping(tx, m.Scope, name, *id)
			if loadErr != nil {
				return loadErr
			}
		}

		key := owned(m.Scope, kindAPIMapping, name, *mapping.ApiMappingId)
		if !change(mapping) {
			tx.Delete(key)
			return nil
		}

		if checkErr := checkMapping(tx, m.Scope, name, mapping); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, key, mapping)
	})
	if err != nil {
		return nil, err
	}

	return mapping, nil
}

func checkMapping(tx storage.ReadTx, scope request.Scope, domain string, mapping *api.ApiMapping) error {
	key := common.Value(mapping.ApiMappingKey)
	if key != "" && (len(key) > 300 || !mappingKeyPattern.MatchString(key)) {
		return badRequest("Invalid API mapping key.")
	}

	a, err := loadAPI(tx, scope, mapping.ApiId)
	if err != nil {
		return err
	}

	if common.Value(a.ProtocolType) != protocolHTTP {
		return badRequest("Custom domains are only supported for HTTP APIs.")
	}

	if _, stageErr := loadStage(tx, scope, *a.ApiId, mapping.Stage); stageErr != nil {
		return stageErr
	}

	all, err := storage.Records[api.ApiMapping](tx, scope.Prefix(service, kindAPIMapping)+domain+"/")
	if err != nil {
		return err
	}

	for _, item := range all {
		if *item.ApiMappingId != *mapping.ApiMappingId && common.Value(item.ApiMappingKey) == key {
			return conflict("An API mapping already exists for this key.")
		}
	}

	return nil
}

func loadMapping(tx storage.ReadTx, scope request.Scope, domain, id string) (*api.ApiMapping, error) {
	mapping, found, err := storage.Read[api.ApiMapping](tx, owned(scope, kindAPIMapping, domain, id))
	if err == nil && !found {
		err = notFound("Invalid API mapping identifier specified")
	}

	return mapping, err
}

func (s *Service) GetApiMapping(
	ctx context.Context, in *api.GetApiMappingRequest,
) (*api.GetApiMappingResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var mapping *api.ApiMapping
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(in.DomainName))
		if loadErr != nil {
			return loadErr
		}

		mapping, loadErr = loadMapping(tx, m.Scope, *d.DomainName, common.Value(in.ApiMappingId))
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return (*api.GetApiMappingResponse)(mapping), nil
}

func (s *Service) GetApiMappings(
	ctx context.Context, in *api.GetApiMappingsRequest,
) (*api.GetApiMappingsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetApiMappingsResponse{Items: []api.ApiMapping{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(in.DomainName))
		if loadErr != nil {
			return loadErr
		}

		items, readErr := storage.Records[api.ApiMapping](tx, m.Prefix(service, kindAPIMapping)+*d.DomainName+"/")
		if readErr != nil {
			return readErr
		}

		selected, next, pageErr := page(items, func(item *api.ApiMapping) string { return *item.ApiMappingId },
			in.NextToken, in.MaxResults)
		out.NextToken = next
		for _, item := range selected {
			out.Items = append(out.Items, *item)
		}

		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func checkMappedStage(tx storage.ReadTx, scope request.Scope, apiID string, stage *string) error {
	items, err := storage.Records[api.ApiMapping](tx, scope.Prefix(service, kindAPIMapping))
	if err != nil {
		return err
	}

	for _, item := range items {
		if *item.ApiId == apiID && (stage == nil || *item.Stage == *stage) {
			return conflict("The API or stage is referenced by an API mapping.")
		}
	}

	return nil
}
