package gateway

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/iam/policy"
)

// listable keeps the resources of principal's account that IAM lets principal
// list, the way the AWS console shows only what a person's list calls return.
// Without enforcement it keeps the whole account. A failed policy lookup keeps
// nothing.
func (s *Server) listable(ctx context.Context, principal *request.Principal, all []catalog.Owned) []catalog.Owned {
	out := slices.DeleteFunc(all, func(o catalog.Owned) bool { return o.Account != principal.Account })
	if s.cfg.Identity.Authorization != config.AuthorizationEnforce {
		return out
	}

	if s.policies == nil {
		return nil
	}

	snapshot, err := s.policies(ctx, principal)
	if err != nil {
		s.logger.Warn("resources: IAM policy lookup failed", "principal", principal.ARN, "error", err)
		return nil
	}

	values := map[string][]string{
		"aws:userid": {principal.UserID}, "aws:principalarn": {principal.PolicyARN},
		"aws:principalaccount": {principal.Account},
	}
	for key, value := range snapshot.Tags {
		values["aws:principaltag/"+strings.ToLower(key)] = []string{value}
	}

	allowed := func(action, arn string) bool {
		return policy.Evaluate(snapshot.Documents, action, arn, policy.Context{Values: values}).Allowed
	}

	return slices.DeleteFunc(out, func(o catalog.Owned) bool {
		service := s.services[o.Service]
		if service == nil {
			return true
		}

		arn := o.ARN
		if arn == "" {
			arn = "*"
		}

		if action := listAction(service, o.Type); action != "" {
			return !allowed(action, arn)
		}

		for _, action := range readActions(service) {
			if allowed(action, arn) {
				return false
			}
		}

		return true
	})
}

// listAction is the IAM action of the operation that lists resources of kind:
// List<Kind>s, Describe<Kind>s or Get<Kind>s, in that order, or "" when the
// service has none.
func listAction(service *catalog.Service, kind string) string {
	word := strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == ' ' {
			return -1
		}

		return r
	}, strings.ToLower(kind))
	plurals := []string{word + "s", word + "es"}
	if strings.HasSuffix(word, "y") {
		plurals = append(plurals, strings.TrimSuffix(word, "y")+"ies")
	}

	for _, verb := range []string{"list", "describe", "get"} {
		for _, plural := range plurals {
			for name, op := range service.Operations {
				if strings.EqualFold(name, verb+plural) {
					return iamAction(service, &op)
				}
			}
		}
	}

	return ""
}

// readActions are the IAM actions of a service's List, Describe and Get
// operations.
func readActions(service *catalog.Service) []string {
	var out []string
	for name, op := range service.Operations {
		if strings.HasPrefix(name, "List") || strings.HasPrefix(name, "Describe") || strings.HasPrefix(name, "Get") {
			out = append(out, iamAction(service, &op))
		}
	}

	slices.Sort(out)
	return out
}

// iamAction is the IAM action an operation is authorized as.
func iamAction(service *catalog.Service, op *catalog.Operation) string {
	if op.IAMAction != "" {
		return op.IAMAction
	}

	return service.IAMPrefix + ":" + op.Name
}
