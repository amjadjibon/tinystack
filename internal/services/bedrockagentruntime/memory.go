package bedrockagentruntime

import (
	"context"
	"time"

	controlapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentruntime"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func memoryEnabled(agent *controlapi.AgentVersion) bool {
	if agent.MemoryConfiguration == nil {
		return false
	}

	for _, typ := range agent.MemoryConfiguration.EnabledMemoryTypes {
		if typ == "SESSION_SUMMARY" {
			return true
		}
	}

	return false
}

func (s *Service) recordMemory(
	ctx context.Context,
	scope request.Scope,
	in *api.InvokeAgentRequest,
	agent *controlapi.AgentVersion,
	state *conversation,
) error {
	if in.MemoryId == nil || !memoryEnabled(agent) || state.Fixture.MemorySummary == "" {
		return nil
	}

	days := int64(30)
	if agent.MemoryConfiguration.StorageDays != nil {
		days = *agent.MemoryConfiguration.StorageDays
	}

	expiry := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
	summary := api.MemorySessionSummary{
		MemoryId:          in.MemoryId,
		SessionId:         in.SessionId,
		SessionStartTime:  new(state.Started),
		SessionExpiryTime: new(expiry),
		SummaryText:       new(state.Fixture.MemorySummary),
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(
			tx,
			scope.Key(
				serviceName,
				kindMemory,
				common.Value(in.AgentId)+"/"+common.Value(in.MemoryId)+"/"+common.Value(in.SessionId),
			),
			summary,
		)
	})
}

// GetAgentMemory returns only fixture-provided summaries that have not expired.
func (s *Service) GetAgentMemory(
	ctx context.Context,
	in *api.GetAgentMemoryRequest,
) (*api.GetAgentMemoryResponse, error) {
	agent, err := s.agents.ResolveInvocation(ctx, common.Value(in.AgentId), common.Value(in.AgentAliasId))
	if err != nil {
		return nil, err
	}

	if !memoryEnabled(agent) {
		return nil, invalid("Agent session-summary memory is not enabled.")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	values := []api.Memory{}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[api.MemorySessionSummary](
			tx,
			scope.Prefix(serviceName, kindMemory)+common.Value(in.AgentId)+"/"+common.Value(in.MemoryId)+"/",
		)
		if e != nil {
			return e
		}

		for _, r := range rows {
			if r.SessionExpiryTime != nil && r.SessionExpiryTime.After(time.Now()) {
				values = append(values, api.Memory{SessionSummary: r})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		values,
		func(v api.Memory) string { return common.Value(v.SessionSummary.SessionId) },
		in.NextToken,
		limit(in.MaxItems),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.GetAgentMemoryResponse{MemoryContents: page, NextToken: next}, nil
}

// DeleteAgentMemory removes fixture-provided summaries for one memory identifier or all memory.
func (s *Service) DeleteAgentMemory(
	ctx context.Context,
	in *api.DeleteAgentMemoryRequest,
) (*api.DeleteAgentMemoryResponse, error) {
	agent, err := s.agents.ResolveInvocation(ctx, common.Value(in.AgentId), common.Value(in.AgentAliasId))
	if err != nil {
		return nil, err
	}

	if !memoryEnabled(agent) {
		return nil, invalid("Agent session-summary memory is not enabled.")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		prefix := scope.Prefix(serviceName, kindMemory) + common.Value(in.AgentId) + "/"
		if in.MemoryId != nil {
			prefix += *in.MemoryId + "/"
		}

		for _, key := range tx.Keys(prefix) {
			tx.Delete(key)
		}

		return nil
	})
	return &api.DeleteAgentMemoryResponse{}, err
}
