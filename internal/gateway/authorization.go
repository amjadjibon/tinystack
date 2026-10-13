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
	Action   string `json:"action"`
	Resource string `json:"resource"`
	policy.Decision
	WildcardOnly bool `json:"wildcard_only,omitempty"`
}

func (s *Server) authorize(ctx context.Context, r *http.Request, c *call, op *catalog.Operation, input any) error {
	mode := s.cfg.Identity.Authorization
	if mode == "" || mode == config.AuthorizationOff {
		return nil
	}

	// Compatibility: configured and unresolved parse-mode credentials act as
	// account root. Anonymous operations retain their modeled auth contract.
	if c.credentials == nil || op.Anonymous {
		return nil
	}

	principal := c.credentials.Principal
	if principal.Kind == request.PrincipalRoot {
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
	if s.policies != nil {
		snapshot, err = s.policies(ctx, &principal)
	}

	if err != nil {
		snapshot = iam.Authorization{
			Principal:   principal,
			Unsupported: []string{"IAM policy lookup failed: " + err.Error()},
		}
	}

	principal = snapshot.Principal
	c.meta.Principal = &principal
	ctx = request.WithMetadata(ctx, c.meta)
	var resources []catalog.AuthorizationResource
	if c.service.Authorizer != nil {
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

	action := op.IAMAction
	if action == "" {
		action = c.service.IAMPrefix + ":" + op.Name
	}

	sourceIP := r.RemoteAddr
	if host, _, splitErr := net.SplitHostPort(sourceIP); splitErr == nil {
		sourceIP = host
	}

	values := map[string][]string{
		"aws:userid": {principal.UserID}, "aws:principalarn": {principal.PolicyARN},
		"aws:principalaccount": {principal.Account}, "aws:sourceip": {sourceIP},
		"aws:currenttime":     {time.Now().UTC().Format(time.RFC3339Nano)},
		"aws:securetransport": {strconv.FormatBool(r.TLS != nil)}, "aws:requestedregion": {c.meta.Region},
	}

	if principal.Kind == request.PrincipalUser {
		values["aws:username"] = []string{principal.Name}
	}

	for key, value := range snapshot.Tags {
		values["aws:principaltag/"+strings.ToLower(key)] = []string{value}
	}

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
		decision.Unsupported = append(decision.Unsupported, snapshot.Unsupported...)
		if resolutionFailed {
			decision.Allowed = false
		}

		check := AuthorizationCheck{
			Decision:     decision,
			Action:       targetAction,
			Resource:     target.ARN,
			WildcardOnly: wildcardOnly,
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
		record.Message = "allowed by identity-based policy"
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
	if check.ExplicitDeny {
		message += "with an explicit deny in an identity-based policy"
	} else {
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
