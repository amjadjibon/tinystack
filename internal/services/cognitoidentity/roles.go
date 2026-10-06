package cognitoidentity

import (
	"context"
	"maps"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidentity"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// matchTypes are how a role mapping rule compares a claim with its value.
var matchTypes = map[string]func(claim, value string) bool{
	"Equals":     func(c, v string) bool { return c == v },
	"NotEqual":   func(c, v string) bool { return c != v },
	"StartsWith": strings.HasPrefix,
	"Contains":   strings.Contains,
}

func checkRoles(roles map[string]string, mappings map[string]api.RoleMapping) error {
	for k, arn := range roles {
		if k != roleAuth && k != roleUnauth {
			return invalid("Roles may name only the %s and %s roles, not %s.", roleAuth, roleUnauth, k)
		}

		if !strings.HasPrefix(arn, "arn:aws:iam::") || !strings.Contains(arn, ":role/") {
			return invalid("Invalid role ARN %s.", arn)
		}
	}

	for provider, m := range mappings {
		resolution := common.Value(m.AmbiguousRoleResolution)
		switch {
		case common.Value(m.Type) != "Token" && common.Value(m.Type) != "Rules":
			return invalid("The role mapping for %s needs a Type of Token or Rules.", provider)
		case resolution != "AuthenticatedRole" && resolution != "Deny":
			return invalid("The role mapping for %s needs an AmbiguousRoleResolution of AuthenticatedRole or Deny.",
				provider)
		case *m.Type == "Rules" && (m.RulesConfiguration == nil || len(m.RulesConfiguration.Rules) == 0 ||
			len(m.RulesConfiguration.Rules) > 25):
			return invalid("The rules role mapping for %s needs 1 to 25 rules.", provider)
		}

		for _, r := range common.OrElse(m.RulesConfiguration, &api.RulesConfigurationType{}).Rules {
			if matchTypes[common.Value(r.MatchType)] == nil || r.Claim == nil || r.Value == nil || r.RoleARN == nil {
				return invalid("A rule needs a Claim, a Value, a RoleARN, and a MatchType of Equals, NotEqual, " +
					"StartsWith, or Contains.")
			}
		}
	}

	return nil
}

func (s *Service) SetIdentityPoolRoles(
	ctx context.Context,
	in *api.SetIdentityPoolRolesInput,
) (*api.SetIdentityPoolRolesOutput, error) {
	if err := checkRoles(in.Roles, in.RoleMappings); err != nil {
		return nil, err
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		p, err := loadPool(tx, scope, in.IdentityPoolId)
		if err != nil {
			return err
		}

		p.Roles, p.Mappings = common.CloneTags(in.Roles), in.RoleMappings
		return storage.Write(tx, poolKey(scope, *in.IdentityPoolId), p)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetIdentityPoolRolesOutput{}, nil
}

func (s *Service) GetIdentityPoolRoles(
	ctx context.Context,
	in *api.GetIdentityPoolRolesInput,
) (*api.GetIdentityPoolRolesResponse, error) {
	var p *pool
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		p, err = loadPool(tx, scope, in.IdentityPoolId)
		return err
	})
	if err != nil {
		return nil, err
	}

	roles := p.Roles
	if roles == nil {
		roles = map[string]string{}
	}

	return &api.GetIdentityPoolRolesResponse{
		IdentityPoolId: in.IdentityPoolId, Roles: roles, RoleMappings: p.Mappings,
	}, nil
}

// chooseRole picks the role a session assumes: the unauthenticated role, or
// the role a login's mapping chooses, or else the authenticated role.
func chooseRole(p *pool, logins []login, custom string) (string, error) {
	if len(logins) == 0 {
		return defaultRole(p, roleUnauth)
	}

	for _, l := range logins {
		for _, key := range slices.Sorted(maps.Keys(p.Mappings)) {
			if key != l.provider && key != l.provider+":"+claimString(l.claims, "aud") {
				continue
			}

			m := p.Mappings[key]
			if role := mappedRole(&m, l.claims, custom); role != "" {
				return role, nil
			}

			if common.Value(m.AmbiguousRoleResolution) == "Deny" {
				return "", notAuthorized("Ambiguous role mapping rules for: %s denied this request.", l.provider)
			}
		}
	}

	return defaultRole(p, roleAuth)
}

func defaultRole(p *pool, kind string) (string, error) {
	if role := p.Roles[kind]; role != "" {
		return role, nil
	}

	return "", errRoles
}

// mappedRole is the role a mapping chooses from a login's claims, if any: a
// token's cognito:roles (custom, or its cognito:preferred_role, or its only
// role), or the role of the first rule a claim matches.
func mappedRole(m *api.RoleMapping, claims map[string]any, custom string) string {
	if common.Value(m.Type) == "Token" {
		roles := claimStrings(claims, "cognito:roles")
		preferred := claimString(claims, "cognito:preferred_role")
		switch {
		case custom != "" && slices.Contains(roles, custom):
			return custom
		case preferred != "":
			return preferred
		case len(roles) == 1:
			return roles[0]
		}

		return ""
	}

	for _, r := range m.RulesConfiguration.Rules {
		match := matchTypes[*r.MatchType]
		if slices.ContainsFunc(claimStrings(claims, *r.Claim), func(v string) bool { return match(v, *r.Value) }) {
			return *r.RoleARN
		}
	}

	return ""
}

// claimStrings are a claim's values: a string, or a list of strings.
func claimStrings(claims map[string]any, name string) []string {
	switch v := claims[name].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}

		return out
	}

	return nil
}
