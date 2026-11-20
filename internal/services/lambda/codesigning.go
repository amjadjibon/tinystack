package lambda

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindCodeSigning = "code-signing-config"
	signingWarn     = "Warn"
	signingEnforce  = "Enforce"
)

var (
	codeSigningARN = regexp.MustCompile(
		`^arn:aws[a-zA-Z-]*:lambda:([^:]+):(\d{12}):code-signing-config:(csc-[a-z0-9]{17})$`,
	)
	signerVersionARN = regexp.MustCompile(
		`^arn:aws[a-zA-Z-]*:signer:[a-z0-9-]+:\d{12}:/signing-profiles/[a-zA-Z0-9_]{2,64}/[a-zA-Z0-9]{10}$`,
	)
)

type signingConfig struct {
	Config api.CodeSigningConfig
	Tags   map[string]string `json:",omitempty"`
}

func signingKey(scope request.Scope, arn string) (string, error) {
	match := codeSigningARN.FindStringSubmatch(arn)
	if len(match) == 0 || len(arn) > 200 {
		return "", invalid("Invalid CodeSigningConfigArn")
	}

	if match[1] != scope.Region || match[2] != scope.AccountID {
		return "", notFound("Code signing config " + arn)
	}

	return scope.Key("lambda", kindCodeSigning, match[3]), nil
}

func readSigningConfig(tx storage.ReadTx, key, arn string) (*signingConfig, error) {
	cfg, found, err := storage.Read[signingConfig](tx, key)
	if err == nil && !found {
		err = notFound("Code signing config " + arn)
	}

	return cfg, err
}

func (c *signingConfig) configure(
	description *string, allowed *api.AllowedPublishers, policy *api.CodeSigningPolicies, modified string,
) error {
	if description != nil {
		if utf8.RuneCountInString(*description) > 256 {
			return invalid("Description must be at most 256 characters")
		}

		c.Config.Description = description
	}

	if allowed != nil {
		if len(allowed.SigningProfileVersionArns) < 1 || len(allowed.SigningProfileVersionArns) > 20 {
			return invalid("SigningProfileVersionArns must contain between 1 and 20 entries")
		}

		for _, arn := range allowed.SigningProfileVersionArns {
			if !signerVersionARN.MatchString(arn) {
				return invalid("Invalid signing profile version ARN: %s", arn)
			}
		}

		c.Config.AllowedPublishers = allowed
	}

	if policy != nil {
		mode := *common.OrElse(policy.UntrustedArtifactOnDeployment, new(signingWarn))
		if mode != signingWarn && mode != signingEnforce {
			return invalid("UntrustedArtifactOnDeployment must be Warn or Enforce")
		}

		c.Config.CodeSigningPolicies = &api.CodeSigningPolicies{UntrustedArtifactOnDeployment: new(mode)}
	}

	c.Config.LastModified = new(modified)
	return nil
}

