package bedrockagent

import (
	"context"
	"strconv"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateAgent creates a DRAFT and the built-in test alias.
func (s *Service) CreateAgent(ctx context.Context, in *api.CreateAgentRequest) (*api.CreateAgentResponse, error) {
	if checkErr1 := s.validateAgent(
		ctx,
		in.AgentResourceRoleArn,
		in.FoundationModel,
		in.CustomerEncryptionKeyArn,
		in.GuardrailConfiguration,
		in.AgentCollaboration,
		in.PromptOverrideConfiguration,
	); checkErr1 != nil {
		return nil, checkErr1
	}

	if checkErr2 := tagging.CheckSet(in.Tags); checkErr2 != nil {
		return nil, invalid("%v", checkErr2)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(10, common.AlphabetUpperHex)
	now := time.Now().UTC()
	// AWS always returns the prompt configuration; TinyStack has no default prompt templates to fill it with.
	prompts := common.OrElse(in.PromptOverrideConfiguration, new(api.PromptOverrideConfiguration{}))
	value := &api.Agent{
		AgentId: new(
			id,
		),
		AgentArn:                    new(arn(scope, kindAgent, id)),
		AgentVersion:                new(versionDraft),
		AgentStatus:                 new(statusCreating),
		AgentName:                   in.AgentName,
		ClientToken:                 in.ClientToken,
		Instruction:                 in.Instruction,
		FoundationModel:             in.FoundationModel,
		Description:                 in.Description,
		IdleSessionTTLInSeconds:     common.OrElse(in.IdleSessionTTLInSeconds, new(int64(600))),
		AgentResourceRoleArn:        in.AgentResourceRoleArn,
		CustomerEncryptionKeyArn:    in.CustomerEncryptionKeyArn,
		PromptOverrideConfiguration: prompts,
		GuardrailConfiguration:      in.GuardrailConfiguration,
		MemoryConfiguration:         in.MemoryConfiguration,
		AgentCollaboration:          common.OrElse(in.AgentCollaboration, new("DISABLED")),
		CreatedAt:                   new(now),
		UpdatedAt:                   new(now),
		OrchestrationType:           new("DEFAULT"),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, kindAgent))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if in.ClientToken != nil && common.Value(r.Agent.ClientToken) == *in.ClientToken {
				value = common.Clone(r.Agent)
				return nil
			}

			if common.Value(r.Agent.AgentName) == common.Value(in.AgentName) {
				return conflict("Agent name already exists.")
			}
		}

		if e := storage.Write(
			tx,
			scope.Key(serviceName, kindAgent, id),
			&record{Agent: value, Tags: in.Tags, NextVersion: 1},
		); e != nil {
			return e
		}

		if e := storage.Write(
			tx,
			scope.Key(serviceName, kindVersion, id+"/DRAFT"),
			&record{Version: agentVersion(value)},
		); e != nil {
			return e
		}

		test := &api.AgentAlias{
			AgentId: new(id), AgentAliasId: new(testAliasID), AgentAliasName: new("AgentTestAlias"),
			AgentAliasArn: new(arn(scope, "agent-alias", id+"/TSTALIASID")), AgentAliasStatus: new(statusPrepared),
			AliasInvocationState: new("ACCEPT_INVOCATIONS"), CreatedAt: new(now), UpdatedAt: new(now),
			RoutingConfiguration: []api.AgentAliasRoutingConfigurationListItem{{AgentVersion: new(versionDraft)}},
		}

		return storage.Write(tx, scope.Key(serviceName, kindAlias, id+"/TSTALIASID"), &record{Alias: test})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAgentResponse{Agent: value}, nil
}

// GetAgent exposes one intermediate lifecycle state before committing its terminal state.
func (s *Service) GetAgent(ctx context.Context, in *api.GetAgentRequest) (*api.GetAgentResponse, error) {
	var result *api.Agent
	_, err := s.mutate(
		ctx,
		kindAgent,
		common.Value(in.AgentId),
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			result = common.Clone(r.Agent)
			switch common.Value(r.Agent.AgentStatus) {
			case statusCreating, statusUpdating:
				r.Agent.AgentStatus = new("NOT_PREPARED")
			case "PREPARING":
				r.Agent.AgentStatus = new(statusPrepared)
				r.Agent.PreparedAt = new(time.Now().UTC())
			case statusDeleting:
				// mutate writes the record back; deleting is finalized by the next lookup below.
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	if common.Value(result.AgentStatus) == statusDeleting {
		scope, e := request.RegionalScope(ctx)
		if e != nil {
			return nil, e
		}

		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(scope.Key(serviceName, kindAgent, common.Value(in.AgentId)))
			return nil
		})
	}

	return &api.GetAgentResponse{Agent: result}, err
}

