package secretsmanager

import (
	"context"
	"fmt"
	"maps"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// PutSecretValue adds a version, labeled AWSCURRENT unless VersionStages
// says otherwise.
func (s *Service) PutSecretValue(
	ctx context.Context,
	in *api.PutSecretValueRequest,
) (*api.PutSecretValueResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	request.ReferenceScope(&m, "secretsmanager", common.Value(in.SecretId))
	value, hasValue, err := checkValue(in.SecretString, in.SecretBinary)
	if err != nil {
		return nil, err
	}

	if !hasValue {
		return nil, invalidParameter("You must provide either SecretString or SecretBinary.")
	}

	if guardErr := checkToken(in.ClientRequestToken); guardErr != nil {
		return nil, guardErr
	}

	stages := in.VersionStages
	if stages == nil {
		stages = []string{stageCurrent}
	}

	if len(stages) > maxStages {
		return nil, invalidParameter("VersionStages can list at most %d labels.", maxStages)
	}

	for _, stage := range stages {
		if guardErr := checkStage(stage); guardErr != nil {
			return nil, guardErr
		}
	}

	key, err := s.keyOf(ctx, m.Scope, in.SecretId)
	if err != nil {
		return nil, err
	}

	if keyErr := s.useKey(ctx, m.Scope, key, "EncryptionFailure"); keyErr != nil {
		return nil, keyErr
	}

	now := s.now()
	var out *api.PutSecretValueResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, findErr := findLive(tx, m.Scope, in.SecretId, now)
		if findErr != nil {
			return findErr
		}

		id, addErr := addVersion(tx, m.Scope, sec, in.ClientRequestToken, value, stages, now.UnixMilli())
		if addErr != nil {
			return addErr
		}

		out = &api.PutSecretValueResponse{
			ARN: new(sec.arn(m.Scope)), Name: new(sec.Name), VersionId: new(id), VersionStages: sec.stagesOf()[id],
		}

		return storage.Write(tx, secretKey(m.Scope, sec.Name), sec)
	})

	return out, err
}

// GetSecretValue returns the value of the version that VersionId or
// VersionStage (AWSCURRENT by default) selects; when both are given they must
// agree.
func (s *Service) GetSecretValue(
	ctx context.Context,
	in *api.GetSecretValueRequest,
) (*api.GetSecretValueResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	request.ReferenceScope(&m, "secretsmanager", common.Value(in.SecretId))
	var out *api.GetSecretValueResponse
	var key string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		sec, findErr := findLive(tx, m.Scope, in.SecretId, s.now())
		if findErr != nil {
			return findErr
		}

		key = sec.KmsKeyID

		id, resolveErr := sec.resolve(in.VersionId, in.VersionStage)
		if resolveErr != nil {
			return resolveErr
		}

		value, _, readErr := storage.Read[payload](tx, valueKey(m.Scope, sec.Name, id))
		if readErr != nil {
			return readErr
		}

		out = &api.GetSecretValueResponse{
			ARN: new(sec.arn(m.Scope)), Name: new(sec.Name), VersionId: new(id), SecretString: value.String,
			SecretBinary: value.Binary, VersionStages: sec.stagesOf()[id],
		}
		for _, v := range sec.Versions {
			if v.ID == id {
				out.CreatedDate = seconds(v.CreatedAt)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if keyErr := s.useKey(ctx, m.Scope, key, "DecryptionFailure"); keyErr != nil {
		return nil, keyErr
	}

	return out, nil
}

// resolve returns the version ID that a version ID and staging label select.
func (s *secret) resolve(versionID, stage *string) (string, error) {
	if versionID != nil && !s.hasVersion(*versionID) {
		return "", notFound("Secrets Manager can't find the specified secret value for VersionId: %s", *versionID)
	}

	if versionID != nil && stage == nil {
		return *versionID, nil
	}

	label := value0(stage)
	if label == "" {
		label = stageCurrent
	}

	id, ok := s.Stages[label]
	if !ok || (versionID != nil && id != *versionID) {
		return "", notFound("Secrets Manager can't find the specified secret value for staging label: %s", label)
	}

	return id, nil
}

// ListSecretVersionIds lists versions in creation order, with versions created
// in the same millisecond ordered by ID: those with a staging label, and with
// IncludeDeprecated the rest too.
func (s *Service) ListSecretVersionIds(
	ctx context.Context,
	in *api.ListSecretVersionIdsRequest,
) (*api.ListSecretVersionIdsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	request.ReferenceScope(&m, "secretsmanager", common.Value(in.SecretId))
	var out *api.ListSecretVersionIdsResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		sec, findErr := find(tx, m.Scope, in.SecretId, s.now())
		if findErr != nil {
			return findErr
		}

		stages := sec.stagesOf()
		var versions []versionInfo
		for _, v := range sec.Versions {
			if len(stages[v.ID]) > 0 || (in.IncludeDeprecated != nil && *in.IncludeDeprecated) {
				versions = append(versions, v)
			}
		}

		key := func(v versionInfo) string { return fmt.Sprintf("%016d/%s", v.CreatedAt, v.ID) }
		selected, next, pageErr := page(versions, key, in.NextToken, in.MaxResults, false)
		if pageErr != nil {
			return pageErr
		}

		out = &api.ListSecretVersionIdsResponse{
			ARN: new(sec.arn(m.Scope)), Name: new(sec.Name), NextToken: next,
			Versions: []api.SecretVersionsListEntry{},
		}
		for _, v := range selected {
			out.Versions = append(out.Versions, api.SecretVersionsListEntry{
				VersionId: new(v.ID), VersionStages: stages[v.ID], CreatedDate: seconds(v.CreatedAt),
			})
		}

		return nil
	})

	return out, err
}

