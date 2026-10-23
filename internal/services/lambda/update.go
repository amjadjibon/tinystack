package lambda

import (
	"context"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// UpdateFunctionCode replaces the code of $LATEST, from ZipFile, an S3 object,
// or an image. Warm environments of the old code are released. With DryRun it
// only validates.
func (s *Service) UpdateFunctionCode(
	ctx context.Context,
	in *api.UpdateFunctionCodeRequest,
) (*api.FunctionConfiguration, error) {
	zipFile, directory, err := s.functionCode(ctx, in.ZipFile, in.S3Bucket, in.S3Key, in.S3ObjectVersion)
	if err != nil {
		return nil, err
	}

	return s.update(ctx, in.FunctionName, in.RevisionId, common.Value(in.DryRun), common.Value(in.Publish), func(
		fn *function,
	) ([]byte, error) {
		if err := fn.setArchitecture(in.Architectures); err != nil {
			return nil, err
		}

		return fn.setCode(zipFile, in.ImageUri, directory)
	})
}

// UpdateFunctionConfiguration changes the settings of $LATEST; fields left
// out keep their values, and an empty Environment removes every variable.
func (s *Service) UpdateFunctionConfiguration(
	ctx context.Context,
	in *api.UpdateFunctionConfigurationRequest,
) (*api.FunctionConfiguration, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	// Layers are resolved first, outside the update's transaction; an empty
	// list removes them.
	var layers []functionLayer
	if in.Layers != nil {
		if layers, err = s.functionLayers(ctx, &m, in.Layers); err != nil {
			return nil, err
		}
	}

	return s.update(ctx, in.FunctionName, in.RevisionId, false, false, func(fn *function) ([]byte, error) {
		if in.Layers != nil {
			if len(layers) > 0 && fn.PackageType == packageImage {
				return nil, invalid("Layers are not supported for Image functions")
			}

			fn.Layers = layers
		}

		return nil, fn.configure(&settings{
			ImageConfig:      in.ImageConfig,
			DeadLetterConfig: in.DeadLetterConfig,
			Role:             in.Role,
			Handler:          in.Handler,
			Description:      in.Description,
			Runtime:          in.Runtime,
			Timeout:          in.Timeout,
			MemorySize:       in.MemorySize,
			Environment:      in.Environment,
		})
	})
}

// update applies change, which returns new code or nil, to the caller's
// function in one transaction when revision (if set) is still current, gives
// it a new revision, and with publish publishes it as a version, which it
// returns. Unless dryRun, it then releases the function's warm environments.
func (s *Service) update(
	ctx context.Context,
	raw, revision *string,
	dryRun, publishing bool,
	change func(*function) ([]byte, error),
) (*api.FunctionConfiguration, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, q, err := resolveName(&m, common.Value(raw))
	if err != nil {
		return nil, err
	}

	if guardErr := unqualified(q); guardErr != nil {
		return nil, guardErr
	}

	var fn *function
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := m.Key("lambda", kindFunction, name)
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
			return revisionMismatch()
		}

		code, changeErr := change(fn)
		if changeErr != nil || dryRun {
			return changeErr
		}

		if fn.HotReloadPath != "" {
			tx.Delete(m.Key("lambda", kindCode, name))
		} else if code != nil {
			tx.Put(m.Key("lambda", kindCode, name), code)
		}

		fn.RevisionID = uuid.NewV4().String()
		fn.LastModified = s.now().UTC().Format(timeFormat)
		if writeErr := writeJSON(tx, key, fn); writeErr != nil || !publishing {
			return writeErr
		}

		var publishErr error
		fn, publishErr = publish(tx, m.Scope, fn, nil, fn.LastModified)
		return publishErr
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