// UpdateAgent changes the DRAFT; numbered versions are immutable.
func (s *Service) UpdateAgent(ctx context.Context, in *api.UpdateAgentRequest) (*api.UpdateAgentResponse, error) {
	if checkErr3 := s.validateAgent(
		ctx,
		in.AgentResourceRoleArn,
		in.FoundationModel,
		in.CustomerEncryptionKeyArn,
		in.GuardrailConfiguration,
		in.AgentCollaboration,
		in.PromptOverrideConfiguration,
	); checkErr3 != nil {
		return nil, checkErr3
	}

	r, err := s.mutate(
		ctx,
		kindAgent,
		common.Value(in.AgentId),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			if common.Value(r.Agent.AgentStatus) == statusDeleting {
				return conflict("Agent is deleting.")
			}

			r.Agent.AgentName = in.AgentName
			r.Agent.Instruction = in.Instruction
			r.Agent.FoundationModel = in.FoundationModel
			r.Agent.Description = in.Description
			r.Agent.IdleSessionTTLInSeconds = common.OrElse(in.IdleSessionTTLInSeconds, new(int64(600)))
			r.Agent.AgentResourceRoleArn = in.AgentResourceRoleArn
			r.Agent.CustomerEncryptionKeyArn = in.CustomerEncryptionKeyArn
			r.Agent.PromptOverrideConfiguration = common.OrElse(
				in.PromptOverrideConfiguration,
				new(api.PromptOverrideConfiguration{}),
			)
			r.Agent.GuardrailConfiguration = in.GuardrailConfiguration
			r.Agent.MemoryConfiguration = in.MemoryConfiguration
			r.Agent.AgentCollaboration = common.OrElse(in.AgentCollaboration, new("DISABLED"))
			r.Agent.AgentStatus = new(statusUpdating)
			r.Agent.UpdatedAt = new(time.Now().UTC())
			r.Agent.PreparedAt = nil
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateAgentResponse{Agent: r.Agent}, nil
}

// PrepareAgent starts deterministic preparation of DRAFT.
func (s *Service) PrepareAgent(ctx context.Context, in *api.PrepareAgentRequest) (*api.PrepareAgentResponse, error) {
	r, err := s.mutate(
		ctx,
		kindAgent,
		common.Value(in.AgentId),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			if common.Value(r.Agent.AgentStatus) != "NOT_PREPARED" &&
				common.Value(r.Agent.AgentStatus) != statusPrepared {
				return conflict("Agent must be NOT_PREPARED or PREPARED.")
			}

			r.Agent.AgentStatus = new("PREPARING")
			r.Agent.UpdatedAt = new(time.Now().UTC())
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.PrepareAgentResponse{
		AgentId:      r.Agent.AgentId,
		AgentStatus:  r.Agent.AgentStatus,
		AgentVersion: r.Agent.AgentVersion,
		PreparedAt:   r.Agent.PreparedAt,
	}, nil
}

// DeleteAgent enforces alias use checks and removes dependent configuration.
func (s *Service) DeleteAgent(ctx context.Context, in *api.DeleteAgentRequest) (*api.DeleteAgentResponse, error) {
	_, err := s.mutate(
		ctx,
		kindAgent,
		common.Value(in.AgentId),
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			aliases := tx.Keys(scope.Prefix(serviceName, kindAlias) + common.Value(in.AgentId) + "/")
			if len(aliases) > 1 && !common.Value(in.SkipResourceInUseCheck) {
				return conflict("Agent has aliases.")
			}

			for _, kind := range []string{kindAlias, kindVersion, kindActionGroup, kindAssociation} {
				for _, key := range tx.Keys(scope.Prefix(serviceName, kind) + common.Value(in.AgentId) + "/") {
					tx.Delete(key)
				}
			}

			r.Agent.AgentStatus = new(statusDeleting)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.DeleteAgentResponse{AgentId: in.AgentId, AgentStatus: new(statusDeleting)}, nil
}

// ListAgents lists DRAFT summaries in storage key order.
func (s *Service) ListAgents(ctx context.Context, in *api.ListAgentsRequest) (*api.ListAgentsResponse, error) {
	rows, err := s.rows(ctx, kindAgent, "")
	if err != nil {
		return nil, err
	}

	values := []api.AgentSummary{}

	for _, r := range rows {
		a := r.Agent
		latest := versionDraft
		if r.NextVersion > 1 {
			latest = strconv.Itoa(r.NextVersion - 1)
		}

		values = append(
			values,
			api.AgentSummary{
				AgentId:                a.AgentId,
				AgentName:              a.AgentName,
				AgentStatus:            a.AgentStatus,
				Description:            a.Description,
				UpdatedAt:              a.UpdatedAt,
				LatestAgentVersion:     new(latest),
				GuardrailConfiguration: a.GuardrailConfiguration,
			},
		)
	}

	page, next, err := common.Page(
		values,
		func(v api.AgentSummary) string { return common.Value(v.AgentId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListAgentsResponse{AgentSummaries: page, NextToken: next}, nil
}
