package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/iam/policy"
)

const (
	authorizationKMS     = "kms"
	authorizationLambda  = "lambda"
	authorizationSecrets = "secretsmanager"
)

func resourceAuthorization(service string) bool {
	return service == consoleS3 || service == consoleSQS || service == consoleSNS ||
		service == authorizationKMS || service == authorizationLambda || service == authorizationSecrets
}

// PolicyLookup reads the current identity policy snapshot through IAM.
type PolicyLookup func(context.Context, *request.Principal) (iam.Authorization, error)

// UseAuthorization connects the IAM policy reader before serving requests.
func (s *Server) UseAuthorization(lookup PolicyLookup) { s.policies = lookup }

// AuthorizationRecord describes the decision independently of the request's
// eventual service error or result; soft denials retain a successful response.
type AuthorizationRecord struct {
	Mode      string               `json:"mode"`
	Principal string               `json:"principal"`
	Message   string               `json:"message"`
	Checks    []AuthorizationCheck `json:"checks,omitempty"`
	Allowed   bool                 `json:"allowed"`
}

type AuthorizationCheck struct {
	Boundary       *policy.Decision `json:"boundary,omitempty"`
	SessionPolicy  *policy.Decision `json:"session_policy,omitempty"`
	ResourcePolicy *policy.Decision `json:"resource_policy,omitempty"`
	Action         string           `json:"action"`
	Resource       string           `json:"resource"`
	policy.Decision
	WildcardOnly bool `json:"wildcard_only,omitempty"`
	CrossAccount bool `json:"cross_account,omitempty"`
}

func (s *Server) authorize(ctx context.Context, r *http.Request, c *call, op *catalog.Operation, input any) error {
	return s.authorizeTargets(ctx, r, c, op, input, nil)
}

