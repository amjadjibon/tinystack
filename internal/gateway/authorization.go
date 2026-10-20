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
	"github.com/amjadjibon/tinystack/internal/aws/execute"
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
		service == authorizationKMS || service == authorizationLambda || service == authorizationSecrets ||
		service == consoleEventBridge || service == executeService
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
	Grant          *catalog.GrantAuthorization `json:"grant,omitempty"`
	Boundary       *policy.Decision            `json:"boundary,omitempty"`
	SessionPolicy  *policy.Decision            `json:"session_policy,omitempty"`
	ResourcePolicy *policy.Decision            `json:"resource_policy,omitempty"`
	Action         string                      `json:"action"`
	Resource       string                      `json:"resource"`
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
	switch {
	case c.identity != execute.Signed:
		principal = request.Principal{Kind: request.PrincipalAnonymous, ARN: "anonymous"}
	case c.credentials != nil:
		principal = c.credentials.Principal
	default:
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

	snapshot, lookupFailed := s.identitySnapshot(ctx, &principal)
	principal = snapshot.Principal
	c.meta.Principal = &principal
	c.meta.ResourceAuthorization = resourceAuthorization(c.meta.Service)
	c.meta.AuthorizationValues = authorizationValues(r, &principal, snapshot.Tags, c.meta.Region)
	ctx = request.WithMetadata(ctx, c.meta)
	record := s.evaluate(ctx, mode, &evaluation{
		service: c.service, op: op, input: input, snapshot: snapshot, values: c.meta.AuthorizationValues,
		resources: resources, serviceName: c.meta.Service, granted: c.identity == execute.Granted,
		lookupFailed: lookupFailed,
	})

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

// evaluation is one call for evaluate to decide. The ctx it runs with carries the metadata of the call's account
// and Region, in which resources resolve.
type evaluation struct {
	service *catalog.Service
	op      *catalog.Operation
	input   any
	values  map[string][]string
	// resources are the concrete targets of a console-only operation; nil asks the service to resolve them.
	resources   []catalog.AuthorizationResource
	serviceName string
	// snapshot is the caller's identity policies, boundary and session policy; its Principal is the caller.
	snapshot iam.Authorization
	// granted marks a caller another authorizer allowed, which only an explicit deny can stop.
	granted bool
	// lookupFailed marks a snapshot that could not be read, which denies everything.
	lookupFailed bool
}

// identitySnapshot reads the caller's current policies. A failed lookup yields a snapshot that carries the reason.
func (s *Server) identitySnapshot(ctx context.Context, principal *request.Principal) (
	snapshot iam.Authorization, lookupFailed bool,
) {
	snapshot = iam.Authorization{Principal: *principal}
	if s.policies == nil || principal.Kind == request.PrincipalRoot || principal.Kind == request.PrincipalAnonymous {
		return snapshot, false
	}

	snapshot, err := s.policies(ctx, principal)
	if err != nil {
		return iam.Authorization{
			Principal:   *principal,
			Unsupported: []string{"IAM policy lookup failed: " + err.Error()},
		}, true
	}

	return snapshot, false
}

// evaluate decides a call against the caller's identity policies, boundary, session policy, and the resource
// policies of its targets, as every signed request and delivery under a role is.
func (s *Server) evaluate(ctx context.Context, mode string, e *evaluation) *AuthorizationRecord {
	op := e.op
	principal := e.snapshot.Principal
	resources := e.resources
	var err error
	if resources == nil && e.service.Authorizer != nil {
		resources, err = e.service.Authorizer.AuthorizationResources(ctx, op.Name, e.input)
	}

	resolutionFailed := err != nil
	if resolutionFailed {
		e.snapshot.Unsupported = append(e.snapshot.Unsupported, "resource resolution failed: "+err.Error())
		resources = nil
	}

	wildcardOnly := len(resources) == 0
	if wildcardOnly {
		resources = []catalog.AuthorizationResource{{ARN: "*"}}
	}

	// Roles the request hands to a service are checked as iam:PassRole beside the operation's own action.
	passed, passErr := s.passedRoles(ctx, e.serviceName, op.Name, e.input)
	if passErr != nil {
		e.snapshot.Unsupported = append(e.snapshot.Unsupported, "iam:PassRole resolution failed: "+passErr.Error())
		resolutionFailed = true
	}

	resources = append(resources, passed...)

	action := iamAction(e.service, op)

	values := e.values

	record := &AuthorizationRecord{Mode: mode, Principal: principal.ARN, Allowed: true}
	for _, target := range resources {
		contextValues := make(map[string][]string, len(values)+len(target.Tags)+len(target.Values))
		for key, value := range values {
			contextValues[key] = value
		}

		for key, value := range target.Values {
			contextValues[key] = value
		}

		for key, value := range target.Tags {
			contextValues["aws:resourcetag/"+strings.ToLower(key)] = []string{value}
		}

		targetAction := target.Action
		if targetAction == "" {
			targetAction = action
		}

		decision := policy.Evaluate(e.snapshot.Documents, targetAction, target.ARN, policy.Context{
			Values: contextValues, ResourceTags: target.TagsKnown, Unknown: target.UnknownKeys,
		})
		var boundaryDecision, sessionDecision *policy.Decision
		limitsAllow, limitsDeny := true, false
		for _, layer := range []struct {
			output    **policy.Decision
			documents []policy.Document
		}{
			{&boundaryDecision, e.snapshot.Boundary}, {&sessionDecision, e.snapshot.Session},
		} {
			if layer.documents == nil {
				continue
			}

			evaluated := policy.Evaluate(layer.documents, targetAction, target.ARN, policy.Context{
				Values: contextValues, ResourceTags: target.TagsKnown, Unknown: target.UnknownKeys,
			})
			*layer.output = &evaluated
			limitsAllow = limitsAllow && evaluated.Allowed
			limitsDeny = limitsDeny || evaluated.ExplicitDeny
			decision.Unsupported = append(decision.Unsupported, evaluated.Unsupported...)
		}

		decision.Allowed = decision.Allowed && limitsAllow

		if e.serviceName == consoleSNS {
			contextValues["aws:sourceowner"] = []string{principal.Account}
		}

		identityAllowed := decision.Allowed
		var resourceDecision *policy.Decision
		anonymous := principal.Kind == request.PrincipalAnonymous
		crossAccount := target.OwnerAccount != "" && target.OwnerAccount != principal.Account && !anonymous
		if principal.Kind == request.PrincipalRoot || e.granted {
			decision.Allowed = true
		}

		if target.OwnerAccount != "" {
			evaluated := policy.EvaluateResource(
				policy.Document{Name: "resource-policy:" + target.ARN, JSON: target.Policy},
				targetAction,
				target.ARN,
				policy.Context{
					Values: contextValues, ResourceAccount: target.OwnerAccount, ResourceTags: target.TagsKnown,
					AccountDelegation: target.PolicyRequired, Unknown: target.UnknownKeys,
				},
				&principal,
			)
			resourceDecision = &evaluated
			ownerRoot := principal.Kind == request.PrincipalRoot && !crossAccount
			bucketRecovery := e.serviceName == consoleS3 &&
				(op.Name == "GetBucketPolicy" || op.Name == "PutBucketPolicy" || op.Name == "DeleteBucketPolicy")
			queueRecovery := e.serviceName == consoleSQS &&
				(op.Name == "GetQueueAttributes" || op.Name == "SetQueueAttributes")
			topicRecovery := e.serviceName == consoleSNS &&
				(op.Name == "GetTopicAttributes" || op.Name == "SetTopicAttributes")
			secretRecovery := e.serviceName == authorizationSecrets &&
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

		if target.Grant != nil {
			grant := target.Grant
			grantAllows := grant.ID != "" && (!grant.RequiresIAM || identityAllowed)
			if grant.Retirement {
				// Key policy Allow alone cannot retire another principal's grant.
				decision.Allowed = grantAllows
			} else {
				decision.Allowed = decision.Allowed || grantAllows
			}
		}

		decision.ExplicitDeny = decision.ExplicitDeny || limitsDeny
		decision.Allowed = decision.Allowed && !decision.ExplicitDeny
		decision.Unsupported = append(decision.Unsupported, e.snapshot.Unsupported...)
		if resolutionFailed || e.lookupFailed || (target.Grant != nil && len(decision.Unsupported) > 0) {
			decision.Allowed = false
		}

		check := AuthorizationCheck{
			Decision:       decision,
			Grant:          target.Grant,
			Boundary:       boundaryDecision,
			SessionPolicy:  sessionDecision,
			Action:         targetAction,
			Resource:       target.ARN,
			WildcardOnly:   wildcardOnly && target.Action == "",
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
		if e.serviceName == authorizationKMS {
			record.Message = "allowed by identity-based policy, key policy or KMS grant"
		}
	}

	if !record.Allowed && mode == config.AuthorizationSoft {
		record.Message = "would deny: " + record.Message
	}

	return record
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

	if check.Grant != nil && check.Grant.Reason != "" {
		message += "; " + check.Grant.Reason
	}

	if len(check.Unsupported) > 0 {
		message += "; " + strings.Join(check.Unsupported, "; ")
	}

	return message
}

// authorizationValues keeps IAM request conditions identical on API calls and
// filtered inventory. Forwarded headers are not trusted for source IP or TLS.
// A call a service makes in-process has no request to take them from: it has no source IP and is secure.
func authorizationValues(
	r *http.Request,
	principal *request.Principal,
	tags map[string]string,
	region string,
) map[string][]string {
	values := map[string][]string{
		"aws:currenttime":     {time.Now().UTC().Format(time.RFC3339Nano)},
		"aws:securetransport": {"true"}, "aws:requestedregion": {region},
	}

	if r != nil {
		sourceIP := r.RemoteAddr
		if host, _, splitErr := net.SplitHostPort(sourceIP); splitErr == nil {
			sourceIP = host
		}

		values["aws:sourceip"] = []string{sourceIP}
		values["aws:securetransport"] = []string{strconv.FormatBool(r.TLS != nil)}
	}

	// An unauthenticated caller has no principal keys, so conditions on them see them absent.
	if principal.Kind != request.PrincipalAnonymous {
		values["aws:userid"] = []string{principal.UserID}
		values["aws:principalarn"] = []string{principal.PolicyARN}
		values["aws:principalaccount"] = []string{principal.Account}
	}

	if principal.Kind == request.PrincipalUser {
		values["aws:username"] = []string{principal.Name}
	}

	for key, value := range tags {
		values["aws:principaltag/"+strings.ToLower(key)] = []string{value}
	}

	return values
}
