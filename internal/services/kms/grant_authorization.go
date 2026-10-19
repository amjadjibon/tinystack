package kms

import (
	"context"
	"encoding/base64"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Grants are immediately consistent. Tokens on grant operations carry no
// additional authority; validate the AWS model's list and string lengths.
func validateGrantTokens(input any) error {
	tokens := catalog.InputStrings(input, "GrantTokens")
	if len(tokens) > 10 {
		return awserror.Validation("GrantTokens must contain at most 10 tokens.")
	}

	for _, token := range tokens {
		if token == "" || len(token) > 8192 {
			return awserror.Validation("Grant tokens must be 1 to 8192 characters.")
		}
	}

	return nil
}

func retirementTarget(in *api.RetireGrantRequest) (keyRef, grantID string, tokenErr error) {
	if in.GrantToken == nil {
		return common.Value(in.KeyId), common.Value(in.GrantId), nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(*in.GrantToken)
	ref, id := string(decoded), ""
	if index := strings.LastIndex(ref, "/"); index >= 0 {
		ref, id = ref[:index], ref[index+1:]
	}

	if err != nil || id == "" || (!keyID.MatchString(ref) && !strings.HasPrefix(ref, "arn:aws:kms:")) {
		return "", "", awserror.New("InvalidGrantTokenException", "The grant token is not valid.")
	}

	return ref, id, nil
}

func containsContext(actual, required map[string]string) bool {
	for name, value := range required {
		if actualValue, ok := actual[name]; !ok || actualValue != value {
			return false
		}
	}

	return true
}

// narrowerConstraints checks implication: every context allowed by the child
// must be allowed by its parent. Equals cannot become Subset or gain pairs.
func narrowerConstraints(parent, child *api.GrantConstraints) bool {
	if parent == nil {
		return true
	}

	if child == nil {
		return len(parent.EncryptionContextEquals) == 0 && len(parent.EncryptionContextSubset) == 0
	}

	if len(parent.EncryptionContextEquals) > 0 {
		return len(child.EncryptionContextSubset) == 0 &&
			maps.Equal(parent.EncryptionContextEquals, child.EncryptionContextEquals)
	}

	actual := child.EncryptionContextSubset
	if len(child.EncryptionContextEquals) > 0 {
		actual = child.EncryptionContextEquals
	}

	return containsContext(actual, parent.EncryptionContextSubset)
}

func requestEncryptionContext(input any, action string) (map[string]string, bool) {
	switch in := input.(type) {
	case *api.EncryptRequest:
		return in.EncryptionContext, true
	case *api.DecryptRequest:
		return in.EncryptionContext, true
	case *api.GenerateDataKeyRequest:
		return in.EncryptionContext, true
	case *api.GenerateDataKeyWithoutPlaintextRequest:
		return in.EncryptionContext, true
	case *api.GenerateDataKeyPairRequest:
		return in.EncryptionContext, true
	case *api.GenerateDataKeyPairWithoutPlaintextRequest:
		return in.EncryptionContext, true
	case *api.ReEncryptRequest:
		if action == opReEncryptFrom {
			return in.SourceEncryptionContext, true
		}

		return in.DestinationEncryptionContext, true
	default:
		return nil, false
	}
}

func (g *grant) permits(input any, operation string) bool {
	if operation == opCreateGrant {
		in, ok := input.(*api.CreateGrantRequest)
		if !ok || len(in.Operations) == 0 {
			return false
		}

		for _, operation := range in.Operations {
			if !slices.Contains(g.Operations, operation) {
				return false
			}
		}

		return narrowerConstraints(g.Constraints, in.Constraints)
	}

	if g.Constraints == nil || operation == opDescribeKey || operation == opRetireGrant {
		return true
	}

	if len(g.Constraints.EncryptionContextEquals) == 0 && len(g.Constraints.EncryptionContextSubset) == 0 {
		return true
	}

	actual, supported := requestEncryptionContext(input, operation)
	if !supported {
		return false
	}

	if len(g.Constraints.EncryptionContextEquals) > 0 {
		return maps.Equal(actual, g.Constraints.EncryptionContextEquals)
	}

	return containsContext(actual, g.Constraints.EncryptionContextSubset)
}

func evaluateGrants(
	ctx context.Context, tx storage.ReadTx, scope request.Scope, id, operation string, input any,
) (*catalog.GrantAuthorization, error) {
	result := &catalog.GrantAuthorization{Reason: "no matching KMS grant allows the operation"}
	m, _ := request.FromContext(ctx)
	if m.Principal == nil {
		return result, nil
	}

	principal := m.Principal
	retirementID := ""
	if in, ok := input.(*api.RetireGrantRequest); ok {
		_, retirementID, _ = retirementTarget(in)
		result.Retirement = true
	}

	for _, record := range tx.Keys(grantPrefix(scope, id)) {
		g, _, err := storage.Read[grant](tx, record)
		if err != nil {
			return nil, err
		}

		if result.Retirement && g.ID != retirementID {
			continue
		}

		grantee, matchErr := policy.MatchAWSPrincipal(g.Grantee, principal)
		if matchErr != nil {
			result.Reason = matchErr.Error()
			continue
		}

		if result.Retirement {
			retiring := false
			if g.Retiring != "" {
				retiring, matchErr = policy.MatchAWSPrincipal(g.Retiring, principal)
				if matchErr != nil {
					result.Reason = matchErr.Error()
					continue
				}
			}

			if retiring || (grantee && slices.Contains(g.Operations, opRetireGrant)) ||
				principal.Account == scope.AccountID {
				result.ID, result.Reason = g.ID, ""
				// Account-root grants and the owning account delegate retirement through IAM.
				result.RequiresIAM = principal.Kind != request.PrincipalRoot &&
					principal.ARN != g.Retiring && principal.PolicyARN != g.Retiring &&
					(!grantee || !slices.Contains(g.Operations, opRetireGrant))
				return result, nil
			}

			continue
		}

		if !grantee || !slices.Contains(g.Operations, operation) {
			continue
		}

		if !g.permits(input, operation) {
			result.Reason = "KMS grant " + g.ID + ": constraints or delegated operations do not permit the request"
			continue
		}

		result.ID, result.Reason = g.ID, ""
		return result, nil
	}

	return result, nil
}