func (s *Service) CreateCodeSigningConfig(
	ctx context.Context, in *api.CreateCodeSigningConfigRequest,
) (*api.CreateCodeSigningConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.AllowedPublishers == nil {
		return nil, invalid("AllowedPublishers is required")
	}

	if tagErr := tagging.Check(in.Tags); tagErr != nil {
		return nil, invalid("Invalid tags: %v", tagErr)
	}

	cfg := signingConfig{Config: api.CodeSigningConfig{
		CodeSigningPolicies: &api.CodeSigningPolicies{UntrustedArtifactOnDeployment: new(signingWarn)},
	}, Tags: in.Tags}
	if configErr := cfg.configure(
		in.Description,
		in.AllowedPublishers,
		in.CodeSigningPolicies,
		s.now().UTC().Format(timeFormat),
	); configErr != nil {
		return nil, configErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for {
			id := "csc-" + common.RandomID(17)
			key := m.Key("lambda", kindCodeSigning, id)
			if _, exists := tx.Get(key); exists {
				continue
			}

			cfg.Config.CodeSigningConfigId = new(id)
			cfg.Config.CodeSigningConfigArn = new(
				"arn:aws:lambda:" + m.Region + ":" + m.AccountID + ":" + kindCodeSigning + ":" + id,
			)
			return storage.Write(tx, key, &cfg)
		}
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateCodeSigningConfigResponse{CodeSigningConfig: &cfg.Config}, nil
}

func (s *Service) GetCodeSigningConfig(
	ctx context.Context, in *api.GetCodeSigningConfigRequest,
) (*api.GetCodeSigningConfigResponse, error) {
	cfg, err := s.signingSetting(ctx, in.CodeSigningConfigArn, nil)
	if err != nil {
		return nil, err
	}

	return &api.GetCodeSigningConfigResponse{CodeSigningConfig: &cfg.Config}, nil
}

func (s *Service) UpdateCodeSigningConfig(
	ctx context.Context, in *api.UpdateCodeSigningConfigRequest,
) (*api.UpdateCodeSigningConfigResponse, error) {
	cfg, err := s.signingSetting(ctx, in.CodeSigningConfigArn, func(cfg *signingConfig) error {
		return cfg.configure(
			in.Description,
			in.AllowedPublishers,
			in.CodeSigningPolicies,
			s.now().UTC().Format(timeFormat),
		)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateCodeSigningConfigResponse{CodeSigningConfig: &cfg.Config}, nil
}

func (s *Service) signingSetting(
	ctx context.Context, raw *string, change func(*signingConfig) error,
) (*signingConfig, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	arn := common.Value(raw)
	key, err := signingKey(m.Scope, arn)
	if err != nil {
		return nil, err
	}

	var cfg *signingConfig
	if change == nil {
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			var readErr error
			cfg, readErr = readSigningConfig(tx, key, arn)
			return readErr
		})
	} else {
		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			var readErr error
			cfg, readErr = readSigningConfig(tx, key, arn)
			if readErr != nil {
				return readErr
			}

			if changeErr := change(cfg); changeErr != nil {
				return changeErr
			}

			return storage.Write(tx, key, cfg)
		})
	}

	return cfg, err
}

func (s *Service) DeleteCodeSigningConfig(
	ctx context.Context, in *api.DeleteCodeSigningConfigRequest,
) (*api.DeleteCodeSigningConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.CodeSigningConfigArn)
	key, err := signingKey(m.Scope, arn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, readErr := readSigningConfig(tx, key, arn); readErr != nil {
			return readErr
		}

		functions, readErr := functionsBySigningConfig(tx, m.Scope, arn)
		if readErr != nil {
			return readErr
		}

		if len(functions) != 0 {
			return conflict("Code signing config is associated with one or more functions")
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteCodeSigningConfigResponse{}, nil
}

func (s *Service) ListCodeSigningConfigs(
	ctx context.Context, in *api.ListCodeSigningConfigsRequest,
) (*api.ListCodeSigningConfigsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxItems, maxVersionItems)
	if err != nil {
		return nil, err
	}

	var configs []api.CodeSigningConfig
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[signingConfig](tx, m.Prefix("lambda", kindCodeSigning))
		for _, record := range records {
			configs = append(configs, record.Config)
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	configs, marker, err := common.Page(configs, func(c api.CodeSigningConfig) string {
		return common.Value(c.CodeSigningConfigId)
	}, in.Marker, int(limit), invalid("Invalid Marker"))
	if err != nil {
		return nil, err
	}

	return &api.ListCodeSigningConfigsResponse{CodeSigningConfigs: configs, NextMarker: marker}, nil
}

func functionsBySigningConfig(tx storage.ReadTx, scope request.Scope, arn string) ([]string, error) {
	functions, err := storage.Records[function](tx, scope.Prefix("lambda", kindFunction))
	var arns []string
	for _, fn := range functions {
		if fn.CodeSigningConfigARN == arn {
			arns = append(arns, fn.arn(scope))
		}
	}

	return arns, err
}

func (s *Service) ListFunctionsByCodeSigningConfig(
	ctx context.Context, in *api.ListFunctionsByCodeSigningConfigRequest,
) (*api.ListFunctionsByCodeSigningConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxItems, maxVersionItems)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.CodeSigningConfigArn)
	key, err := signingKey(m.Scope, arn)
	if err != nil {
		return nil, err
	}

	var functions []string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, readErr := readSigningConfig(tx, key, arn); readErr != nil {
			return readErr
		}

		var readErr error
		functions, readErr = functionsBySigningConfig(tx, m.Scope, arn)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	functions, marker, err := common.Page(functions, func(arn string) string { return arn },
		in.Marker, int(limit), invalid("Invalid Marker"))
	if err != nil {
		return nil, err
	}

	return &api.ListFunctionsByCodeSigningConfigResponse{FunctionArns: functions, NextMarker: marker}, nil
}

