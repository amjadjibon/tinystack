package codebuild

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) resolve(ctx context.Context, kind, ref string) (string, error) {
	if kind == plaintext || kind == "" {
		return ref, nil
	}

	if s.caller == nil {
		return "", fmt.Errorf("secret resolver unavailable")
	}

	if kind == parameterStore {
		out, err := s.caller.Call(ctx, "ssm", "GetParameter", map[string]any{"Name": ref, "WithDecryption": true})
		if err != nil {
			return "", fmt.Errorf("parameter resolution failed")
		}

		parameter, ok := out["Parameter"].(map[string]any)
		if !ok {
			return "", fmt.Errorf("parameter has no value")
		}

		value, ok := parameter["Value"].(string)
		if !ok {
			return "", fmt.Errorf("parameter has no value")
		}

		return value, nil
	}

	if kind != secretsManager {
		return "", fmt.Errorf("unsupported variable type")
	}

	parts := strings.Split(ref, ":")
	if len(parts) > 4 {
		return "", fmt.Errorf("secret references support name:json-key:version-stage:version-id")
	}

	params := map[string]any{"SecretId": parts[0]}
	if len(parts) > 2 && parts[2] != "" {
		params["VersionStage"] = parts[2]
	}

	if len(parts) > 3 && parts[3] != "" {
		params["VersionId"] = parts[3]
	}

	out, err := s.caller.Call(ctx, "secretsmanager", "GetSecretValue", params)
	if err != nil {
		return "", fmt.Errorf("secret resolution failed")
	}

	value, ok := out["SecretString"].(string)
	if !ok {
		return "", fmt.Errorf("secret has no string value")
	}

	if len(parts) > 1 && parts[1] != "" {
		var object map[string]json.RawMessage
		if err = json.Unmarshal([]byte(value), &object); err != nil {
			return "", fmt.Errorf("secret is not a JSON object")
		}

		raw, found := object[parts[1]]
		if !found {
			return "", fmt.Errorf("secret JSON key missing")
		}

		if err = json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("secret JSON value is not a string")
		}
	}

	return value, nil
}

func (s *Service) environment(ctx context.Context, r *buildRecord, spec *buildspec) (map[string]string, error) {
	type variable struct{ kind, ref string }
	variables := map[string]variable{}
	for kind, values := range map[string]map[string]string{
		plaintext: spec.Env.Variables, parameterStore: spec.Env.Parameters, secretsManager: spec.Env.Secrets,
	} {
		for name, ref := range values {
			variables[name] = variable{kind, ref}
		}
	}

	// Resolve only effective values; project and request variables win over the buildspec.
	for _, v := range r.Project.Environment.EnvironmentVariables {
		variables[common.Value(v.Name)] = variable{common.Value(v.Type), common.Value(v.Value)}
	}

	env := map[string]string{}
	for name, v := range variables {
		value, err := s.resolve(ctx, v.kind, v.ref)
		if err != nil {
			return nil, fmt.Errorf("environment variable %s: %w", name, err)
		}

		env[name] = value
	}

	endpoint := s.endpoint
	if r.Endpoint != "" {
		endpoint = containerEndpoint(r.Endpoint)
	}

	builtins := map[string]string{
		"AWS_REGION":             r.Scope.Region,
		"AWS_DEFAULT_REGION":     r.Scope.Region,
		"AWS_ENDPOINT_URL":       endpoint,
		"AWS_ACCESS_KEY_ID":      r.Scope.AccountID,
		"AWS_SECRET_ACCESS_KEY":  "test",
		"CODEBUILD_BUILD_ID":     common.Value(r.Build.Id),
		"CODEBUILD_BUILD_ARN":    common.Value(r.Build.Arn),
		"CODEBUILD_BUILD_NUMBER": strconv.FormatInt(common.Value(r.Build.BuildNumber), 10),
		"CODEBUILD_PROJECT_ARN": common.Value(
			r.Project.Arn,
		),
		"CODEBUILD_BUILD_IMAGE": common.Value(r.Build.Environment.Image),
		"CODEBUILD_INITIATOR": common.Value(
			r.Build.Initiator,
		),
		"CODEBUILD_SOURCE_VERSION":   common.Value(r.Build.SourceVersion),
		"CODEBUILD_SRC_DIR":          "/codebuild/output/src",
		"CODEBUILD_START_TIME":       strconv.FormatInt(int64(common.Value(r.Build.StartTime)*1000), 10),
		"CODEBUILD_BUILD_SUCCEEDING": "1",
		"HOME":                       "/root",
	}

	if common.Value(r.Project.Source.Type) == s3Type {
		builtins["CODEBUILD_SOURCE_REPO_URL"] = "s3://" + common.Value(r.Project.Source.Location)
	}

	for k, v := range builtins {
		env[k] = v
	}

	return env, nil
}
