package apigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
)

// shorthandPrefix starts the abbreviated resource form execute-api:/<stage>/<METHOD>/<path>, which
// AWS expands to the API's full ARN when it saves the policy.
const shorthandPrefix = "execute-api:/"

// vpcKeys are context keys only a private API's VPC endpoint can supply. A request here never has a
// literal value for them, so a condition on one cannot be satisfied.
var vpcKeys = []string{"aws:sourcevpc", "aws:sourcevpce", "aws:vpcsourceip"}

// checkPolicy validates an API's resource policy and stores it with the shorthand resources
// expanded, as AWS does. An empty policy means the API has none.
func checkPolicy(scope request.Scope, a *api.RestApi) error {
	if common.Value(a.Policy) == "" {
		a.Policy = nil
		return nil
	}

	expanded, err := expandPolicy(*a.Policy, "arn:aws:execute-api:"+scope.Region+":"+scope.AccountID+":"+*a.Id+"/")
	if err == nil {
		err = iampolicy.ValidateResource(expanded)
	}

	if err != nil {
		return badRequest(
			"Invalid policy document. Please check the policy syntax and ensure that Principals are valid. (%s)", err)
	}

	a.Policy = &expanded
	return nil
}

// expandPolicy rewrites each Resource and NotResource of the form execute-api:/<rest> to
// <arn prefix><rest>, where prefix ends with the API ID and a slash.
func expandPolicy(document, prefix string) (string, error) {
	var doc map[string]any
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return "", err
	}

	statements, ok := doc["Statement"].([]any)
	if !ok {
		statements = []any{doc["Statement"]}
	}

	for _, raw := range statements {
		statement, isObject := raw.(map[string]any)
		if !isObject {
			continue
		}

		for _, element := range []string{"Resource", "NotResource"} {
			switch resources := statement[element].(type) {
			case string:
				statement[element] = expandResource(resources, prefix)
			case []any:
				for i, resource := range resources {
					if name, isString := resource.(string); isString {
						resources[i] = expandResource(name, prefix)
					}
				}
			}
		}
	}

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(doc); err != nil {
		return "", err
	}

	return strings.TrimSpace(out.String()), nil
}

func expandResource(resource, prefix string) string {
	if rest, ok := strings.CutPrefix(resource, shorthandPrefix); ok {
		return prefix + rest
	}

	return resource
}

// invokeTarget is the execute-api:Invoke resource a request names, with the API's resource policy
// when it has one.
func (c *call) invokeTarget() catalog.AuthorizationResource {
	target := execute.APIResource(c.scope, *c.api.API.Id, *c.stage.StageName, c.req.Method, c.path)
	if c.hasPolicy() {
		target.Policy, target.UnknownKeys = *c.api.API.Policy, vpcKeys
	}

	return target
}

func (c *call) hasPolicy() bool { return common.Value(c.api.API.Policy) != "" }

// authorizePolicy evaluates the API's resource policy for a caller that is not signing the request,
// or returns nil when the API has none.
func (c *call) authorizePolicy(ctx context.Context, identity execute.Identity) *execute.Response {
	if !c.hasPolicy() {
		return nil
	}

	target := c.invokeTarget()
	denied := c.req.AuthorizePolicy(ctx, &target, identity)
	if denied != nil {
		c.authorizerError = string(denied.Body)
	}

	return denied
}
