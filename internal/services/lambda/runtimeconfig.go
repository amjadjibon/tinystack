package lambda

import (
	"cmp"
	"context"
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const recursionAllow = "Allow"

func (s *Service) GetFunctionRecursionConfig(
	ctx context.Context, in *api.GetFunctionRecursionConfigRequest,
) (*api.GetFunctionRecursionConfigResponse, error) {
	var loop string
	err := s.functionSetting(ctx, in.FunctionName, false, func(_ storage.ReadTx, _ request.Scope, fn *function) error {
		loop = cmp.Or(fn.RecursiveLoop, "Terminate")
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetFunctionRecursionConfigResponse{RecursiveLoop: new(loop)}, nil
}

func (s *Service) PutFunctionRecursionConfig(
	ctx context.Context, in *api.PutFunctionRecursionConfigRequest,
) (*api.PutFunctionRecursionConfigResponse, error) {
	loop := common.Value(in.RecursiveLoop)
	if loop != recursionAllow && loop != "Terminate" {
		return nil, invalid("RecursiveLoop must be Allow or Terminate")
	}

	err := s.functionSetting(ctx, in.FunctionName, true, func(_ storage.ReadTx, _ request.Scope, fn *function) error {
		fn.RecursiveLoop = loop
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutFunctionRecursionConfigResponse{RecursiveLoop: new(loop)}, nil
}

// functionSetting manages controls attached to the unqualified function.
func (s *Service) functionSetting(
	ctx context.Context, raw *string, write bool, apply func(storage.ReadTx, request.Scope, *function) error,
) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	load := func(tx storage.ReadTx) (*function, error) {
		name, q, err := resolveName(&m, common.Value(raw))
		if err != nil {
			return nil, err
		}

		if guardErr := unqualified(q); guardErr != nil {
			return nil, guardErr
		}

		fn, _, err := resolve(tx, m.Scope, name, "")
		return fn, err
	}

	if !write {
		return s.store.View(ctx, func(tx storage.ReadTx) error {
			fn, err := load(tx)
			if err != nil {
				return err
			}

			return apply(tx, m.Scope, fn)
		})
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		fn, err := load(tx)
		if err != nil {
			return err
		}

		if err := apply(tx, m.Scope, fn); err != nil {
			return err
		}

		return writeJSON(tx, m.Key("lambda", kindFunction, fn.Name), fn)
	})
}

var runtimeVersionARN = regexp.MustCompile(`^arn:aws[a-zA-Z-]*:lambda:([^:]+)::runtime:[^\s:]+$`)

func (s *Service) GetRuntimeManagementConfig(
	ctx context.Context, in *api.GetRuntimeManagementConfigRequest,
) (*api.GetRuntimeManagementConfigResponse, error) {
	m, name, q, err := runtimeManagementTarget(ctx, in.FunctionName, in.Qualifier)
	if err != nil {
		return nil, err
	}

	var fn *function
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		fn, _, readErr = resolve(tx, m.Scope, name, q)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	if fn.PackageType == packageImage {
		return nil, invalid("Runtime management is only supported for Zip functions")
	}

	return &api.GetRuntimeManagementConfigResponse{
		UpdateRuntimeOn:   new(cmp.Or(fn.RuntimeUpdateMode, "Auto")),
		FunctionArn:       new(fn.arn(m.Scope)),
		RuntimeVersionArn: fn.RuntimeVersionARN,
	}, nil
}

func (s *Service) PutRuntimeManagementConfig(
	ctx context.Context, in *api.PutRuntimeManagementConfigRequest,
) (*api.PutRuntimeManagementConfigResponse, error) {
	m, name, q, err := runtimeManagementTarget(ctx, in.FunctionName, in.Qualifier)
	if err != nil {
		return nil, err
	}

	mode := common.Value(in.UpdateRuntimeOn)
	switch mode {
	case "Auto", "FunctionUpdate":
		if in.RuntimeVersionArn != nil {
			return nil, invalid("RuntimeVersionArn is only supported with Manual runtime updates")
		}
	case "Manual":
		arn := common.Value(in.RuntimeVersionArn)
		match := runtimeVersionARN.FindStringSubmatch(arn)
		if len(arn) > 2048 || len(match) == 0 || match[1] != m.Region {
			return nil, invalid("Manual runtime updates require a RuntimeVersionArn in the function's Region")
		}
	default:
		return nil, invalid("UpdateRuntimeOn must be Auto, FunctionUpdate, or Manual")
	}

	var fn *function
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var readErr error
		fn, _, readErr = resolve(tx, m.Scope, name, q)
		if readErr != nil {
			return readErr
		}

		if fn.PackageType == packageImage {
			return invalid("Runtime management is only supported for Zip functions")
		}

		fn.RuntimeUpdateMode, fn.RuntimeVersionARN = mode, in.RuntimeVersionArn
		key := m.Key("lambda", kindFunction, name)
		if fn.Version != "" {
			key = m.Key("lambda", kindVersion, versionID(name, fn.Version))
		}

		return writeJSON(tx, key, fn)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutRuntimeManagementConfigResponse{
		UpdateRuntimeOn: new(mode), FunctionArn: new(fn.arn(m.Scope)), RuntimeVersionArn: fn.RuntimeVersionARN,
	}, nil
}

func runtimeManagementTarget(
	ctx context.Context, raw, qualifier *string,
) (m request.Metadata, name, q string, err error) {
	m, err = request.Regional(ctx)
	if err != nil {
		return m, "", "", err
	}

	name, q, err = resolveName(&m, common.Value(raw))
	if err != nil {
		return m, "", "", err
	}

	q, err = withQualifier(q, qualifier)
	if err == nil && q != "" && q != latest && !versionNumber.MatchString(q) {
		err = invalid("Runtime management requires $LATEST or a published version number")
	}

	return m, name, q, err
}

const (
	snapStartPublished = "PublishedVersions"
	snapStartNone      = "None"
)

func (fn *function) configureSnapStart(cfg *api.SnapStart) error {
	if cfg != nil {
		if fn.PackageType != packageZip {
			return invalid("SnapStart is only supported for Zip functions")
		}

		switch common.Value(cfg.ApplyOn) {
		case snapStartNone, snapStartPublished:
			fn.SnapStart = *cfg.ApplyOn
		default:
			return invalid("SnapStart ApplyOn must be None or PublishedVersions")
		}
	}

	if fn.SnapStart == snapStartPublished && !strings.HasPrefix(fn.Runtime, "java") {
		return invalid("SnapStart PublishedVersions is supported only for Java 11 and later Zip functions")
	}

	return nil
}
