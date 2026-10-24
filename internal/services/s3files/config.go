package s3files

import (
	"context"
	"encoding/json"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3files"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// checkPolicy requires a JSON object with a Statement. Policies are stored and returned, never evaluated.
func checkPolicy(policy string) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(policy), &document); err != nil || document == nil {
		return invalid("policy must be a JSON object")
	}

	if _, ok := document["Statement"]; !ok {
		return invalid("policy must contain Statement")
	}

	return nil
}

func (s *Service) PutFileSystemPolicy(
	ctx context.Context,
	in *api.PutFileSystemPolicyRequest,
) (*api.PutFileSystemPolicyResponse, error) {
	if err := checkPolicy(common.Value(in.Policy)); err != nil {
		return nil, err
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		r.Policy = in.Policy

		return storage.Write(tx, fsKey(scope, r.ID), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutFileSystemPolicyResponse{}, nil
}

func (s *Service) GetFileSystemPolicy(
	ctx context.Context,
	in *api.GetFileSystemPolicyRequest,
) (*api.GetFileSystemPolicyResponse, error) {
	var out *api.GetFileSystemPolicyResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		if r.Policy == nil {
			return notFound("File system %s has no policy", r.ID)
		}

		out = &api.GetFileSystemPolicyResponse{FileSystemId: new(r.ID), Policy: r.Policy}
		return nil
	})

	return out, err
}

func (s *Service) DeleteFileSystemPolicy(
	ctx context.Context,
	in *api.DeleteFileSystemPolicyRequest,
) (*api.DeleteFileSystemPolicyOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		if r.Policy == nil {
			return notFound("File system %s has no policy", r.ID)
		}

		r.Policy = nil

		return storage.Write(tx, fsKey(scope, r.ID), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFileSystemPolicyOutput{}, nil
}

// PutSynchronizationConfiguration stores the rules at the next version. The first configuration omits
// latestVersionNumber; every later one must repeat the current version or fail with ConflictException.
func (s *Service) PutSynchronizationConfiguration(
	ctx context.Context,
	in *api.PutSynchronizationConfigurationRequest,
) (*api.PutSynchronizationConfigurationResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		next := &syncConfig{Version: 1, Import: in.ImportDataRules, Expiration: in.ExpirationDataRules}
		switch {
		case r.Sync == nil && in.LatestVersionNumber != nil:
			return conflict("File system %s has no synchronization configuration; omit latestVersionNumber", r.ID)
		case r.Sync != nil && in.LatestVersionNumber == nil:
			return conflict(
				"File system %s has synchronization configuration version %d; pass it as latestVersionNumber",
				r.ID,
				r.Sync.Version,
			)
		case r.Sync != nil && *in.LatestVersionNumber != r.Sync.Version:
			return conflict("latestVersionNumber %d does not match the current version %d",
				*in.LatestVersionNumber, r.Sync.Version)
		case r.Sync != nil:
			next.Version = r.Sync.Version + 1
		}

		r.Sync = next

		return storage.Write(tx, fsKey(scope, r.ID), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutSynchronizationConfigurationResponse{}, nil
}

func (s *Service) GetSynchronizationConfiguration(
	ctx context.Context,
	in *api.GetSynchronizationConfigurationRequest,
) (*api.GetSynchronizationConfigurationResponse, error) {
	var out *api.GetSynchronizationConfigurationResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		if r.Sync == nil {
			return notFound("File system %s has no synchronization configuration", r.ID)
		}

		out = &api.GetSynchronizationConfigurationResponse{
			LatestVersionNumber: new(r.Sync.Version),
			ImportDataRules:     r.Sync.Import,
			ExpirationDataRules: r.Sync.Expiration,
		}

		return nil
	})

	return out, err
}