func (s *Service) GetFunctionCodeSigningConfig(
	ctx context.Context, in *api.GetFunctionCodeSigningConfigRequest,
) (*api.GetFunctionCodeSigningConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, q, err := resolveName(&m, common.Value(in.FunctionName))
	if err != nil {
		return nil, err
	}

	var fn *function
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if q != "" && q != latest {
			if _, _, readErr := resolve(tx, m.Scope, name, q); readErr != nil {
				return readErr
			}
		}

		var readErr error
		fn, _, readErr = resolve(tx, m.Scope, name, "")
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return &api.GetFunctionCodeSigningConfigResponse{
		CodeSigningConfigArn: new(fn.CodeSigningConfigARN), FunctionName: new(fn.Name),
	}, nil
}

func (s *Service) PutFunctionCodeSigningConfig(
	ctx context.Context, in *api.PutFunctionCodeSigningConfigRequest,
) (*api.PutFunctionCodeSigningConfigResponse, error) {
	arn := common.Value(in.CodeSigningConfigArn)
	out := &api.PutFunctionCodeSigningConfigResponse{}
	err := s.functionSetting(
		ctx,
		in.FunctionName,
		true,
		func(tx storage.ReadTx, scope request.Scope, fn *function) error {
			if _, err := signingForFunction(tx, scope, fn, arn); err != nil {
				return err
			}

			fn.CodeSigningConfigARN = arn
			out.CodeSigningConfigArn, out.FunctionName = new(arn), new(fn.Name)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteFunctionCodeSigningConfig(
	ctx context.Context, in *api.DeleteFunctionCodeSigningConfigRequest,
) (*api.DeleteFunctionCodeSigningConfigOutput, error) {
	err := s.functionSetting(ctx, in.FunctionName, true, func(_ storage.ReadTx, _ request.Scope, fn *function) error {
		fn.CodeSigningConfigARN = ""
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFunctionCodeSigningConfigOutput{}, nil
}

func signingForFunction(tx storage.ReadTx, scope request.Scope, fn *function, arn string) (*signingConfig, error) {
	if fn.PackageType != packageZip {
		return nil, invalid("Code signing is only supported for Zip functions")
	}

	key, err := signingKey(scope, arn)
	var cfg *signingConfig
	if err == nil {
		cfg, err = readSigningConfig(tx, key, arn)
	}

	if e, ok := errors.AsType[*awserror.Error](err); ok && e.Code == "ResourceNotFoundException" {
		err = awserror.New("CodeSigningConfigNotFoundException", e.Message)
	}

	return cfg, err
}

func checkDeploymentSigning(tx storage.ReadTx, scope request.Scope, fn *function) error {
	if fn.CodeSigningConfigARN == "" {
		return nil
	}

	cfg, err := signingForFunction(tx, scope, fn, fn.CodeSigningConfigARN)
	if err != nil {
		return err
	}

	if common.Value(cfg.Config.CodeSigningPolicies.UntrustedArtifactOnDeployment) == signingEnforce {
		return awserror.New("CodeVerificationFailedException",
			"Code signing enforcement requires a verified deployment package. TinyStack cannot verify signatures.")
	}

	return nil
}

func isSigningARN(raw string) bool {
	return strings.Contains(raw, ":"+kindCodeSigning+":")
}
