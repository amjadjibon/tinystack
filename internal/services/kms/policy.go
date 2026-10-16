package kms

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/iam/policy"
)

// UseAuthorization connects IAM for the policy lockout safety check, which also
// runs when gateway authorization is disabled. Lookups run outside transactions.
func (s *Service) UseAuthorization(lookup func(context.Context, *request.Principal) (iam.Authorization, error)) {
	s.policies = lookup
}

// referenceScope allows ARN-based cross-account access only on the gateway's
// resource authorization path. Bare key IDs and alias names remain caller scoped.
func referenceScope(ctx context.Context, scope request.Scope, ref string) request.Scope {
	m, _ := request.FromContext(ctx)
	parts := strings.SplitN(ref, ":", 6)
	if m.ResourceAuthorization && len(parts) == 6 && parts[0] == "arn" && parts[2] == "kms" &&
		parts[3] == scope.Region && parts[4] != "" {
		scope.AccountID = parts[4]
	}

	return scope
}

func (s *Service) checkLockout(ctx context.Context, scope request.Scope, k *key, document string, bypass bool) error {
	if bypass {
		return nil
	}

	m, _ := request.FromContext(ctx)
	principal := m.Principal
	if principal == nil {
		arn := "arn:aws:iam::" + m.AccountID + ":root"
		principal = &request.Principal{Kind: request.PrincipalRoot, Account: m.AccountID, ARN: arn, PolicyARN: arn}
	}

	values := maps.Clone(m.AuthorizationValues)
	if values == nil {
		values = map[string][]string{
			"aws:principalarn": {principal.PolicyARN}, "aws:principalaccount": {principal.Account},
			"aws:userid": {principal.UserID}, "aws:requestedregion": {scope.Region},
		}
	}

	for name, value := range k.Tags {
		values["aws:resourcetag/"+strings.ToLower(name)] = []string{value}
	}

	evaluation := policy.Context{
		Values:            values,
		ResourceTags:      true,
		ResourceAccount:   scope.AccountID,
		AccountDelegation: true,
	}
	identity := policy.Decision{Allowed: principal.Kind == request.PrincipalRoot}
	limitsAllow := true
	if principal.Kind != request.PrincipalRoot && s.policies != nil {
		snapshot, err := s.policies(ctx, principal)
		if err != nil {
			return err
		}

		for name, value := range snapshot.Tags {
			values["aws:principaltag/"+strings.ToLower(name)] = []string{value}
		}

		identity = policy.Evaluate(snapshot.Documents, "kms:PutKeyPolicy", keyARN(scope, k.ID), evaluation)
		for _, documents := range [][]policy.Document{snapshot.Boundary, snapshot.Session} {
			if documents == nil {
				continue
			}

			limit := policy.Evaluate(documents, "kms:PutKeyPolicy", keyARN(scope, k.ID), evaluation)
			limitsAllow = limitsAllow && limit.Allowed
			identity.ExplicitDeny = identity.ExplicitDeny || limit.ExplicitDeny
		}

		identity.Allowed = identity.Allowed && limitsAllow
	}

	resource := policy.EvaluateResource(policy.Document{Name: "key-policy", JSON: document},
		"kms:PutKeyPolicy", keyARN(scope, k.ID), evaluation, principal)
	resource.Allowed = resource.Allowed && (resource.BypassesLimits || limitsAllow)
	if !policy.KeyPolicyAllowed(identity, resource, principal.Account != scope.AccountID) {
		return awserror.New("MalformedPolicyDocumentException",
			"The new key policy will not allow you to update the key policy in the future.")
	}

	return nil
}
