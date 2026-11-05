package bedrock

import (
	"context"
	"strconv"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type guardrail struct {
	Detail      api.GetGuardrailResponse
	Tags        map[string]string
	Versions    []api.GetGuardrailResponse
	NextVersion int
}

func wordPolicy(config *api.GuardrailWordPolicyConfig) (*api.GuardrailWordPolicy, error) {
	if config == nil {
		return &api.GuardrailWordPolicy{}, nil
	}

	if len(config.WordsConfig) == 0 || len(config.WordsConfig) > 10000 {
		return nil, invalid("wordsConfig must contain 1 to 10000 words.")
	}

	policy := &api.GuardrailWordPolicy{}
	for _, word := range config.WordsConfig {
		if common.Value(word.Text) == "" {
			return nil, invalid("Word text cannot be empty.")
		}

		policy.Words = append(policy.Words, api.GuardrailWord{Text: word.Text})
	}

	return policy, nil
}

func (s *Service) changeGuardrail(ctx context.Context, ref *string, fn func(*guardrail) error) (*guardrail, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := resourceID(scope, guardrailKind, common.Value(ref))
	if err != nil {
		return nil, err
	}

	var row *guardrail
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var ok bool
		var e error
		row, ok, e = storage.Read[guardrail](tx, scope.Key(serviceName, guardrailKind, id))
		if e != nil {
			return e
		}

		if !ok {
			return missing()
		}

		if e := fn(row); e != nil {
			return e
		}

		return storage.Write(tx, scope.Key(serviceName, guardrailKind, id), row)
	})
	return row, err
}

// CreateGuardrail creates a draft word-policy guardrail.
func (s *Service) CreateGuardrail(
	ctx context.Context,
	in *api.CreateGuardrailRequest,
) (*api.CreateGuardrailResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !resourceName.MatchString(common.Value(in.Name)) || common.Value(in.BlockedInputMessaging) == "" ||
		common.Value(in.BlockedOutputsMessaging) == "" {
		return nil, invalid("Name and blocked messages are required.")
	}

	policy, err := wordPolicy(in.WordPolicyConfig)
	if err != nil {
		return nil, err
	}

	tags, err := checkedTags(in.Tags)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(12, "0123456789abcdefghijklmnopqrstuvwxyz")
	now := time.Now().UTC()
	row := guardrail{Detail: api.GetGuardrailResponse{
		Name:                    in.Name,
		Description:             in.Description,
		GuardrailId:             new(id),
		GuardrailArn:            new(resourceARN(scope, guardrailKind, id)),
		Version:                 new("DRAFT"),
		Status:                  new("READY"),
		CreatedAt:               new(now),
		UpdatedAt:               new(now),
		WordPolicy:              policy,
		BlockedInputMessaging:   in.BlockedInputMessaging,
		BlockedOutputsMessaging: in.BlockedOutputsMessaging,
	}, Tags: tags, NextVersion: 1}
	token := idempotency.Key(scope, serviceName, "CreateGuardrail", in.ClientRequestToken)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, found, e := idempotency.Replay[guardrail](tx, token, in,
			invalid("clientRequestToken was used with different parameters."))
		if e != nil || found {
			row = previous
			return e
		}

		records, e := storage.Records[guardrail](tx, scope.Prefix(serviceName, guardrailKind))
		if e != nil {
			return e
		}

		for _, existing := range records {
			if *existing.Detail.Name == *in.Name {
				return invalid("A guardrail with this name already exists.")
			}
		}

		if e := storage.Write(tx, scope.Key(serviceName, guardrailKind, id), row); e != nil {
			return e
		}

		return idempotency.Remember(tx, token, in, row)
	})
	return &api.CreateGuardrailResponse{
		GuardrailId:  row.Detail.GuardrailId,
		GuardrailArn: row.Detail.GuardrailArn,
		Version:      row.Detail.Version,
		CreatedAt:    row.Detail.CreatedAt,
	}, err
}

// GetGuardrail reads a draft or immutable numbered version.
func (s *Service) GetGuardrail(ctx context.Context, in *api.GetGuardrailRequest) (*api.GetGuardrailResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := resourceID(scope, guardrailKind, common.Value(in.GuardrailIdentifier))
	if err != nil {
		return nil, err
	}

	var out *api.GetGuardrailResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		row, ok, e := storage.Read[guardrail](tx, scope.Key(serviceName, guardrailKind, id))
		if e != nil {
			return e
		}

		if !ok {
			return missing()
		}

		if in.GuardrailVersion == nil || *in.GuardrailVersion == "DRAFT" {
			out = &row.Detail
			return nil
		}

		for i := range row.Versions {
			version := &row.Versions[i]
			if *version.Version == *in.GuardrailVersion {
				out = version
				return nil
			}
		}

		return missing()
	})
	return out, err
}

