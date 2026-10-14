package kms

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// EnsureManagedKey creates the AWS managed key alias/aws/<service> in scope
// when the account has none yet, as AWS creates it on first use. It is for
// services that encrypt with the default key, and runs outside any
// transaction.
func (s *Service) EnsureManagedKey(ctx context.Context, scope request.Scope, service string) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		name := "aws/" + service
		if _, found := tx.Get(aliasKey(scope, name)); found {
			return nil
		}

		material, err := newMaterial(symmetricDefault)
		if err != nil {
			return err
		}

		now := s.now().UnixMilli()
		k := &key{
			ID: newKeyID(), State: stateEnabled, Material: material, Created: now, Managed: true,
			Description: "Default key that protects my " + service + " data when no other key is defined",
			Policy:      defaultPolicy(scope),
		}
		if writeErr := storage.Write(tx, keyKey(scope, k.ID), k); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, aliasKey(scope, name), &alias{Name: name, Target: k.ID, Created: now, Updated: now})
	})
}
