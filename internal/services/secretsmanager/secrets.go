package secretsmanager

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateSecret creates a secret, with a first version labeled AWSCURRENT when
// the request sets a value.
func (s *Service) CreateSecret(ctx context.Context, in *api.CreateSecretRequest) (*api.CreateSecretResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.Name == nil || !secretName.MatchString(*in.Name) {
		return nil, invalidParameter("Name must be 1 to 512 characters long and contain only letters, numbers, " +
			"and the symbols /_+=.@-.")
	}

	value, hasValue, err := checkValue(in.SecretString, in.SecretBinary)
	if err != nil {
		return nil, err
	}

	for _, check := range []error{checkToken(in.ClientRequestToken), checkDescription(in.Description)} {
		if check != nil {
			return nil, check
		}
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.useKey(ctx, m.Scope, value0(in.KmsKeyId), "EncryptionFailure")
	if err != nil {
		return nil, err
	}

	now := s.now()
	var out *api.CreateSecretResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, exists, loadErr := load(tx, m.Scope, *in.Name)
		if loadErr != nil {
			return loadErr
		}

		switch {
		case exists && expired(existing, now):
			purge(tx, m.Scope, existing)
		case exists && existing.scheduled():
			return invalidRequest("You can't create this secret because a secret with this name is already " +
				"scheduled for deletion.")
		case exists:
			return awserror.New("ResourceExistsException",
				fmt.Sprintf("The operation failed because the secret %s already exists.", *in.Name))
		}

		created := &secret{
			Tags: tags, Name: *in.Name, Suffix: newSuffix(), Description: value0(in.Description),
			KmsKeyID: value0(in.KmsKeyId), CreatedAt: now.UnixMilli(), ChangedAt: now.UnixMilli(),
		}
		out = &api.CreateSecretResponse{ARN: new(created.arn(m.Scope)), Name: new(created.Name)}
		if hasValue {
			id, addErr := addVersion(tx, m.Scope, created, in.ClientRequestToken, value, []string{stageCurrent},
				now.UnixMilli())
			if addErr != nil {
				return addErr
			}

			out.VersionId = new(id)
		}

		return write(tx, secretKey(m.Scope, created.Name), created)
	})

	return out, err
}

func value0(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

func (s *secret) describe(scope request.Scope) *api.DescribeSecretResponse {
	out := &api.DescribeSecretResponse{
		ARN:                new(s.arn(scope)),
		Name:               new(s.Name),
		Description:        optional(s.Description),
		KmsKeyId:           optional(s.KmsKeyID),
		LastChangedDate:    seconds(s.ChangedAt),
		Tags:               tagList(s.Tags),
		VersionIdsToStages: s.stagesOf(),
		CreatedDate:        seconds(s.CreatedAt),
	}
	if s.scheduled() {
		out.DeletedDate = seconds(s.DeletionAt)
	}

	s.describeRotation(out)
	return out
}

// DescribeSecret returns a secret's metadata, never its value.
func (s *Service) DescribeSecret(
	ctx context.Context,
	in *api.DescribeSecretRequest,
) (*api.DescribeSecretResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeSecretResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		found, findErr := find(tx, m.Scope, in.SecretId, s.now())
		if findErr != nil {
			return findErr
		}

		out = found.describe(m.Scope)
		return nil
	})

	return out, err
}

// matcher reports whether a secret matches one ListSecrets filter.
type matcher func(s *secret) bool

// fields returns the values of s that a filter key compares against.
func fields(s *secret, key string) []string {
	switch key {
	case "name":
		return []string{s.Name}
	case "description":
		return []string{s.Description}
	case "tag-key":
		return slices.Collect(maps.Keys(s.Tags))
	case "tag-value":
		return slices.Collect(maps.Values(s.Tags))
	case "all":
		return slices.Concat(fields(s, "name"), fields(s, "description"), fields(s, "tag-key"),
			fields(s, "tag-value"))
	}

	// TinyStack secrets have no primary region or owning service.
	return nil
}

