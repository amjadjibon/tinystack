package secretsmanager

import (
	"context"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateManagedSecret creates a service-managed secret for another AWS service, such as EventBridge.
// It bypasses the public secret name validation, but otherwise stores the secret identically to
// CreateSecret: standard record layout, ARN with a random 6-character suffix, and OwningService set.
func (s *Service) CreateManagedSecret(
	ctx context.Context,
	scope request.Scope,
	name, owningService, description string,
	secretString *string,
) (string, error) {
	now := s.now()
	var arn string
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, exists, loadErr := load(tx, scope, name)
		if loadErr != nil {
			return loadErr
		}

		switch {
		case exists && expired(existing, now):
			purge(tx, scope, existing)
		case exists && existing.scheduled():
			return invalidRequest("You can't create this secret because a secret with this name is already " +
				"scheduled for deletion.")
		case exists:
			return awserror.New("ResourceExistsException",
				fmt.Sprintf("The operation failed because the secret %s already exists.", name))
		}

		created := &secret{
			Name:          name,
			Suffix:        newSuffix(),
			Description:   description,
			OwningService: owningService,
			CreatedAt:     now.UnixMilli(),
			ChangedAt:     now.UnixMilli(),
		}
		arn = created.arn(scope)
		if secretString != nil {
			v := &payload{String: secretString}
			_, addErr := addVersion(tx, scope, created, nil, v, []string{stageCurrent}, now.UnixMilli())
			if addErr != nil {
				return addErr
			}
		}

		return storage.Write(tx, secretKey(scope, created.Name), created)
	})
	if err != nil {
		return "", err
	}

	return arn, nil
}

// UpdateManagedSecret updates the SecretString of a service-managed secret.
func (s *Service) UpdateManagedSecret(
	ctx context.Context,
	scope request.Scope,
	arnOrName string,
	secretString *string,
) error {
	now := s.now()
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, err := find(tx, scope, &arnOrName, now)
		if err != nil {
			return err
		}

		sec.ChangedAt = now.UnixMilli()
		if secretString != nil {
			v := &payload{String: secretString}
			_, addErr := addVersion(tx, scope, sec, nil, v, []string{stageCurrent}, now.UnixMilli())
			if addErr != nil {
				return addErr
			}
		}

		return storage.Write(tx, secretKey(scope, sec.Name), sec)
	})
}

// DeleteManagedSecret deletes a service-managed secret and its versions immediately.
func (s *Service) DeleteManagedSecret(
	ctx context.Context,
	scope request.Scope,
	arnOrName string,
) error {
	now := s.now()
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, err := find(tx, scope, &arnOrName, now)
		if err != nil {
			return err
		}

		purge(tx, scope, sec)
		return nil
	})
}

// GetManagedSecretValue returns the current SecretString of a service-managed secret.
func (s *Service) GetManagedSecretValue(
	ctx context.Context,
	scope request.Scope,
	arnOrName string,
) (string, error) {
	now := s.now()
	var val string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		sec, err := findLive(tx, scope, &arnOrName, now)
		if err != nil {
			return err
		}

		id, err := sec.resolve(nil, nil)
		if err != nil {
			return err
		}

		p, ok, err := storage.Read[payload](tx, valueKey(scope, sec.Name, id))
		if err != nil {
			return err
		}

		if ok && p.String != nil {
			val = *p.String
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return val, nil
}
