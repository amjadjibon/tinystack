package bedrockagent

import (
	"context"
	"reflect"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateAgentActionGroup stores validated configuration.
func (s *Service) CreateAgentActionGroup(
	ctx context.Context,
	in *api.CreateAgentActionGroupRequest,
) (*api.CreateAgentActionGroupResponse, error) {
	if checkErr1 := draft(in.AgentVersion); checkErr1 != nil {
		return nil, checkErr1
	}

	if _, checkErr2 := s.lookup(ctx, kindAgent, common.Value(in.AgentId)); checkErr2 != nil {
		return nil, checkErr2
	}

	if checkErr3 := s.validateGroup(
		ctx,
		in.ActionGroupExecutor,
		in.ApiSchema,
		in.FunctionSchema,
		in.ParentActionGroupSignature,
		in.Description,
	); checkErr3 != nil {
		return nil, checkErr3
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(10, common.AlphabetUpperHex)

	now := time.Now().UTC()
	value := &api.AgentActionGroup{ActionGroupId: new(id), CreatedAt: new(now), UpdatedAt: new(now)}

	value.ClientToken = in.ClientToken
	value.AgentId = in.AgentId
	value.AgentVersion = in.AgentVersion
	value.ActionGroupName = in.ActionGroupName
	value.Description = in.Description
	value.ParentActionGroupSignatureParams = in.ParentActionGroupSignatureParams
	value.ActionGroupExecutor = in.ActionGroupExecutor
	value.ApiSchema = in.ApiSchema
	value.ActionGroupState = in.ActionGroupState
	value.FunctionSchema = in.FunctionSchema
	value.ParentActionSignature = in.ParentActionGroupSignature
	value.ActionGroupState = common.OrElse(in.ActionGroupState, new(stateEnabled))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.ClientToken != nil {
			records, readErr := storage.Records[record](
				tx,
				scope.Prefix(
					serviceName,
					kindActionGroup,
				)+common.Value(
					in.AgentId,
				)+"/"+common.Value(
					in.AgentVersion,
				)+"/",
			)
			if readErr != nil {
				return readErr
			}

			for _, r := range records {
				if r.Token == *in.ClientToken {
					value = r.Group
					return nil
				}
			}
		}

		key := scope.Key(
			serviceName,
			kindActionGroup,
			common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+id,
		)
		if _, exists := tx.Get(key); exists {
			return conflict("Resource already exists.")
		}

		return storage.Write(tx, key, &record{Token: common.Value(in.ClientToken), Group: value})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAgentActionGroupResponse{AgentActionGroup: value}, nil
}

// GetAgentActionGroup reads stored configuration.
func (s *Service) GetAgentActionGroup(
	ctx context.Context,
	in *api.GetAgentActionGroupRequest,
) (*api.GetAgentActionGroupResponse, error) {
	r, err := s.lookup(
		ctx,
		kindActionGroup,
		common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+common.Value(in.ActionGroupId),
	)
	if err != nil {
		return nil, err
	}

	return &api.GetAgentActionGroupResponse{AgentActionGroup: r.Group}, nil
}

// UpdateAgentActionGroup replaces mutable configuration in place.
func (s *Service) UpdateAgentActionGroup(
	ctx context.Context,
	in *api.UpdateAgentActionGroupRequest,
) (*api.UpdateAgentActionGroupResponse, error) {
	if checkErr4 := draft(in.AgentVersion); checkErr4 != nil {
		return nil, checkErr4
	}

	if checkErr5 := s.validateGroup(
		ctx,
		in.ActionGroupExecutor,
		in.ApiSchema,
		in.FunctionSchema,
		in.ParentActionGroupSignature,
		in.Description,
	); checkErr5 != nil {
		return nil, checkErr5
	}

	r, err := s.mutate(
		ctx,
		kindActionGroup,
		common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+common.Value(in.ActionGroupId),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			r.Group.ActionGroupName = in.ActionGroupName
			r.Group.Description = in.Description
			r.Group.ParentActionGroupSignatureParams = in.ParentActionGroupSignatureParams
			r.Group.ActionGroupExecutor = in.ActionGroupExecutor
			r.Group.ActionGroupState = in.ActionGroupState
			r.Group.ApiSchema = in.ApiSchema
			r.Group.FunctionSchema = in.FunctionSchema
			r.Group.ParentActionSignature = in.ParentActionGroupSignature
			r.Group.UpdatedAt = new(time.Now().UTC())
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateAgentActionGroupResponse{AgentActionGroup: r.Group}, nil
}

// ListAgentActionGroups returns records in storage key order.
func (s *Service) ListAgentActionGroups(
	ctx context.Context,
	in *api.ListAgentActionGroupsRequest,
) (*api.ListAgentActionGroupsResponse, error) {
	if _, checkErr6 := s.lookup(ctx, kindAgent, common.Value(in.AgentId)); checkErr6 != nil {
		return nil, checkErr6
	}

	rows, err := s.rows(ctx, kindActionGroup, common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/")
	if err != nil {
		return nil, err
	}

	values := []api.ActionGroupSummary{}

	for _, r := range rows {
		values = append(
			values,
			api.ActionGroupSummary{
				ActionGroupId:    r.Group.ActionGroupId,
				ActionGroupName:  r.Group.ActionGroupName,
				ActionGroupState: r.Group.ActionGroupState,
				Description:      r.Group.Description,
				UpdatedAt:        r.Group.UpdatedAt,
			},
		)
	}

	page, next, err := common.Page(
		values,
		func(v api.ActionGroupSummary) string { return common.Value(v.ActionGroupId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListAgentActionGroupsResponse{ActionGroupSummaries: page, NextToken: next}, nil
}

// DeleteAgentActionGroup removes a local record.
func (s *Service) DeleteAgentActionGroup(
	ctx context.Context,
	in *api.DeleteAgentActionGroupRequest,
) (*api.DeleteAgentActionGroupResponse, error) {
	if checkErr7 := draft(in.AgentVersion); checkErr7 != nil {
		return nil, checkErr7
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(
			tx,
			scope,
			kindActionGroup,
			common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+common.Value(in.ActionGroupId),
		)
		if e != nil {
			return e
		}

		_ = r
		if common.Value(r.Group.ActionGroupState) == stateEnabled && !common.Value(in.SkipResourceInUseCheck) {
			return conflict("Disable the action group before deleting it.")
		}

		tx.Delete(
			scope.Key(
				serviceName,
				kindActionGroup,
				common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+common.Value(in.ActionGroupId),
			),
		)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAgentActionGroupResponse{}, nil
}

// CreateKnowledgeBase stores validated configuration.
func (s *Service) CreateKnowledgeBase(
	ctx context.Context,
	in *api.CreateKnowledgeBaseRequest,
) (*api.CreateKnowledgeBaseResponse, error) {
	if checkErr8 := s.validateKnowledgeBase(
		ctx,
		in.RoleArn,
		in.KnowledgeBaseConfiguration,
		in.StorageConfiguration,
	); checkErr8 != nil {
		return nil, checkErr8
	}

	if checkErr9 := tagging.CheckSet(in.Tags); checkErr9 != nil {
		return nil, invalid("%v", checkErr9)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(10, common.AlphabetUpperHex)

	now := time.Now().UTC()
	value := &api.KnowledgeBase{KnowledgeBaseId: new(id), CreatedAt: new(now), UpdatedAt: new(now)}

	value.Name = in.Name
	value.Description = in.Description
	value.RoleArn = in.RoleArn
	value.KnowledgeBaseConfiguration = in.KnowledgeBaseConfiguration
	value.StorageConfiguration = in.StorageConfiguration
	value.KnowledgeBaseArn = new(arn(scope, kindKnowledgeBase, id))
	value.Status = new("ACTIVE")
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.ClientToken != nil {
			records, readErr := storage.Records[record](tx, scope.Prefix(serviceName, kindKnowledgeBase)+"")
			if readErr != nil {
				return readErr
			}

			for _, r := range records {
				if r.Token == *in.ClientToken {
					value = r.KnowledgeBase
					return nil
				}
			}
		}

		key := scope.Key(serviceName, kindKnowledgeBase, id)
		if _, exists := tx.Get(key); exists {
			return conflict("Resource already exists.")
		}

		return storage.Write(tx, key, &record{Token: common.Value(in.ClientToken), KnowledgeBase: value, Tags: in.Tags})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateKnowledgeBaseResponse{KnowledgeBase: value}, nil
}

// GetKnowledgeBase reads stored configuration.
func (s *Service) GetKnowledgeBase(
	ctx context.Context,
	in *api.GetKnowledgeBaseRequest,
) (*api.GetKnowledgeBaseResponse, error) {
	r, err := s.lookup(ctx, kindKnowledgeBase, common.Value(in.KnowledgeBaseId))
	if err != nil {
		return nil, err
	}

	return &api.GetKnowledgeBaseResponse{KnowledgeBase: r.KnowledgeBase}, nil
}

// UpdateKnowledgeBase replaces mutable configuration in place.
func (s *Service) UpdateKnowledgeBase(
	ctx context.Context,
	in *api.UpdateKnowledgeBaseRequest,
) (*api.UpdateKnowledgeBaseResponse, error) {
	if checkErr10 := s.validateKnowledgeBase(
		ctx,
		in.RoleArn,
		in.KnowledgeBaseConfiguration,
		in.StorageConfiguration,
	); checkErr10 != nil {
		return nil, checkErr10
	}

	r, err := s.mutate(
		ctx,
		kindKnowledgeBase,
		common.Value(in.KnowledgeBaseId),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			// UpdateKnowledgeBase requires the original immutable configurations (AWS API reference).
			if !reflect.DeepEqual(r.KnowledgeBase.KnowledgeBaseConfiguration, in.KnowledgeBaseConfiguration) ||
				!reflect.DeepEqual(r.KnowledgeBase.StorageConfiguration, in.StorageConfiguration) {
				return invalid("knowledgeBaseConfiguration and storageConfiguration cannot be changed.")
			}

			r.KnowledgeBase.Name = in.Name
			r.KnowledgeBase.Description = in.Description
			r.KnowledgeBase.RoleArn = in.RoleArn
			r.KnowledgeBase.KnowledgeBaseConfiguration = in.KnowledgeBaseConfiguration
			r.KnowledgeBase.StorageConfiguration = in.StorageConfiguration
			r.KnowledgeBase.UpdatedAt = new(time.Now().UTC())
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateKnowledgeBaseResponse{KnowledgeBase: r.KnowledgeBase}, nil
}

// ListKnowledgeBases returns records in storage key order.
func (s *Service) ListKnowledgeBases(
	ctx context.Context,
	in *api.ListKnowledgeBasesRequest,
) (*api.ListKnowledgeBasesResponse, error) {
	rows, err := s.rows(ctx, kindKnowledgeBase, "")
	if err != nil {
		return nil, err
	}

	values := []api.KnowledgeBaseSummary{}

	for _, r := range rows {
		values = append(
			values,
			api.KnowledgeBaseSummary{
				KnowledgeBaseId: r.KnowledgeBase.KnowledgeBaseId,
				Name:            r.KnowledgeBase.Name,
				Description:     r.KnowledgeBase.Description,
				Status:          r.KnowledgeBase.Status,
				UpdatedAt:       r.KnowledgeBase.UpdatedAt,
			},
		)
	}

	page, next, err := common.Page(
		values,
		func(v api.KnowledgeBaseSummary) string { return common.Value(v.KnowledgeBaseId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListKnowledgeBasesResponse{KnowledgeBaseSummaries: page, NextToken: next}, nil
}

// DeleteKnowledgeBase removes a local record.
func (s *Service) DeleteKnowledgeBase(
	ctx context.Context,
	in *api.DeleteKnowledgeBaseRequest,
) (*api.DeleteKnowledgeBaseResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kindKnowledgeBase, common.Value(in.KnowledgeBaseId))
		if e != nil {
			return e
		}

		_ = r
		if len(tx.Keys(scope.Prefix(serviceName, kindDataSource)+common.Value(in.KnowledgeBaseId)+"/")) > 0 {
			return conflict("Knowledge base has data sources.")
		}

		tx.Delete(scope.Key(serviceName, kindKnowledgeBase, common.Value(in.KnowledgeBaseId)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteKnowledgeBaseResponse{KnowledgeBaseId: in.KnowledgeBaseId, Status: new(statusDeleting)}, nil
}

// CreateDataSource stores validated configuration.
func (s *Service) CreateDataSource(
	ctx context.Context,
	in *api.CreateDataSourceRequest,
) (*api.CreateDataSourceResponse, error) {
	if _, checkErr11 := s.lookup(ctx, kindKnowledgeBase, common.Value(in.KnowledgeBaseId)); checkErr11 != nil {
		return nil, checkErr11
	}

	if checkErr12 := s.validateDataSource(
		ctx,
		in.DataSourceConfiguration,
		in.ServerSideEncryptionConfiguration,
	); checkErr12 != nil {
		return nil, checkErr12
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(10, common.AlphabetUpperHex)

	now := time.Now().UTC()
	value := &api.DataSource{DataSourceId: new(id), CreatedAt: new(now), UpdatedAt: new(now)}

	value.KnowledgeBaseId = in.KnowledgeBaseId
	value.Name = in.Name
	value.Description = in.Description
	value.DataSourceConfiguration = in.DataSourceConfiguration
	value.DataDeletionPolicy = in.DataDeletionPolicy
	value.ServerSideEncryptionConfiguration = in.ServerSideEncryptionConfiguration
	value.VectorIngestionConfiguration = in.VectorIngestionConfiguration
	value.Status = new("AVAILABLE")
	value.DataDeletionPolicy = common.OrElse(in.DataDeletionPolicy, new("DELETE"))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.ClientToken != nil {
			records, readErr := storage.Records[record](
				tx,
				scope.Prefix(serviceName, kindDataSource)+common.Value(in.KnowledgeBaseId)+"/",
			)
			if readErr != nil {
				return readErr
			}

			for _, r := range records {
				if r.Token == *in.ClientToken {
					value = r.DataSource
					return nil
				}
			}
		}

		key := scope.Key(serviceName, kindDataSource, common.Value(in.KnowledgeBaseId)+"/"+id)
		if _, exists := tx.Get(key); exists {
			return conflict("Resource already exists.")
		}

		return storage.Write(tx, key, &record{Token: common.Value(in.ClientToken), DataSource: value})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDataSourceResponse{DataSource: value}, nil
}

// GetDataSource reads stored configuration.
func (s *Service) GetDataSource(ctx context.Context, in *api.GetDataSourceRequest) (*api.GetDataSourceResponse, error) {
	r, err := s.lookup(ctx, kindDataSource, common.Value(in.KnowledgeBaseId)+"/"+common.Value(in.DataSourceId))
	if err != nil {
		return nil, err
	}

	return &api.GetDataSourceResponse{DataSource: r.DataSource}, nil
}

// UpdateDataSource replaces mutable configuration in place.
func (s *Service) UpdateDataSource(
	ctx context.Context,
	in *api.UpdateDataSourceRequest,
) (*api.UpdateDataSourceResponse, error) {
	if checkErr13 := s.validateDataSource(
		ctx,
		in.DataSourceConfiguration,
		in.ServerSideEncryptionConfiguration,
	); checkErr13 != nil {
		return nil, checkErr13
	}

	r, err := s.mutate(
		ctx,
		kindDataSource,
		common.Value(in.KnowledgeBaseId)+"/"+common.Value(in.DataSourceId),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			r.DataSource.Name = in.Name
			r.DataSource.Description = in.Description
			r.DataSource.DataSourceConfiguration = in.DataSourceConfiguration
			r.DataSource.DataDeletionPolicy = in.DataDeletionPolicy
			r.DataSource.ServerSideEncryptionConfiguration = in.ServerSideEncryptionConfiguration
			r.DataSource.VectorIngestionConfiguration = in.VectorIngestionConfiguration
			r.DataSource.UpdatedAt = new(time.Now().UTC())
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateDataSourceResponse{DataSource: r.DataSource}, nil
}

// ListDataSources returns records in storage key order.
func (s *Service) ListDataSources(
	ctx context.Context,
	in *api.ListDataSourcesRequest,
) (*api.ListDataSourcesResponse, error) {
	if _, checkErr14 := s.lookup(ctx, kindKnowledgeBase, common.Value(in.KnowledgeBaseId)); checkErr14 != nil {
		return nil, checkErr14
	}

	rows, err := s.rows(ctx, kindDataSource, common.Value(in.KnowledgeBaseId)+"/")
	if err != nil {
		return nil, err
	}

	values := []api.DataSourceSummary{}

	for _, r := range rows {
		values = append(
			values,
			api.DataSourceSummary{
				KnowledgeBaseId: r.DataSource.KnowledgeBaseId,
				DataSourceId:    r.DataSource.DataSourceId,
				Name:            r.DataSource.Name,
				Status:          r.DataSource.Status,
				Description:     r.DataSource.Description,
				UpdatedAt:       r.DataSource.UpdatedAt,
			},
		)
	}

	page, next, err := common.Page(
		values,
		func(v api.DataSourceSummary) string { return common.Value(v.DataSourceId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListDataSourcesResponse{DataSourceSummaries: page, NextToken: next}, nil
}

// DeleteDataSource removes a local record.
func (s *Service) DeleteDataSource(
	ctx context.Context,
	in *api.DeleteDataSourceRequest,
) (*api.DeleteDataSourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kindDataSource, common.Value(in.KnowledgeBaseId)+"/"+common.Value(in.DataSourceId))
		if e != nil {
			return e
		}

		_ = r

		tx.Delete(
			scope.Key(serviceName, kindDataSource, common.Value(in.KnowledgeBaseId)+"/"+common.Value(in.DataSourceId)),
		)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDataSourceResponse{
		KnowledgeBaseId: in.KnowledgeBaseId,
		DataSourceId:    in.DataSourceId,
		Status:          new(statusDeleting),
	}, nil
}

// AssociateAgentKnowledgeBase stores validated configuration.
func (s *Service) AssociateAgentKnowledgeBase(
	ctx context.Context,
	in *api.AssociateAgentKnowledgeBaseRequest,
) (*api.AssociateAgentKnowledgeBaseResponse, error) {
	if checkErr15 := draft(in.AgentVersion); checkErr15 != nil {
		return nil, checkErr15
	}

	if _, checkErr16 := s.lookup(ctx, kindAgent, common.Value(in.AgentId)); checkErr16 != nil {
		return nil, checkErr16
	}

	if checkErr17 := s.requireKnowledgeBase(ctx, in.KnowledgeBaseId); checkErr17 != nil {
		return nil, checkErr17
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.KnowledgeBaseId)
	now := time.Now().UTC()
	value := &api.AgentKnowledgeBase{KnowledgeBaseId: new(id), CreatedAt: new(now), UpdatedAt: new(now)}

	value.AgentId = in.AgentId
	value.AgentVersion = in.AgentVersion
	value.Description = in.Description
	value.KnowledgeBaseState = in.KnowledgeBaseState
	value.KnowledgeBaseState = common.OrElse(in.KnowledgeBaseState, new(stateEnabled))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(
			serviceName,
			kindAssociation,
			common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+id,
		)
		if _, exists := tx.Get(key); exists {
			return conflict("Resource already exists.")
		}

		return storage.Write(tx, key, &record{Association: value})
	})
	if err != nil {
		return nil, err
	}

	return &api.AssociateAgentKnowledgeBaseResponse{AgentKnowledgeBase: value}, nil
}

// GetAgentKnowledgeBase reads stored configuration.
func (s *Service) GetAgentKnowledgeBase(
	ctx context.Context,
	in *api.GetAgentKnowledgeBaseRequest,
) (*api.GetAgentKnowledgeBaseResponse, error) {
	r, err := s.lookup(
		ctx,
		kindAssociation,
		common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+common.Value(in.KnowledgeBaseId),
	)
	if err != nil {
		return nil, err
	}

	return &api.GetAgentKnowledgeBaseResponse{AgentKnowledgeBase: r.Association}, nil
}

// UpdateAgentKnowledgeBase replaces mutable configuration in place.
func (s *Service) UpdateAgentKnowledgeBase(
	ctx context.Context,
	in *api.UpdateAgentKnowledgeBaseRequest,
) (*api.UpdateAgentKnowledgeBaseResponse, error) {
	if checkErr18 := draft(in.AgentVersion); checkErr18 != nil {
		return nil, checkErr18
	}

	if checkErr19 := s.requireKnowledgeBase(ctx, in.KnowledgeBaseId); checkErr19 != nil {
		return nil, checkErr19
	}

	r, err := s.mutate(
		ctx,
		kindAssociation,
		common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+common.Value(in.KnowledgeBaseId),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			r.Association.Description = in.Description
			r.Association.KnowledgeBaseState = in.KnowledgeBaseState
			r.Association.UpdatedAt = new(time.Now().UTC())
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateAgentKnowledgeBaseResponse{AgentKnowledgeBase: r.Association}, nil
}

// ListAgentKnowledgeBases returns records in storage key order.
func (s *Service) ListAgentKnowledgeBases(
	ctx context.Context,
	in *api.ListAgentKnowledgeBasesRequest,
) (*api.ListAgentKnowledgeBasesResponse, error) {
	if _, checkErr20 := s.lookup(ctx, kindAgent, common.Value(in.AgentId)); checkErr20 != nil {
		return nil, checkErr20
	}

	rows, err := s.rows(ctx, kindAssociation, common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/")
	if err != nil {
		return nil, err
	}

	values := []api.AgentKnowledgeBaseSummary{}

	for _, r := range rows {
		values = append(
			values,
			api.AgentKnowledgeBaseSummary{
				KnowledgeBaseId:    r.Association.KnowledgeBaseId,
				Description:        r.Association.Description,
				KnowledgeBaseState: r.Association.KnowledgeBaseState,
				UpdatedAt:          r.Association.UpdatedAt,
			},
		)
	}

	page, next, err := common.Page(
		values,
		func(v api.AgentKnowledgeBaseSummary) string { return common.Value(v.KnowledgeBaseId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListAgentKnowledgeBasesResponse{AgentKnowledgeBaseSummaries: page, NextToken: next}, nil
}

// DisassociateAgentKnowledgeBase removes a local record.
func (s *Service) DisassociateAgentKnowledgeBase(
	ctx context.Context,
	in *api.DisassociateAgentKnowledgeBaseRequest,
) (*api.DisassociateAgentKnowledgeBaseResponse, error) {
	if checkErr21 := draft(in.AgentVersion); checkErr21 != nil {
		return nil, checkErr21
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(
			tx,
			scope,
			kindAssociation,
			common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+common.Value(in.KnowledgeBaseId),
		)
		if e != nil {
			return e
		}

		_ = r

		tx.Delete(
			scope.Key(
				serviceName,
				kindAssociation,
				common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion)+"/"+common.Value(in.KnowledgeBaseId),
			),
		)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateAgentKnowledgeBaseResponse{}, nil
}