// parseFilters compiles ListSecrets filters: each value is a prefix, and a
// leading "!" negates it. A secret must match every filter, and any value.
func parseFilters(filters []api.Filter) ([]matcher, error) {
	keys := []string{"description", "name", "tag-key", "tag-value", "primary-region", "owning-service", "all"}
	out := make([]matcher, 0, len(filters))
	for _, f := range filters {
		key := value0(f.Key)
		if !slices.Contains(keys, key) {
			return nil, invalidParameter("Filter key %q is not valid.", key)
		}

		if len(f.Values) == 0 || len(f.Values) > 10 {
			return nil, invalidParameter("A filter must have 1 to 10 values.")
		}

		values := f.Values
		out = append(out, func(s *secret) bool {
			have := fields(s, key)
			return slices.ContainsFunc(values, func(v string) bool {
				prefix, negated := strings.CutPrefix(v, "!")
				matched := slices.ContainsFunc(have, func(h string) bool { return strings.HasPrefix(h, prefix) })
				return matched != negated
			})
		})
	}

	return out, nil
}

// sortDesc lists secrets newest first.
const sortDesc = "desc"

// createdKey orders secrets by creation, then name.
func createdKey(s *secret) string { return fmt.Sprintf("%016d/%s", s.CreatedAt, s.Name) }

// ListSecrets lists secret metadata ordered by creation date, ascending unless
// SortOrder is desc.
func (s *Service) ListSecrets(ctx context.Context, in *api.ListSecretsRequest) (*api.ListSecretsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	matchers, err := parseFilters(in.Filters)
	if err != nil {
		return nil, err
	}

	order := value0(in.SortOrder)
	if order != "" && order != "asc" && order != sortDesc {
		return nil, invalidParameter("SortOrder must be asc or desc.")
	}

	now := s.now()
	out := &api.ListSecretsResponse{SecretList: []api.SecretListEntry{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := secretKey(m.Scope, "")
		var found []*secret
		for _, k := range tx.Keys(prefix) {
			sec, _, readErr := read[secret](tx, k)
			if readErr != nil {
				return readErr
			}

			planned := in.IncludePlannedDeletion != nil && *in.IncludePlannedDeletion
			if expired(sec, now) || (sec.scheduled() && !planned) ||
				slices.ContainsFunc(matchers, func(match matcher) bool { return !match(sec) }) {
				continue
			}

			found = append(found, sec)
		}

		selected, next, pageErr := page(found, createdKey, in.NextToken, in.MaxResults, order == sortDesc)
		if pageErr != nil {
			return pageErr
		}

		for _, sec := range selected {
			d := sec.describe(m.Scope)
			out.SecretList = append(out.SecretList, api.SecretListEntry{
				ARN: d.ARN, Name: d.Name, Description: d.Description, KmsKeyId: d.KmsKeyId,
				LastChangedDate: d.LastChangedDate, DeletedDate: d.DeletedDate, Tags: d.Tags,
				SecretVersionsToStages: d.VersionIdsToStages, CreatedDate: d.CreatedDate,
				RotationEnabled: d.RotationEnabled, RotationLambdaARN: d.RotationLambdaARN,
				RotationRules: d.RotationRules, LastRotatedDate: d.LastRotatedDate,
				NextRotationDate: d.NextRotationDate,
			})
		}

		out.NextToken = next
		return nil
	})

	return out, err
}

