package lambda

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// UpdateFunctionCode replaces the code of $LATEST. Warm environments of the
// old code are released. With DryRun it only validates.
func (s *Service) UpdateFunctionCode(
	ctx context.Context,
	in *api.UpdateFunctionCodeRequest,
) (*api.FunctionConfiguration, error) {
	return s.update(ctx, in.FunctionName, in.RevisionId, value(in.DryRun), func(fn *function) ([]byte, error) {
		if err := fn.setArchitecture(in.Architectures); err != nil {
			return nil, err
		}

		return fn.setCode(in.ZipFile, in.ImageUri)
	})
}

// UpdateFunctionConfiguration changes the settings of $LATEST; fields left
// out keep their values, and an empty Environment removes every variable.
func (s *Service) UpdateFunctionConfiguration(
	ctx context.Context,
	in *api.UpdateFunctionConfigurationRequest,
) (*api.FunctionConfiguration, error) {
	return s.update(ctx, in.FunctionName, in.RevisionId, false, func(fn *function) ([]byte, error) {
		return nil, fn.configure(&settings{
			Role:        in.Role,
			Handler:     in.Handler,
			Description: in.Description,
			Runtime:     in.Runtime,
			Timeout:     in.Timeout,
			MemorySize:  in.MemorySize,
			Environment: in.Environment,
		})
	})
}

// update applies change, which returns new code or nil, to the caller's
// function in one transaction when revision (if set) is still current, and
// gives it a new revision. Unless dryRun, it then releases the function's warm
// environments.
func (s *Service) update(
	ctx context.Context,
	raw, revision *string,
	dryRun bool,
	change func(*function) ([]byte, error),
) (*api.FunctionConfiguration, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveName(m, value(raw))
	if err != nil {
		return nil, err
	}

	var fn *function
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scopeKey(m.Scope, kindFunction, name)
		var found bool
		var loadErr error
		fn, found, loadErr = loadFunction(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if !found {
			return notFound("Function " + functionARN(m.Scope, name))
		}

		if revision != nil && *revision != fn.RevisionID {
			return awserror.New("PreconditionFailedException", "The Revision Id provided does not match the "+
				"latest Revision Id. Call the GetFunction/GetAlias API to retrieve the latest Revision Id")
		}

		code, changeErr := change(fn)
		if changeErr != nil || dryRun {
			return changeErr
		}

		if code != nil {
			tx.Put(scopeKey(m.Scope, "code", name), code)
		}

		fn.RevisionID = newUUID()
		fn.LastModified = s.now().UTC().Format(timeFormat)
		return writeJSON(tx, key, fn)
	})
	if err != nil {
		return nil, err
	}

	if !dryRun {
		s.runtime.Release(functionARN(m.Scope, name))
	}

	cfg := fn.configuration(m.Scope)
	return &cfg, nil
}
