// Package containerenv resolves environment values for local containers.
package containerenv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// ResolveSecret resolves an ECS-style Secrets Manager ARN or SSM parameter reference.
func ResolveSecret(ctx context.Context, caller *catalog.Caller, scope request.Scope, ref string) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("secret resolver unavailable")
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
	if strings.HasPrefix(ref, "arn:") && strings.Contains(ref, ":secretsmanager:") {
		parts := strings.Split(ref, ":")
		if len(parts) < 7 || len(parts) > 8 {
			return "", fmt.Errorf("invalid secret ARN")
		}

		ctx = request.WithMetadata(ctx, request.Metadata{Scope: request.Scope{Region: parts[3], AccountID: parts[4]}})
		key := ""
		if len(parts) == 8 {
			key = parts[7]
			ref = strings.Join(parts[:7], ":")
		}

		out, err := caller.Base64Blobs().
			Call(ctx, "secretsmanager", "GetSecretValue", map[string]any{"SecretId": ref})
		if err != nil {
			return "", err
		}

		value, ok := out["SecretString"].(string)
		if !ok {
			if encoded, exists := out["SecretBinary"].(string); exists {
				data, decodeErr := base64.StdEncoding.DecodeString(encoded)
				if decodeErr != nil {
					return "", decodeErr
				}

				value = string(data)
			} else {
				return "", fmt.Errorf("secret has no string value")
			}
		}

		if key != "" {
			var object map[string]json.RawMessage
			if parseErr := json.Unmarshal([]byte(value), &object); parseErr != nil {
				return "", parseErr
			}

			raw, found := object[key]
			if !found {
				return "", fmt.Errorf("JSON key missing")
			}

			value = string(raw)
			if len(raw) > 0 && raw[0] == '"' {
				if decodeErr := json.Unmarshal(raw, &value); decodeErr != nil {
					return "", decodeErr
				}
			}
		}

		return value, nil
	}

	parameterARN := false
	if strings.HasPrefix(ref, "arn:") {
		parameterARN = true
		parts := strings.SplitN(ref, ":", 6)
		if len(parts) != 6 || parts[2] != "ssm" || !strings.HasPrefix(parts[5], "parameter/") {
			return "", fmt.Errorf("invalid SSM parameter ARN")
		}

		scope = request.Scope{AccountID: parts[4], Region: parts[3]}
		ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
		ref = "/" + strings.TrimPrefix(parts[5], "parameter/")
	}

	out, err := caller.Call(ctx, "ssm", "GetParameter", map[string]any{"Name": ref, "WithDecryption": true})
	if err != nil && parameterARN {
		var awsErr *awserror.Error
		if errors.As(err, &awsErr) && awsErr.Code == "ParameterNotFound" {
			out, err = caller.Call(ctx, "ssm", "GetParameter",
				map[string]any{"Name": strings.TrimPrefix(ref, "/"), "WithDecryption": true})
		}
	}

	if err != nil {
		return "", err
	}

	parameter, ok := out["Parameter"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("parameter has no value")
	}

	value, ok := parameter["Value"].(string)
	if !ok {
		return "", fmt.Errorf("parameter has no string value")
	}

	return value, nil
}
