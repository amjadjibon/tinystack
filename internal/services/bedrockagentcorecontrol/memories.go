package bedrockagentcorecontrol

import (
	"context"
	"errors"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// strategiesUnsupported explains why long-term memory fails: extracting records needs a model.
const strategiesUnsupported = "memoryStrategies are not supported: long-term memory strategies extract and " +
	"consolidate records with a model, and TinyStack does not invent model output. A memory stores short-term " +
	"events only"

type memoryRecord struct {
	Created       time.Time
	Updated       time.Time
	Due           time.Time
	Tags          map[string]string
	Description   *string
	ID            string
	Name          string
	EncryptionKey string
	ExecutionRole string
	Status        string
	ExpiryDays    int64
}

func readMemory(tx storage.ReadTx, scope request.Scope, id string) (*memoryRecord, error) {
	m, found, err := storage.Read[memoryRecord](tx, memoryKey(scope, id))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, notFound("Memory %s does not exist", id)
	}

	return m, nil
}

func (m *memoryRecord) output(scope request.Scope) *api.Memory {
	out := &api.Memory{
		Arn: new(memoryARN(scope, m.ID)), CreatedAt: common.SecondsMilli(m.Created), Description: m.Description,
		EventExpiryDuration: new(m.ExpiryDays), Id: new(m.ID), Name: new(m.Name), Status: new(m.Status),
		UpdatedAt: common.SecondsMilli(m.Updated), Strategies: []api.MemoryStrategy{},
	}

	if m.EncryptionKey != "" {
		out.EncryptionKeyArn = new(m.EncryptionKey)
	}

	if m.ExecutionRole != "" {
		out.MemoryExecutionRoleArn = new(m.ExecutionRole)
	}

	return out
}

// checkReferences requires the execution role and the encryption key to exist in TinyStack.
func (s *Service) checkReferences(ctx context.Context, scope request.Scope, role, key *string) error {
	if role != nil {
		if err := s.requireRole(ctx, *role); err != nil {
			return err
		}
	}

	if key == nil {
		return nil
	}

	err := s.keys.Check(ctx, scope, *key)
	if e, ok := errors.AsType[*awserror.Error](err); ok {
		if e.Code == "NotFoundException" {
			return notFound("KMS key %s does not exist", *key)
		}

		return invalid("KMS key %s cannot be used: %s", *key, e.Message)
	}

	return err
}

func (s *Service) CreateMemory(ctx context.Context, in *api.CreateMemoryInput) (*api.CreateMemoryOutput, error) {
	if len(in.MemoryStrategies) > 0 {
		return nil, invalid("%s", strategiesUnsupported)
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid("%s", err.Error())
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.checkReferences(ctx, scope, in.MemoryExecutionRoleArn, in.EncryptionKeyArn)
	if err != nil {
		return nil, err
	}

	var out *api.CreateMemoryOutput
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		token := idempotency.Key(scope, serviceName, "CreateMemory", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.CreateMemoryOutput](tx, token, in,
			conflict("Client token %s was already used with different parameters", common.Value(in.ClientToken)))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		existing, e := storage.Records[memoryRecord](tx, scope.Prefix(serviceName, kindMemory))
		if e != nil {
			return e
		}

		for _, old := range existing {
			if old.Name == common.Value(in.Name) {
				return conflict("Memory %s already exists", old.Name)
			}
		}

		now := time.Now().UTC()
		m := &memoryRecord{
			Created: now, Updated: now, Due: now.Add(transitionDelay), Tags: common.CloneTags(in.Tags),
			Description: in.Description, ID: newID(common.Value(in.Name)), Name: common.Value(in.Name),
			EncryptionKey: common.Value(in.EncryptionKeyArn),
			ExecutionRole: common.Value(in.MemoryExecutionRoleArn), Status: statusCreating,
			ExpiryDays: common.Value(in.EventExpiryDuration),
		}

		out = &api.CreateMemoryOutput{Memory: m.output(scope)}

		if e := queue(tx, scope, kindMemory, m.ID, memoryKey(scope, m.ID), m); e != nil {
			return e
		}

		return idempotency.Remember(tx, token, in, out)
	})

	return out, err
}

// GetMemory describes a memory. The view only changes how AWS decrypts the memory's content, which TinyStack
// stores unencrypted either way.
func (s *Service) GetMemory(ctx context.Context, in *api.GetMemoryInput) (*api.GetMemoryOutput, error) {
	var out *api.GetMemoryOutput
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		m, e := readMemory(tx, scope, common.Value(in.MemoryId))
		if e != nil {
			return e
		}

		out = &api.GetMemoryOutput{Memory: m.output(scope)}

		return nil
	})

	return out, err
}