// authorizeTargets also accepts concrete targets for console-only operations
// whose inputs are not part of the generated AWS service descriptor.
func (s *Server) authorizeTargets(ctx context.Context, r *http.Request, c *call, op *catalog.Operation,
	input any, resources []catalog.AuthorizationResource,
) error {
	if c.meta.Service == authorizationKMS {
		arn := "arn:aws:iam::" + c.meta.AccountID + ":root"
		principal := request.Principal{
			Kind: request.PrincipalRoot, Account: c.meta.AccountID, UserID: c.meta.AccountID,
			ARN: arn, PolicyARN: arn,
		}
		if c.credentials != nil {
			principal = c.credentials.Principal
		}

		c.meta.Principal = &principal
		c.meta.AuthorizationValues = authorizationValues(r, &principal, nil, c.meta.Region)
	}

	mode := s.cfg.Identity.Authorization
	if mode == "" || mode == config.AuthorizationOff {
		return nil
	}

	// Configured credentials act as root; unresolved parse-mode callers retain
	// their existing bypass. Anonymous operations keep their modeled contract.
	if op.Anonymous {
		return nil
	}

	var principal request.Principal
	if c.credentials != nil {
		principal = c.credentials.Principal
	} else {
		_, mapped := s.cfg.Identity.Accounts[c.meta.AccessKey]
		_, secret := s.cfg.Identity.SecretKeys[c.meta.AccessKey]
		if (!mapped && !secret) || !resourceAuthorization(c.meta.Service) {
			return nil
		}

		arn := "arn:aws:iam::" + c.meta.AccountID + ":root"
		principal = request.Principal{
			Kind:      request.PrincipalRoot,
			Account:   c.meta.AccountID,
			UserID:    c.meta.AccountID,
			ARN:       arn,
			PolicyARN: arn,
		}
	}

	if principal.Kind == request.PrincipalRoot && !resourceAuthorization(c.meta.Service) {
		return nil
	}

	// These are authentication operations, not permission-controlled APIs:
	// https://docs.aws.amazon.com/STS/latest/APIReference/API_GetCallerIdentity.html
	// https://docs.aws.amazon.com/STS/latest/APIReference/API_GetSessionToken.html
	if c.meta.Service == "sts" && (op.Name == opGetCallerIdentity || op.Name == "GetSessionToken") {
		return nil
	}

	snapshot := iam.Authorization{Principal: principal}
	var err error
	if s.policies != nil && principal.Kind != request.PrincipalRoot {
		snapshot, err = s.policies(ctx, &principal)
	}

	lookupFailed := err != nil
	if err != nil {
		snapshot = iam.Authorization{
			Principal:   principal,
			Unsupported: []string{"IAM policy lookup failed: " + err.Error()},
		}
	}

	principal = snapshot.Principal
	c.meta.Principal = &principal
	c.meta.ResourceAuthorization = resourceAuthorization(c.meta.Service)
	c.meta.AuthorizationValues = authorizationValues(r, &principal, snapshot.Tags, c.meta.Region)
	ctx = request.WithMetadata(ctx, c.meta)
	if resources == nil && c.service.Authorizer != nil {
		resources, err = c.service.Authorizer.AuthorizationResources(ctx, op.Name, input)
	}

	resolutionFailed := err != nil
	if resolutionFailed {
		snapshot.Unsupported = append(snapshot.Unsupported, "resource resolution failed: "+err.Error())
		resources = nil
	}

	wildcardOnly := len(resources) == 0
	if wildcardOnly {
		resources = []catalog.AuthorizationResource{{ARN: "*"}}
	}

	action := iamAction(c.service, op)

	values := c.meta.AuthorizationValues

	record := &AuthorizationRecord{Mode: mode, Principal: principal.ARN, Allowed: true}
	for _, target := range resources {
		contextValues := make(map[string][]string, len(values)+len(target.Tags))
		for key, value := range values {
			contextValues[key] = value
		}

		for key, value := range target.Tags {
			contextValues["aws:resourcetag/"+strings.ToLower(key)] = []string{value}
		}

		targetAction := target.Action
		if targetAction == "" {
			targetAction = action
		}

		decision := policy.Evaluate(snapshot.Documents, targetAction, target.ARN, policy.Context{
			Values: contextValues, ResourceTags: target.TagsKnown,
		})
		var boundaryDecision, sessionDecision *policy.Decision
		limitsAllow, limitsDeny := true, false
		for _, layer := range []struct {
			output    **policy.Decision
			documents []policy.Document
		}{
			{&boundaryDecision, snapshot.Boundary}, {&sessionDecision, snapshot.Session},
		} {
			if layer.documents == nil {
				continue
			}

			evaluated := policy.Evaluate(layer.documents, targetAction, target.ARN, policy.Context{
				Values: contextValues, ResourceTags: target.TagsKnown,
			})
			*layer.output = &evaluated
			limitsAllow = limitsAllow && evaluated.Allowed
			limitsDeny = limitsDeny || evaluated.ExplicitDeny
			decision.Unsupported = append(decision.Unsupported, evaluated.Unsupported...)
		}

		decision.Allowed = decision.Allowed && limitsAllow

		if c.meta.Service == consoleSNS {
			contextValues["aws:sourceowner"] = []string{principal.Account}
		}

		var resourceDecision *policy.Decision
		crossAccount := target.OwnerAccount != "" && target.OwnerAccount != principal.Account
		if principal.Kind == request.PrincipalRoot {
			decision.Allowed = true
		}

		if target.OwnerAccount != "" {
			evaluated := policy.EvaluateResource(
				policy.Document{Name: "resource-policy:" + target.ARN, JSON: target.Policy},
				targetAction,
				target.ARN,
				policy.Context{
					Values: contextValues, ResourceAccount: target.OwnerAccount, ResourceTags: target.TagsKnown,
					AccountDelegation: target.PolicyRequired,
				},
				&principal,
			)
			resourceDecision = &evaluated
			ownerRoot := principal.Kind == request.PrincipalRoot && !crossAccount
			bucketRecovery := c.meta.Service == consoleS3 &&
				(op.Name == "GetBucketPolicy" || op.Name == "PutBucketPolicy" || op.Name == "DeleteBucketPolicy")
			queueRecovery := c.meta.Service == consoleSQS &&
				(op.Name == "GetQueueAttributes" || op.Name == "SetQueueAttributes")
			topicRecovery := c.meta.Service == consoleSNS &&
				(op.Name == "GetTopicAttributes" || op.Name == "SetTopicAttributes")
			secretRecovery := c.meta.Service == authorizationSecrets &&
				(op.Name == "GetResourcePolicy" || op.Name == "PutResourcePolicy" || op.Name == "DeleteResourcePolicy")
			recovery := ownerRoot && (bucketRecovery || queueRecovery || topicRecovery || secretRecovery)
			if !recovery {
				switch {
				case target.PolicyRequired:
					// A key policy naming the role is still capped by its boundary and session policy.
					keyPolicy := evaluated
					keyPolicy.Allowed = evaluated.Allowed && (evaluated.BypassesLimits || limitsAllow)
					decision.Allowed = policy.KeyPolicyAllowed(decision, keyPolicy, crossAccount)
				case crossAccount:
					decision.Allowed = decision.Allowed && evaluated.Allowed
				default:
					decision.Allowed = decision.Allowed ||
						(evaluated.Allowed && (evaluated.BypassesLimits || limitsAllow))
				}

				decision.ExplicitDeny = decision.ExplicitDeny || evaluated.ExplicitDeny
				decision.Allowed = decision.Allowed && !decision.ExplicitDeny
			}

			decision.Unsupported = append(decision.Unsupported, evaluated.Unsupported...)
		}

		decision.ExplicitDeny = decision.ExplicitDeny || limitsDeny
		decision.Allowed = decision.Allowed && !decision.ExplicitDeny
		decision.Unsupported = append(decision.Unsupported, snapshot.Unsupported...)
		if resolutionFailed || lookupFailed {
			decision.Allowed = false
		}

		check := AuthorizationCheck{
			Decision:       decision,
			Boundary:       boundaryDecision,
			SessionPolicy:  sessionDecision,
			Action:         targetAction,
			Resource:       target.ARN,
			WildcardOnly:   wildcardOnly,
			ResourcePolicy: resourceDecision,
			CrossAccount:   crossAccount,
		}

		record.Checks = append(record.Checks, check)
		if decision.Allowed {
			continue
		}

		record.Allowed = false
		if record.Message == "" {
			record.Message = denial(principal.ARN, &check)
		}
	}

	if record.Allowed {
		record.Message = "allowed by identity-based or resource-based policy"
	}

	if !record.Allowed && mode == config.AuthorizationSoft {
		record.Message = "would deny: " + record.Message
	}

	c.authorization = record
	s.logger.Info("authorization", "mode", mode, "principal", record.Principal, "allowed", record.Allowed,
		"decision", record.Message, "checks", record.Checks)
	if record.Allowed || mode == config.AuthorizationSoft {
		return nil
	}

	code := "AccessDeniedException"
	if c.meta.Protocol == catalog.RESTXML || formProtocol(c.meta.Protocol) {
		code = accessDenied
	}

	return &awserror.Error{Code: code, Message: record.Message, StatusCode: http.StatusForbidden}
}