// UpdateGuardrail replaces the mutable draft; numbered versions remain unchanged.
func (s *Service) UpdateGuardrail(
	ctx context.Context,
	in *api.UpdateGuardrailRequest,
) (*api.UpdateGuardrailResponse, error) {
	if !resourceName.MatchString(common.Value(in.Name)) || common.Value(in.BlockedInputMessaging) == "" ||
		common.Value(in.BlockedOutputsMessaging) == "" {
		return nil, invalid("Name and blocked messages are required.")
	}

	policy, err := wordPolicy(in.WordPolicyConfig)
	if err != nil {
		return nil, err
	}

	row, err := s.changeGuardrail(ctx, in.GuardrailIdentifier, func(row *guardrail) error {
		row.Detail.Name = in.Name
		row.Detail.Description = in.Description
		row.Detail.BlockedInputMessaging = in.BlockedInputMessaging
		row.Detail.BlockedOutputsMessaging = in.BlockedOutputsMessaging
		row.Detail.WordPolicy = policy
		row.Detail.UpdatedAt = new(time.Now().UTC())
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateGuardrailResponse{
		GuardrailId:  row.Detail.GuardrailId,
		GuardrailArn: row.Detail.GuardrailArn,
		Version:      row.Detail.Version,
		UpdatedAt:    row.Detail.UpdatedAt,
	}, nil
}

// CreateGuardrailVersion snapshots the current draft with a monotonically increasing version.
func (s *Service) CreateGuardrailVersion(
	ctx context.Context,
	in *api.CreateGuardrailVersionRequest,
) (*api.CreateGuardrailVersionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := resourceID(scope, guardrailKind, common.Value(in.GuardrailIdentifier))
	if err != nil {
		return nil, err
	}

	token := idempotency.Key(scope, serviceName, "CreateGuardrailVersion/"+id, in.ClientRequestToken)
	var out *api.CreateGuardrailVersionResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, found, e := idempotency.Replay[*api.CreateGuardrailVersionResponse](tx, token, in,
			invalid("clientRequestToken was used with different parameters."))
		if e != nil || found {
			out = previous
			return e
		}

		row, found, e := storage.Read[guardrail](tx, scope.Key(serviceName, guardrailKind, id))
		if e != nil {
			return e
		}

		if !found {
			return missing()
		}

		version := strconv.Itoa(row.NextVersion)
		row.NextVersion++
		snapshot := *common.Clone(&row.Detail)
		snapshot.Version = new(version)
		snapshot.Description = in.Description
		snapshot.CreatedAt = new(time.Now().UTC())
		row.Versions = append(row.Versions, snapshot)
		if e := storage.Write(tx, scope.Key(serviceName, guardrailKind, id), row); e != nil {
			return e
		}

		out = &api.CreateGuardrailVersionResponse{GuardrailId: row.Detail.GuardrailId, Version: new(version)}
		return idempotency.Remember(tx, token, in, out)
	})
	return out, err
}

// DeleteGuardrail deletes a numbered version or the whole guardrail if no version is given.
func (s *Service) DeleteGuardrail(
	ctx context.Context,
	in *api.DeleteGuardrailRequest,
) (*api.DeleteGuardrailResponse, error) {
	if in.GuardrailVersion != nil {
		if *in.GuardrailVersion == "DRAFT" {
			return nil, invalid("Omit the version to delete a draft and its versions.")
		}

		_, err := s.changeGuardrail(ctx, in.GuardrailIdentifier, func(row *guardrail) error {
			for i := range row.Versions {
				v := &row.Versions[i]
				if *v.Version == *in.GuardrailVersion {
					row.Versions = append(row.Versions[:i], row.Versions[i+1:]...)
					return nil
				}
			}

			return missing()
		})
		return &api.DeleteGuardrailResponse{}, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := resourceID(scope, guardrailKind, common.Value(in.GuardrailIdentifier))
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, guardrailKind, id)
		if _, ok := tx.Get(key); !ok {
			return missing()
		}

		tx.Delete(key)
		return nil
	})
	return &api.DeleteGuardrailResponse{}, err
}

// ListGuardrails lists drafts, or all versions when a guardrail identifier is supplied.
func (s *Service) ListGuardrails(
	ctx context.Context,
	in *api.ListGuardrailsRequest,
) (*api.ListGuardrailsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	n, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	id := ""
	if in.GuardrailIdentifier != nil {
		id, err = resourceID(scope, guardrailKind, *in.GuardrailIdentifier)
		if err != nil {
			return nil, err
		}
	}

	rows := []api.GuardrailSummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[guardrail](tx, scope.Prefix(serviceName, guardrailKind))
		if e != nil {
			return e
		}

		found := false
		for _, row := range records {
			if id != "" && id != *row.Detail.GuardrailId {
				continue
			}

			found = true
			details := []api.GetGuardrailResponse{row.Detail}
			if id != "" {
				details = append(details, row.Versions...)
			}

			for i := range details {
				d := &details[i]
				rows = append(
					rows,
					api.GuardrailSummary{
						Id:          d.GuardrailId,
						Arn:         d.GuardrailArn,
						Name:        d.Name,
						Description: d.Description,
						Version:     d.Version,
						Status:      d.Status,
						CreatedAt:   d.CreatedAt,
						UpdatedAt:   d.UpdatedAt,
					},
				)
			}
		}

		if id != "" && !found {
			return missing()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(row api.GuardrailSummary) string { return *row.Id + ":" + *row.Version },
		in.NextToken,
		n,
		invalid("Invalid nextToken."),
	)
	return &api.ListGuardrailsResponse{Guardrails: page, NextToken: next}, err
}
