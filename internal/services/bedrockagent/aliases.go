package bedrockagent

import (
	"context"
	"strconv"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func snapshot(
	tx storage.WriteTx,
	scope request.Scope,
	r *record,
	routes []api.AgentAliasRoutingConfigurationListItem,
) ([]api.AgentAliasRoutingConfigurationListItem, error) {
	id := common.Value(r.Agent.AgentId)
	if len(routes) > 0 {
		for _, route := range routes {
			if route.ProvisionedThroughput != nil {
				return nil, invalid("Provisioned throughput is not supported.")
			}

			if common.Value(route.AgentVersion) == versionDraft {
				return nil, invalid("Only the test alias may route to DRAFT.")
			}

			if _, checkErr1 := read(tx, scope, kindVersion, id+"/"+common.Value(route.AgentVersion)); checkErr1 != nil {
				return nil, checkErr1
			}
		}

		return routes, nil
	}

	if common.Value(r.Agent.AgentStatus) != statusPrepared {
		return nil, conflict("Prepare DRAFT before creating an agent version.")
	}

	version := strconv.Itoa(r.NextVersion)
	r.NextVersion++
	a := common.Clone(r.Agent)
	a.AgentVersion = new(version)
	if checkErr2 := storage.Write(
		tx,
		scope.Key(serviceName, kindVersion, id+"/"+version),
		&record{Version: agentVersion(a)},
	); checkErr2 != nil {
		return nil, checkErr2
	}

	for _, kind := range []string{kindActionGroup, kindAssociation} {
		prefix := scope.Prefix(serviceName, kind) + id + "/DRAFT/"
		for _, key := range tx.Keys(prefix) {
			child, ok, err := storage.Read[record](tx, key)
			if err != nil {
				return nil, err
			}

			if !ok {
				continue
			}

			if child.Group != nil {
				child.Group.AgentVersion = new(version)
			}

			if child.Association != nil {
				child.Association.AgentVersion = new(version)
			}

			target := scope.Prefix(serviceName, kind) + id + "/" + version + "/" + key[len(prefix):]
			if checkErr3 := storage.Write(tx, target, child); checkErr3 != nil {
				return nil, checkErr3
			}
		}
	}

	return []api.AgentAliasRoutingConfigurationListItem{{AgentVersion: new(version)}}, nil
}

// CreateAgentAlias snapshots prepared DRAFT when no explicit route is supplied.
func (s *Service) CreateAgentAlias(
	ctx context.Context,
	in *api.CreateAgentAliasRequest,
) (*api.CreateAgentAliasResponse, error) {
	if checkErr4 := tagging.CheckSet(in.Tags); checkErr4 != nil {
		return nil, invalid("%v", checkErr4)
	}

	var alias *api.AgentAlias
	_, err := s.mutate(
		ctx,
		kindAgent,
		common.Value(in.AgentId),
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			token := idempotency.Key(scope, serviceName, "CreateAgentAlias", in.ClientToken)
			previous, found, replayErr := idempotency.Replay[*api.AgentAlias](tx, token, in,
				conflict("ClientToken was used with different parameters."))
			if replayErr != nil || found {
				alias = previous
				return replayErr
			}

			routes, e := snapshot(tx, scope, r, in.RoutingConfiguration)
			if e != nil {
				return e
			}

			id := common.RandomID(10, common.AlphabetUpperHex)
			now := time.Now().UTC()
			alias = &api.AgentAlias{
				AgentId:              in.AgentId,
				AgentAliasId:         new(id),
				AgentAliasName:       in.AgentAliasName,
				AgentAliasArn:        new(arn(scope, "agent-alias", common.Value(in.AgentId)+"/"+id)),
				ClientToken:          in.ClientToken,
				Description:          in.Description,
				RoutingConfiguration: routes,
				AgentAliasStatus: new(
					statusCreating,
				),
				AliasInvocationState: new("ACCEPT_INVOCATIONS"),
				CreatedAt:            new(now),
				UpdatedAt:            new(now),
			}

			if e := storage.Write(
				tx,
				scope.Key(serviceName, kindAlias, common.Value(in.AgentId)+"/"+id),
				&record{Alias: alias, Tags: in.Tags},
			); e != nil {
				return e
			}

			return idempotency.Remember(tx, token, in, alias)
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateAgentAliasResponse{AgentAlias: alias}, nil
}

// GetAgentAlias observes CREATING/UPDATING before changing the stored status to PREPARED.
func (s *Service) GetAgentAlias(ctx context.Context, in *api.GetAgentAliasRequest) (*api.GetAgentAliasResponse, error) {
	var out *api.AgentAlias
	_, err := s.mutate(
		ctx,
		kindAlias,
		common.Value(in.AgentId)+"/"+common.Value(in.AgentAliasId),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			out = common.Clone(r.Alias)
			if common.Value(r.Alias.AgentAliasStatus) == statusCreating ||
				common.Value(r.Alias.AgentAliasStatus) == statusUpdating {
				r.Alias.AgentAliasStatus = new(statusPrepared)
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.GetAgentAliasResponse{AgentAlias: out}, nil
}

// UpdateAgentAlias changes routing or its mutable metadata.
func (s *Service) UpdateAgentAlias(
	ctx context.Context,
	in *api.UpdateAgentAliasRequest,
) (*api.UpdateAgentAliasResponse, error) {
	if common.Value(in.AgentAliasId) == testAliasID {
		return nil, invalid("The test alias cannot be modified.")
	}

	var out *api.AgentAlias
	_, err := s.mutate(
		ctx,
		kindAgent,
		common.Value(in.AgentId),
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			key := common.Value(in.AgentId) + "/" + common.Value(in.AgentAliasId)
			a, e := read(tx, scope, kindAlias, key)
			if e != nil {
				return e
			}

			routes, e := snapshot(tx, scope, r, in.RoutingConfiguration)
			if e != nil {
				return e
			}

			a.Alias.AgentAliasName = in.AgentAliasName
			a.Alias.Description = in.Description
			a.Alias.RoutingConfiguration = routes
			a.Alias.AliasInvocationState = common.OrElse(in.AliasInvocationState, a.Alias.AliasInvocationState)
			a.Alias.AgentAliasStatus = new(statusUpdating)
			a.Alias.UpdatedAt = new(time.Now().UTC())
			out = common.Clone(a.Alias)
			return storage.Write(tx, scope.Key(serviceName, kindAlias, key), a)
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateAgentAliasResponse{AgentAlias: out}, nil
}

// DeleteAgentAlias removes an alias; the test alias is owned by its agent.
func (s *Service) DeleteAgentAlias(
	ctx context.Context,
	in *api.DeleteAgentAliasRequest,
) (*api.DeleteAgentAliasResponse, error) {
	if common.Value(in.AgentAliasId) == testAliasID {
		return nil, invalid("The test alias cannot be deleted.")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		id := common.Value(in.AgentId) + "/" + common.Value(in.AgentAliasId)
		if _, e := read(tx, scope, kindAlias, id); e != nil {
			return e
		}

		tx.Delete(scope.Key(serviceName, kindAlias, id))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAgentAliasResponse{
		AgentId:          in.AgentId,
		AgentAliasId:     in.AgentAliasId,
		AgentAliasStatus: new(statusDeleting),
	}, nil
}

// GetAgentVersion reads an immutable numbered snapshot.
func (s *Service) GetAgentVersion(
	ctx context.Context,
	in *api.GetAgentVersionRequest,
) (*api.GetAgentVersionResponse, error) {
	if common.Value(in.AgentVersion) == versionDraft {
		r, err := s.lookup(ctx, kindAgent, common.Value(in.AgentId))
		if err != nil {
			return nil, err
		}

		return &api.GetAgentVersionResponse{AgentVersion: agentVersion(r.Agent)}, nil
	}

	r, err := s.lookup(ctx, kindVersion, common.Value(in.AgentId)+"/"+common.Value(in.AgentVersion))
	if err != nil {
		return nil, err
	}

	return &api.GetAgentVersionResponse{AgentVersion: r.Version}, nil
}

// DeleteAgentVersion enforces alias references unless explicitly skipped.
func (s *Service) DeleteAgentVersion(
	ctx context.Context,
	in *api.DeleteAgentVersionRequest,
) (*api.DeleteAgentVersionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		id := common.Value(in.AgentId) + "/" + common.Value(in.AgentVersion)
		if _, e := read(tx, scope, kindVersion, id); e != nil {
			return e
		}

		aliases, e := storage.Records[record](tx, scope.Prefix(serviceName, kindAlias)+common.Value(in.AgentId)+"/")
		if e != nil {
			return e
		}

		for _, a := range aliases {
			for _, route := range a.Alias.RoutingConfiguration {
				if common.Value(route.AgentVersion) == common.Value(in.AgentVersion) &&
					!common.Value(in.SkipResourceInUseCheck) {
					return conflict("Version is referenced by an alias.")
				}
			}
		}

		tx.Delete(scope.Key(serviceName, kindVersion, id))
		for _, kind := range []string{kindActionGroup, kindAssociation} {
			for _, key := range tx.Keys(scope.Prefix(serviceName, kind) + id + "/") {
				tx.Delete(key)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAgentVersionResponse{
		AgentId:      in.AgentId,
		AgentVersion: in.AgentVersion,
		AgentStatus:  new(statusDeleting),
	}, nil
}

// ResolveInvocation checks the alias and prepared version without advancing lifecycle states.
func (s *Service) ResolveInvocation(ctx context.Context, agentID, aliasID string) (*api.AgentVersion, error) {
	a, err := s.lookup(ctx, kindAlias, agentID+"/"+aliasID)
	if err != nil {
		return nil, err
	}

	if common.Value(a.Alias.AgentAliasStatus) != statusPrepared ||
		common.Value(a.Alias.AliasInvocationState) == "REJECT_INVOCATIONS" {
		return nil, conflict("Alias is not prepared for invocation.")
	}

	version, err := s.GetAgentVersion(
		ctx,
		&api.GetAgentVersionRequest{AgentId: new(agentID), AgentVersion: a.Alias.RoutingConfiguration[0].AgentVersion},
	)
	if err != nil {
		return nil, err
	}

	if common.Value(version.AgentVersion.AgentStatus) != statusPrepared {
		return nil, conflict("Agent version is not PREPARED.")
	}

	return version.AgentVersion, nil
}

// ActionGroup resolves a named group within a specific immutable or DRAFT version.
func (s *Service) ActionGroup(ctx context.Context, agentID, version, name string) (*api.AgentActionGroup, error) {
	rows, err := s.rows(ctx, kindActionGroup, agentID+"/"+version+"/")
	if err != nil {
		return nil, err
	}

	for _, r := range rows {
		if common.Value(r.Group.ActionGroupName) == name && common.Value(r.Group.ActionGroupState) == stateEnabled {
			return r.Group, nil
		}
	}

	return nil, missing()
}

// ListAgentAliases lists records in storage key order.
func (s *Service) ListAgentAliases(
	ctx context.Context,
	in *api.ListAgentAliasesRequest,
) (*api.ListAgentAliasesResponse, error) {
	if _, checkErr5 := s.lookup(ctx, kindAgent, common.Value(in.AgentId)); checkErr5 != nil {
		return nil, checkErr5
	}

	rows, err := s.rows(ctx, kindAlias, common.Value(in.AgentId)+"/")
	if err != nil {
		return nil, err
	}

	values := []api.AgentAliasSummary{}

	for _, r := range rows {
		values = append(
			values,
			api.AgentAliasSummary{
				AgentAliasId:         r.Alias.AgentAliasId,
				AgentAliasName:       r.Alias.AgentAliasName,
				Description:          r.Alias.Description,
				RoutingConfiguration: r.Alias.RoutingConfiguration,
				AgentAliasStatus:     r.Alias.AgentAliasStatus,
				CreatedAt:            r.Alias.CreatedAt,
				UpdatedAt:            r.Alias.UpdatedAt,
				AliasInvocationState: r.Alias.AliasInvocationState,
			},
		)
	}

	page, next, err := common.Page(
		values,
		func(v api.AgentAliasSummary) string { return common.Value(v.AgentAliasId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListAgentAliasesResponse{AgentAliasSummaries: page, NextToken: next}, nil
}

// ListAgentVersions lists records in storage key order.
func (s *Service) ListAgentVersions(
	ctx context.Context,
	in *api.ListAgentVersionsRequest,
) (*api.ListAgentVersionsResponse, error) {
	if _, checkErr6 := s.lookup(ctx, kindAgent, common.Value(in.AgentId)); checkErr6 != nil {
		return nil, checkErr6
	}

	rows, err := s.rows(ctx, kindVersion, common.Value(in.AgentId)+"/")
	if err != nil {
		return nil, err
	}

	values := []api.AgentVersionSummary{}

	for _, r := range rows {
		values = append(
			values,
			api.AgentVersionSummary{
				AgentName:              r.Version.AgentName,
				AgentStatus:            r.Version.AgentStatus,
				CreatedAt:              r.Version.CreatedAt,
				UpdatedAt:              r.Version.UpdatedAt,
				Description:            r.Version.Description,
				GuardrailConfiguration: r.Version.GuardrailConfiguration,
			},
		)
	}

	page, next, err := common.Page(
		values,
		func(v api.AgentVersionSummary) string { return common.Value(v.AgentVersion) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListAgentVersionsResponse{AgentVersionSummaries: page, NextToken: next}, nil
}
