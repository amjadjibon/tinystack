package gateway

import (
	"cmp"
	"net/http"
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
func (s *Server) listable(r *http.Request, principal *request.Principal, all []catalog.Owned) []catalog.Owned {
	out := slices.DeleteFunc(all, func(o catalog.Owned) bool { return o.Account != principal.Account })
	if s.cfg.Identity.Authorization != config.AuthorizationEnforce {
		return out
	}

	if s.policies == nil {
		return nil
	}

	snapshot, err := s.policies(r.Context(), principal)
	if err != nil {
		s.logger.Warn("resources: IAM policy lookup failed", "principal", principal.ARN, "error", err)
		return nil
	}

	requestRegion := s.cfg.Defaults.Region
	if a, signed, parseErr := signature(r); parseErr == nil && signed {
		requestRegion = a.Region
	}

	out = slices.DeleteFunc(out, func(o catalog.Owned) bool {
		service := s.services[o.Service]
		if service == nil {
			return true
		}

		// Inventory is a list view, not permission to fetch values, policy
		// documents, or other details that require distinct read actions.
		if o.ARN == "" {
			return true
		}

		action := listAction(service, o.Type)
		if action == "" {
			return true
		}

		values := authorizationValues(r, &snapshot.Principal, snapshot.Tags, cmp.Or(o.Region, requestRegion))
		for key, value := range o.Tags {
			values["aws:resourcetag/"+strings.ToLower(key)] = []string{value}
		}

		for _, documents := range [][]policy.Document{snapshot.Documents, snapshot.Boundary, snapshot.Session} {
			if documents == nil {
				continue
			}

			if !policy.Evaluate(documents, action, o.ARN, policy.Context{
				Values: values, ResourceTags: true,
			}).Allowed {
				return true
			}
		}

		return len(snapshot.Documents) == 0
	})
	for i := range out {
		out[i].Details = nil
	}

	return out
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

// iamAction is the IAM action an operation is authorized as.
func iamAction(service *catalog.Service, op *catalog.Operation) string {
	if op.IAMAction != "" {
		return op.IAMAction
	}

	return service.IAMPrefix + ":" + op.Name
}
