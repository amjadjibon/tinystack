package sso

import (
	"maps"
	"regexp"
	"slices"
	"strings"
)

var invalidSessionName = regexp.MustCompile(`[^\w+=,.@-]`)

// groups reads the claim that lists a person's groups: a string, a list of
// strings, or an object whose keys are the groups. A claim name that contains dots is tried whole first, as namespaced
// claims such as https://example.com/roles need; otherwise the dots descend
// through nested objects, as in realm_access.roles.
func groups(claims map[string]any, path string) []string {
	value, ok := claims[path]
	if !ok {
		var current any = claims
		for part := range strings.SplitSeq(path, ".") {
			object, isObject := current.(map[string]any)
			if !isObject {
				return nil
			}

			if current, ok = object[part]; !ok {
				return nil
			}
		}

		value = current
	}

	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			if s, isString := item.(string); isString {
				out = append(out, s)
			}
		}

		return out
	case map[string]any:
		// Zitadel lists a project's roles as an object keyed by role name.
		return slices.Sorted(maps.Keys(v))
	}

	return nil
}

// sessionName names the STS session after the person: their email, else their
// username, else their subject, within the characters and length AWS allows.
func sessionName(claims map[string]any) string {
	for _, name := range []string{"email", "preferred_username", "sub"} {
		value, _ := claims[name].(string)
		value = invalidSessionName.ReplaceAllString(value, "-")
		if len(value) > 64 {
			value = value[:64]
		}

		if len(value) >= 2 {
			return value
		}
	}

	return "sso-user"
}
