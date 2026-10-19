package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/iam/policy"
)

// DeliveryTarget describes the resource an in-process delivery goes to, with its resource policy. The resource has
// no owner when it does not exist.
type DeliveryTarget func(ctx context.Context, arn string) (catalog.AuthorizationResource, error)

// deliveryActions are the actions a service principal performs on each kind of delivery target.
var deliveryActions = map[string]string{
	"sqs":    "sqs:SendMessage",
	"sns":    "sns:Publish",
	"lambda": "lambda:InvokeFunction",
}

// UseDeliveryTargets names the services AuthorizeService can deliver to, by the service in their ARNs. Call it
// before serving requests.
func (s *Server) UseDeliveryTargets(targets map[string]DeliveryTarget) { s.deliveryTargets = targets }

// AuthorizeService checks an in-process delivery by an AWS service against its target's resource policy, as AWS
// does for a service principal: only that policy can allow it, with no identity policy beside it and no default
// for the owner's account. Soft mode records a denied check and lets the delivery through; enforce mode fails it.
func (s *Server) AuthorizeService(ctx context.Context, d catalog.ServiceDelivery) error {
	mode := s.cfg.Identity.Authorization
	if mode == "" || mode == config.AuthorizationOff {
		return nil
	}

	parts := strings.SplitN(d.Target, ":", 6)
	if len(parts) != 6 {
		return awserror.Validation("%s is not an ARN.", d.Target)
	}

	lookup, ok := s.deliveryTargets[parts[2]]
	if !ok {
		return awserror.Validation("Deliveries to %s are not supported.", parts[2])
	}

	target, err := lookup(ctx, d.Target)
	if err != nil {
		return err
	}

	// A missing target has no policy to read; the delivery reports its absence itself.
	if target.OwnerAccount == "" {
		return nil
	}

	action := deliveryActions[parts[2]]
	principal := request.Principal{Kind: request.PrincipalService, Name: d.Principal}
	values := map[string][]string{
		"aws:sourcearn":            {d.SourceARN},
		"aws:principalservicename": {d.Principal},
		"aws:currenttime":          {time.Now().UTC().Format(time.RFC3339Nano)},
		"aws:securetransport":      {"true"},
		"aws:requestedregion":      {parts[3]},
	}
	if d.SourceAccount != "" {
		values["aws:sourceaccount"] = []string{d.SourceAccount}
	}

	for key, value := range target.Tags {
		values["aws:resourcetag/"+strings.ToLower(key)] = []string{value}
	}

	decision := policy.EvaluateResource(
		policy.Document{Name: "resource-policy:" + target.ARN, JSON: target.Policy},
		action,
		target.ARN,
		policy.Context{Values: values, ResourceAccount: target.OwnerAccount, ResourceTags: target.TagsKnown},
		&principal,
	)
	crossAccount := d.SourceAccount != "" && d.SourceAccount != target.OwnerAccount
	check := AuthorizationCheck{
		Decision: decision, ResourcePolicy: &decision, Action: action, Resource: target.ARN, CrossAccount: crossAccount,
	}
	record := &AuthorizationRecord{
		Mode: mode, Principal: d.Principal, Allowed: decision.Allowed, Checks: []AuthorizationCheck{check},
		Message: "allowed by resource-based policy",
	}
	if !record.Allowed {
		record.Message = serviceDenial(&d, &check)
		if mode == config.AuthorizationSoft {
			record.Message = "would deny: " + record.Message
		}
	}

	s.logger.Info("authorization", "mode", mode, "principal", record.Principal, "allowed", record.Allowed,
		"decision", record.Message, "checks", record.Checks)
	s.recordDelivery(ctx, &d, parts, action, record)
	if record.Allowed || mode == config.AuthorizationSoft {
		return nil
	}

	return &awserror.Error{Code: accessDenied, Message: record.Message, StatusCode: http.StatusForbidden}
}

// recordDelivery adds the delivery's check to the request inspector, which has no HTTP request to hang it on.
func (s *Server) recordDelivery(
	ctx context.Context,
	d *catalog.ServiceDelivery,
	target []string,
	action string,
	authorization *AuthorizationRecord,
) {
	fields := map[string]string{
		"principal": d.Principal, "source_arn": d.SourceARN, "source_account": d.SourceAccount, "target": d.Target,
	}
	// One request can cause many deliveries, so each gets its own ID and names the request that caused it.
	if m, ok := request.FromContext(ctx); ok && m.RequestID != "" {
		fields["caused_by"] = m.RequestID
	}

	body, _ := json.Marshal(fields)
	rec := Record{
		Authorization: authorization,
		RequestID:     common.RandomID(32),
		Time:          time.Now().UTC(),
		Method:        "DELIVER",
		Path:          d.Target,
		Service:       target[2],
		Operation:     action[strings.Index(action, ":")+1:],
		Account:       target[4],
		Region:        target[3],
		Status:        http.StatusOK,
		RequestBody:   string(body),
	}
	if !authorization.Allowed && authorization.Mode == config.AuthorizationEnforce {
		rec.Status = http.StatusForbidden
		rec.ErrorCode, rec.ErrorMessage = accessDenied, authorization.Message
	}

	s.requests.Add(rec)
}

func serviceDenial(d *catalog.ServiceDelivery, check *AuthorizationCheck) string {
	message := "Service: " + d.Principal + " is not authorized to perform: " + check.Action + " on resource: " +
		check.Resource + " "
	if check.ExplicitDeny {
		message += "with an explicit deny in a resource-based policy"
	} else {
		message += "because no resource-based policy allows the " + check.Action + " action"
	}

	if len(check.Unsupported) > 0 {
		message += "; " + strings.Join(check.Unsupported, "; ")
	}

	return message
}