func (s *Service) ListMemories(ctx context.Context, in *api.ListMemoriesInput) (*api.ListMemoriesOutput, error) {
	var out *api.ListMemoriesOutput
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		memories, e := storage.Records[memoryRecord](tx, scope.Prefix(serviceName, kindMemory))
		if e != nil {
			return e
		}

		page, next, e := common.Page(memories, func(m *memoryRecord) string { return m.ID }, in.NextToken,
			pageLimit(in.MaxResults), errBadToken)
		if e != nil {
			return e
		}

		out = &api.ListMemoriesOutput{Memories: []api.MemorySummary{}, NextToken: next}
		for _, m := range page {
			out.Memories = append(out.Memories, api.MemorySummary{
				Arn: new(memoryARN(scope, m.ID)), CreatedAt: common.SecondsMilli(m.Created), Id: new(m.ID),
				Status: new(m.Status), UpdatedAt: common.SecondsMilli(m.Updated),
			})
		}

		return nil
	})

	return out, err
}

func (s *Service) UpdateMemory(ctx context.Context, in *api.UpdateMemoryInput) (*api.UpdateMemoryOutput, error) {
	if ms := in.MemoryStrategies; ms != nil &&
		len(ms.AddMemoryStrategies)+len(ms.ModifyMemoryStrategies)+len(ms.DeleteMemoryStrategies) > 0 {
		return nil, invalid("%s", strategiesUnsupported)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.checkReferences(ctx, scope, in.MemoryExecutionRoleArn, nil)
	if err != nil {
		return nil, err
	}

	var out *api.UpdateMemoryOutput
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		token := idempotency.Key(scope, serviceName, "UpdateMemory", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.UpdateMemoryOutput](tx, token, in,
			conflict("Client token %s was already used with different parameters", common.Value(in.ClientToken)))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		changeErr := func() error {
			m, e := readMemory(tx, scope, common.Value(in.MemoryId))
			if e != nil {
				return e
			}

			if m.Status != statusActive {
				return conflict("Memory %s is %s and can only be updated when it is ACTIVE", m.ID, m.Status)
			}

			if in.Description != nil {
				m.Description = in.Description
			}

			if in.EventExpiryDuration != nil {
				m.ExpiryDays = *in.EventExpiryDuration
			}

			if in.MemoryExecutionRoleArn != nil {
				m.ExecutionRole = *in.MemoryExecutionRoleArn
			}

			now := time.Now().UTC()
			m.Status, m.Updated, m.Due = statusUpdating, now, now.Add(transitionDelay)
			out = &api.UpdateMemoryOutput{Memory: m.output(scope)}

			return queue(tx, scope, kindMemory, m.ID, memoryKey(scope, m.ID), m)
		}()
		if changeErr != nil {
			return changeErr
		}

		return idempotency.Remember(tx, token, in, out)
	})

	return out, err
}

func (s *Service) DeleteMemory(ctx context.Context, in *api.DeleteMemoryInput) (*api.DeleteMemoryOutput, error) {
	var out *api.DeleteMemoryOutput
	var scope request.Scope
	err := s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc

		token := idempotency.Key(sc, serviceName, "DeleteMemory", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.DeleteMemoryOutput](tx, token, in,
			conflict("Client token %s was already used with different parameters", common.Value(in.ClientToken)))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		changeErr := func() error {
			scope = sc
			m, e := readMemory(tx, scope, common.Value(in.MemoryId))
			if e != nil {
				return e
			}

			out = &api.DeleteMemoryOutput{MemoryId: new(m.ID), Status: new(statusDeleting)}
			if m.Status == statusDeleting {
				return nil
			}

			m.Status, m.Due = statusDeleting, time.Now().UTC().Add(transitionDelay)

			return queue(tx, scope, kindMemory, m.ID, memoryKey(scope, m.ID), m)
		}()
		if changeErr != nil {
			return changeErr
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err == nil && s.observer != nil {
		s.observer.MemoryDeleted(ctx, scope, common.Value(out.MemoryId))
	}

	return out, err
}

// MemoryInfo is what the data plane needs of a memory.
type MemoryInfo struct {
	// ExpiryDays is how many days an event is kept.
	ExpiryDays int64
}

// Memory returns the ACTIVE memory with id in scope, for the data plane. A memory that is not ACTIVE, such as one
// still CREATING, is a validation failure; an unknown one is not found.
func Memory(tx storage.ReadTx, scope request.Scope, id string) (MemoryInfo, error) {
	m, err := readMemory(tx, scope, id)
	if err != nil {
		return MemoryInfo{}, err
	}

	if m.Status != statusActive && m.Status != statusUpdating {
		return MemoryInfo{}, invalid("Memory %s is %s and cannot take events", m.ID, m.Status)
	}

	return MemoryInfo{ExpiryDays: m.ExpiryDays}, nil
}