// UpdateSecretVersionStage moves a staging label to MoveToVersionId, or
// removes it. A label that a version already has moves only when
// RemoveFromVersionId names that version, and AWSCURRENT can only move.
func (s *Service) UpdateSecretVersionStage(
	ctx context.Context,
	in *api.UpdateSecretVersionStageRequest,
) (*api.UpdateSecretVersionStageResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	request.ReferenceScope(&m, "secretsmanager", common.Value(in.SecretId))
	stage := value0(in.VersionStage)
	if guardErr := checkStage(stage); guardErr != nil {
		return nil, guardErr
	}

	now := s.now()
	var out *api.UpdateSecretVersionStageResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, findErr := findLive(tx, m.Scope, in.SecretId, now)
		if findErr != nil {
			return findErr
		}

		if moveErr := sec.moveStage(stage, in.RemoveFromVersionId, in.MoveToVersionId); moveErr != nil {
			return moveErr
		}

		sec.ChangedAt = now.UnixMilli()
		out = &api.UpdateSecretVersionStageResponse{ARN: new(sec.arn(m.Scope)), Name: new(sec.Name)}
		return storage.Write(tx, secretKey(m.Scope, sec.Name), sec)
	})

	return out, err
}

func (s *secret) moveStage(stage string, removeFrom, moveTo *string) error {
	for _, id := range []*string{removeFrom, moveTo} {
		if id != nil && !s.hasVersion(*id) {
			return notFound("Secrets Manager can't find the specified secret version: %s", *id)
		}
	}

	holder, held := s.Stages[stage]
	if held && (removeFrom == nil || *removeFrom != holder) && (moveTo == nil || *moveTo != holder) {
		return invalidParameter("The staging label %s is attached to version %s, which RemoveFromVersionId must "+
			"name.", stage, holder)
	}

	if moveTo == nil {
		if removeFrom == nil {
			return invalidParameter("Specify RemoveFromVersionId, MoveToVersionId, or both.")
		}

		if stage == stageCurrent {
			return invalidParameter("You can't remove the staging label AWSCURRENT without moving it to " +
				"another version.")
		}

		if held {
			delete(s.Stages, stage)
		}

		return nil
	}

	s.setStage(stage, *moveTo)
	return s.checkStageLimit()
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.retag(ctx, in.SecretId, func(tags map[string]string) error {
		merged, mergeErr := tagging.Merge(tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		maps.Copy(tags, merged)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	err := s.retag(ctx, in.SecretId, func(tags map[string]string) error {
		tagging.Remove(tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

// retag applies fn to a secret's tags and saves them.
func (s *Service) retag(ctx context.Context, id *string, fn func(tags map[string]string) error) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	request.ReferenceScope(&m, "secretsmanager", common.Value(id))
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, findErr := findLive(tx, m.Scope, id, s.now())
		if findErr != nil {
			return findErr
		}

		if sec.Tags == nil {
			sec.Tags = map[string]string{}
		}

		if fnErr := fn(sec.Tags); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, secretKey(m.Scope, sec.Name), sec)
	})
}
