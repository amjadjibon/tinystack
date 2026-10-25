package bedrockagentruntime

import (
	"context"
	"encoding/json"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentruntime"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateSession stores a standalone session, independent of InvokeAgent conversation sessions.
func (s *Service) CreateSession(ctx context.Context, in *api.CreateSessionRequest) (*api.CreateSessionResponse, error) {
	if in.EncryptionKeyArn != nil {
		if s.caller == nil {
			return nil, invalid("Reference validation is unavailable.")
		}

		if _, checkErr1 := s.caller.Call(
			ctx,
			"kms",
			"DescribeKey",
			map[string]any{"KeyId": *in.EncryptionKeyArn},
		); checkErr1 != nil {
			return nil, invalid("Invalid encryption key: %v", checkErr1)
		}
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := uuid()
	now := time.Now().UTC()
	r := &api.GetSessionResponse{
		SessionId:        new(id),
		SessionArn:       new(sessionARN(scope, id)),
		SessionStatus:    new(statusActive),
		CreatedAt:        new(now),
		LastUpdatedAt:    new(now),
		SessionMetadata:  common.CloneTags(in.SessionMetadata),
		EncryptionKeyArn: in.EncryptionKeyArn,
	}

	err = s.store.Update(
		ctx,
		func(tx storage.WriteTx) error { return storage.Write(tx, scope.Key(serviceName, kindSession, id), r) },
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateSessionResponse{
		SessionId:     r.SessionId,
		SessionArn:    r.SessionArn,
		SessionStatus: r.SessionStatus,
		CreatedAt:     r.CreatedAt,
	}, nil
}

// GetSession reads an active or ended session.
func (s *Service) GetSession(ctx context.Context, in *api.GetSessionRequest) (*api.GetSessionResponse, error) {
	return s.mutateSession(
		ctx,
		in.SessionIdentifier,
		false,
		func(_ storage.WriteTx, _ request.Scope, _ *api.GetSessionResponse) error { return nil },
	)
}

// UpdateSession replaces metadata on an active session.
func (s *Service) UpdateSession(ctx context.Context, in *api.UpdateSessionRequest) (*api.UpdateSessionResponse, error) {
	r, err := s.mutateSession(
		ctx,
		in.SessionIdentifier,
		true,
		func(_ storage.WriteTx, _ request.Scope, r *api.GetSessionResponse) error {
			r.SessionMetadata = common.CloneTags(in.SessionMetadata)
			r.LastUpdatedAt = new(time.Now().UTC())
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateSessionResponse{
		SessionId:     r.SessionId,
		SessionArn:    r.SessionArn,
		SessionStatus: r.SessionStatus,
		CreatedAt:     r.CreatedAt,
		LastUpdatedAt: r.LastUpdatedAt,
	}, nil
}

// EndSession stops further writes while retaining invocations and steps.
func (s *Service) EndSession(ctx context.Context, in *api.EndSessionRequest) (*api.EndSessionResponse, error) {
	r, err := s.mutateSession(
		ctx,
		in.SessionIdentifier,
		false,
		func(_ storage.WriteTx, _ request.Scope, r *api.GetSessionResponse) error {
			r.SessionStatus = new("ENDED")
			r.LastUpdatedAt = new(time.Now().UTC())
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.EndSessionResponse{
		SessionId:     r.SessionId,
		SessionArn:    r.SessionArn,
		SessionStatus: r.SessionStatus,
	}, nil
}

// DeleteSession removes a session and its invocation history.
func (s *Service) DeleteSession(ctx context.Context, in *api.DeleteSessionRequest) (*api.DeleteSessionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := sessionID(scope, common.Value(in.SessionIdentifier))
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := readSession(tx, scope, id); e != nil {
			return e
		}

		tx.Delete(scope.Key(serviceName, kindSession, id))
		for _, kind := range []string{kindInvocation, kindStep} {
			for _, key := range tx.Keys(scope.Prefix(serviceName, kind) + id + "/") {
				tx.Delete(key)
			}
		}

		return nil
	})
	return &api.DeleteSessionResponse{}, err
}

// ListSessions returns sessions in storage key order.
func (s *Service) ListSessions(ctx context.Context, in *api.ListSessionsRequest) (*api.ListSessionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	values := []api.SessionSummary{}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[api.GetSessionResponse](tx, scope.Prefix(serviceName, kindSession))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if r.CreatedAt != nil && time.Since(*r.CreatedAt) >= 30*24*time.Hour {
				continue
			}

			if common.Value(r.SessionStatus) == statusActive && r.LastUpdatedAt != nil &&
				time.Since(*r.LastUpdatedAt) >= time.Hour {
				r.SessionStatus = new("EXPIRED")
			}

			values = append(
				values,
				api.SessionSummary{
					SessionId:     r.SessionId,
					SessionArn:    r.SessionArn,
					SessionStatus: r.SessionStatus,
					CreatedAt:     r.CreatedAt,
					LastUpdatedAt: r.LastUpdatedAt,
				},
			)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		values,
		func(v api.SessionSummary) string { return common.Value(v.SessionId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListSessionsResponse{SessionSummaries: page, NextToken: next}, nil
}

type invocationRecord struct {
	api.InvocationSummary
	Description *string `json:"description,omitempty"`
}

// CreateInvocation adds a durable invocation to an active session.
func (s *Service) CreateInvocation(
	ctx context.Context,
	in *api.CreateInvocationRequest,
) (*api.CreateInvocationResponse, error) {
	id := common.Value(in.InvocationId)
	if id == "" {
		id = uuid()
	}

	var result api.InvocationSummary
	_, err := s.mutateSession(
		ctx,
		in.SessionIdentifier,
		true,
		func(tx storage.WriteTx, scope request.Scope, r *api.GetSessionResponse) error {
			key := scope.Key(serviceName, kindInvocation, common.Value(r.SessionId)+"/"+id)
			if _, ok := tx.Get(key); ok {
				return conflict("Invocation already exists.")
			}

			now := time.Now().UTC()
			result = api.InvocationSummary{SessionId: r.SessionId, InvocationId: new(id), CreatedAt: new(now)}

			r.LastUpdatedAt = new(now)
			return storage.Write(tx, key, &invocationRecord{InvocationSummary: result, Description: in.Description})
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateInvocationResponse{
		SessionId:    result.SessionId,
		InvocationId: result.InvocationId,
		CreatedAt:    result.CreatedAt,
	}, nil
}

// ListInvocations reads invocation summaries in storage key order.
func (s *Service) ListInvocations(
	ctx context.Context,
	in *api.ListInvocationsRequest,
) (*api.ListInvocationsResponse, error) {
	values := []api.InvocationSummary{}

	_, err := s.mutateSession(
		ctx,
		in.SessionIdentifier,
		false,
		func(tx storage.WriteTx, scope request.Scope, r *api.GetSessionResponse) error {
			rows, e := storage.Records[invocationRecord](
				tx,
				scope.Prefix(serviceName, kindInvocation)+common.Value(r.SessionId)+"/",
			)
			if e != nil {
				return e
			}

			for _, row := range rows {
				values = append(values, row.InvocationSummary)
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		values,
		func(v api.InvocationSummary) string { return common.Value(v.InvocationId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListInvocationsResponse{InvocationSummaries: page, NextToken: next}, nil
}

// PutInvocationStep stores a step in an existing invocation.
func (s *Service) PutInvocationStep(
	ctx context.Context,
	in *api.PutInvocationStepRequest,
) (*api.PutInvocationStepResponse, error) {
	id := common.Value(in.InvocationStepId)
	if id == "" {
		id = uuid()
	}

	if checkErr2 := validatePayload(in.Payload); checkErr2 != nil {
		return nil, checkErr2
	}

	_, err := s.mutateSession(
		ctx,
		in.SessionIdentifier,
		true,
		func(tx storage.WriteTx, scope request.Scope, r *api.GetSessionResponse) error {
			prefix := common.Value(r.SessionId) + "/" + common.Value(in.InvocationIdentifier)
			if _, ok := tx.Get(scope.Key(serviceName, kindInvocation, prefix)); !ok {
				return missing()
			}

			key := scope.Key(serviceName, kindStep, prefix+"/"+id)
			if _, ok := tx.Get(key); ok {
				return conflict("Invocation step already exists.")
			}

			// Session Management quotas apply across all invocations in the session.
			if len(tx.Keys(scope.Prefix(serviceName, kindStep)+common.Value(r.SessionId)+"/")) >= 1000 {
				return invalid("A session can have at most 1000 invocation steps.")
			}

			r.LastUpdatedAt = new(time.Now().UTC())
			return storage.Write(
				tx,
				key,
				&api.InvocationStep{
					SessionId:          r.SessionId,
					InvocationId:       in.InvocationIdentifier,
					InvocationStepId:   new(id),
					InvocationStepTime: in.InvocationStepTime,
					Payload:            in.Payload,
				},
			)
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.PutInvocationStepResponse{InvocationStepId: new(id)}, nil
}

func validatePayload(payload *api.InvocationStepPayload) error {
	if payload == nil || payload.ContentBlocks == nil {
		return invalid("Invocation step requires content blocks.")
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	if len(encoded) > 50<<20 {
		return invalid("An invocation step cannot exceed 50 MB.")
	}

	for _, b := range *payload.ContentBlocks {
		if (b.Text == nil) == (b.Image == nil) {
			return invalid("Each content block requires exactly one of text or image.")
		}

		if b.Image != nil {
			if b.Image.Source == nil || (b.Image.Source.Bytes == nil) == (b.Image.Source.S3Location == nil) {
				return invalid("Image requires exactly one source.")
			}
		}
	}

	return nil
}

// GetInvocationStep reads an exact invocation step.
func (s *Service) GetInvocationStep(
	ctx context.Context,
	in *api.GetInvocationStepRequest,
) (*api.GetInvocationStepResponse, error) {
	var result *api.InvocationStep
	_, err := s.mutateSession(
		ctx,
		in.SessionIdentifier,
		false,
		func(tx storage.WriteTx, scope request.Scope, r *api.GetSessionResponse) error {
			key := scope.Key(
				serviceName,
				kindStep,
				common.Value(
					r.SessionId,
				)+"/"+common.Value(
					in.InvocationIdentifier,
				)+"/"+common.Value(
					in.InvocationStepId,
				),
			)
			value, ok, e := storage.Read[api.InvocationStep](tx, key)
			if e != nil {
				return e
			}

			if !ok {
				return missing()
			}

			result = value
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.GetInvocationStepResponse{InvocationStep: result}, nil
}

// ListInvocationSteps lists steps, optionally restricted to one invocation.
func (s *Service) ListInvocationSteps(
	ctx context.Context,
	in *api.ListInvocationStepsRequest,
) (*api.ListInvocationStepsResponse, error) {
	values := []api.InvocationStepSummary{}

	_, err := s.mutateSession(
		ctx,
		in.SessionIdentifier,
		false,
		func(tx storage.WriteTx, scope request.Scope, r *api.GetSessionResponse) error {
			prefix := scope.Prefix(serviceName, kindStep) + common.Value(r.SessionId) + "/"
			if in.InvocationIdentifier != nil {
				prefix += *in.InvocationIdentifier + "/"
			}

			rows, e := storage.Records[api.InvocationStep](tx, prefix)
			if e != nil {
				return e
			}

			for _, v := range rows {
				values = append(
					values,
					api.InvocationStepSummary{
						SessionId:          v.SessionId,
						InvocationId:       v.InvocationId,
						InvocationStepId:   v.InvocationStepId,
						InvocationStepTime: v.InvocationStepTime,
					},
				)
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(values, func(v api.InvocationStepSummary) string {
		return common.Value(v.InvocationId) + "/" + common.Value(v.InvocationStepId)
	}, in.NextToken, limit(in.MaxResults), invalid("Invalid nextToken."))
	if err != nil {
		return nil, err
	}

	return &api.ListInvocationStepsResponse{InvocationStepSummaries: page, NextToken: next}, nil
}