func denial(arn string, check *AuthorizationCheck) string {
	message := fmt.Sprintf(
		"User: %s is not authorized to perform: %s on resource: %s ",
		arn,
		check.Action,
		check.Resource,
	)
	switch {
	case check.ResourcePolicy != nil && check.ResourcePolicy.ExplicitDeny:
		message += "with an explicit deny in a resource-based policy"
	case check.Boundary != nil && check.Boundary.ExplicitDeny:
		message += "with an explicit deny in a permissions boundary"
	case check.SessionPolicy != nil && check.SessionPolicy.ExplicitDeny:
		message += "with an explicit deny in a session policy"
	case check.ExplicitDeny:
		message += "with an explicit deny in an identity-based policy"
	case check.CrossAccount && check.ResourcePolicy != nil && !check.ResourcePolicy.Allowed:
		message += "because no resource-based policy allows the " + check.Action + " action for cross-account access"
	case check.Boundary != nil && !check.Boundary.Allowed:
		message += "because no permissions boundary allows the " + check.Action + " action"
	case check.SessionPolicy != nil && !check.SessionPolicy.Allowed:
		message += "because no session policy allows the " + check.Action + " action"
	default:
		message += "because no identity-based policy allows the " + check.Action + " action"
	}

	if check.WildcardOnly {
		message += "; Resource '*' only: resource-level evaluation is unavailable for this operation"
	}

	if len(check.Unsupported) > 0 {
		message += "; " + strings.Join(check.Unsupported, "; ")
	}

	return message
}

// authorizationValues keeps IAM request conditions identical on API calls and
// filtered inventory. Forwarded headers are not trusted for source IP or TLS.
func authorizationValues(
	r *http.Request,
	principal *request.Principal,
	tags map[string]string,
	region string,
) map[string][]string {
	sourceIP := r.RemoteAddr
	if host, _, splitErr := net.SplitHostPort(sourceIP); splitErr == nil {
		sourceIP = host
	}

	values := map[string][]string{
		"aws:userid": {principal.UserID}, "aws:principalarn": {principal.PolicyARN},
		"aws:principalaccount": {principal.Account}, "aws:sourceip": {sourceIP},
		"aws:currenttime":     {time.Now().UTC().Format(time.RFC3339Nano)},
		"aws:securetransport": {strconv.FormatBool(r.TLS != nil)}, "aws:requestedregion": {region},
	}

	if principal.Kind == request.PrincipalUser {
		values["aws:username"] = []string{principal.Name}
	}

	for key, value := range tags {
		values["aws:principaltag/"+strings.ToLower(key)] = []string{value}
	}

	return values
}