// UpdateSecret changes a secret's description or key, and with a value adds
// a version labeled AWSCURRENT.
func (s *Service) UpdateSecret(ctx context.Context, in *api.UpdateSecretRequest) (*api.UpdateSecretResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	value, hasValue, err := checkValue(in.SecretString, in.SecretBinary)
	if err != nil {
		return nil, err
	}

	for _, check := range []error{checkToken(in.ClientRequestToken), checkDescription(in.Description)} {
		if check != nil {
			return nil, check
		}
	}

	// The new value is sealed with the new key, or the secret's current one.
	key := value0(in.KmsKeyId)
	if key == "" && hasValue {
		if key, err = s.keyOf(ctx, m.Scope, in.SecretId); err != nil {
			return nil, err
		}
	}

	if keyErr := s.useKey(ctx, m.Scope, key, "EncryptionFailure"); keyErr != nil {
		return nil, keyErr
	}

	now := s.now()
	var out *api.UpdateSecretResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, findErr := findLive(tx, m.Scope, in.SecretId, now)
		if findErr != nil {
			return findErr
		}

		if in.Description != nil {
			sec.Description = *in.Description
		}

		if in.KmsKeyId != nil {
			sec.KmsKeyID = *in.KmsKeyId
		}

		sec.ChangedAt = now.UnixMilli()
		out = &api.UpdateSecretResponse{ARN: new(sec.arn(m.Scope)), Name: new(sec.Name)}
		if hasValue {
			id, addErr := addVersion(tx, m.Scope, sec, in.ClientRequestToken, value, []string{stageCurrent},
				now.UnixMilli())
			if addErr != nil {
				return addErr
			}

			out.VersionId = new(id)
		}

		return write(tx, secretKey(m.Scope, sec.Name), sec)
	})

	return out, err
}

// DeleteSecret schedules a secret for deletion after a recovery window of 7
// to 30 days, or deletes it at once with ForceDeleteWithoutRecovery.
func (s *Service) DeleteSecret(ctx context.Context, in *api.DeleteSecretRequest) (*api.DeleteSecretResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	force := in.ForceDeleteWithoutRecovery != nil && *in.ForceDeleteWithoutRecovery
	window := int64(maxRecoveryWindow)
	if in.RecoveryWindowInDays != nil {
		if force {
			return nil, invalidParameter("You can't use ForceDeleteWithoutRecovery in conjunction with " +
				"RecoveryWindowInDays.")
		}

		window = *in.RecoveryWindowInDays
		if window < minRecoveryWindow || window > maxRecoveryWindow {
			return nil, invalidParameter("RecoveryWindowInDays must be between %d and %d.", minRecoveryWindow,
				maxRecoveryWindow)
		}
	}

	now := s.now()
	var out *api.DeleteSecretResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, findErr := find(tx, m.Scope, in.SecretId, now)
		if findErr != nil {
			return findErr
		}

		out = &api.DeleteSecretResponse{ARN: new(sec.arn(m.Scope)), Name: new(sec.Name)}
		if force {
			purge(tx, m.Scope, sec)
			out.DeletionDate = seconds(now.UnixMilli())
			return nil
		}

		if sec.scheduled() {
			return errScheduled()
		}

		sec.DeletionAt = now.Add(time.Duration(window) * day).UnixMilli()
		out.DeletionDate = seconds(sec.DeletionAt)
		return write(tx, secretKey(m.Scope, sec.Name), sec)
	})

	return out, err
}

// RestoreSecret cancels a scheduled deletion.
func (s *Service) RestoreSecret(ctx context.Context, in *api.RestoreSecretRequest) (*api.RestoreSecretResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.RestoreSecretResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, findErr := find(tx, m.Scope, in.SecretId, s.now())
		if findErr != nil {
			return findErr
		}

		sec.DeletionAt = 0
		index(tx, m.Scope, sec)
		out = &api.RestoreSecretResponse{ARN: new(sec.arn(m.Scope)), Name: new(sec.Name)}
		return write(tx, secretKey(m.Scope, sec.Name), sec)
	})

	return out, err
}

// GetResourcePolicy reports that a secret has no resource policy: TinyStack
// does not store them.
func (s *Service) GetResourcePolicy(
	ctx context.Context,
	in *api.GetResourcePolicyRequest,
) (*api.GetResourcePolicyResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.GetResourcePolicyResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		sec, findErr := find(tx, m.Scope, in.SecretId, s.now())
		if findErr != nil {
			return findErr
		}

		out = &api.GetResourcePolicyResponse{ARN: new(sec.arn(m.Scope)), Name: new(sec.Name)}
		return nil
	})

	return out, err
}
