package cognitoidp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// Lambda triggers run outside transactions: an operation's transaction
// decides on a call, and the operation makes it after the transaction ends.
// SignUp, whose pre sign-up trigger decides what the transaction writes, runs
// its transaction again with the trigger's response.

const (
	triggerScopes = "scopes"
	preTokenV1    = "V1_0"
	preTokenV2    = "V2_0"
	preTokenV3    = "V3_0"
	// noClient is the client ID in the events of admin operations.
	noClient = "CLIENT_ID_NOT_APPLICABLE"
)

// errTrigger stops a transaction that must call a trigger before it can
// continue.
var errTrigger = errors.New("trigger pending")

// checkTriggers validates a pool's Lambda triggers and fills in both forms of
// its pre token generation trigger, as AWS reports them.
func checkTriggers(l *api.LambdaConfigType) error {
	if l == nil {
		return nil
	}

	arns := []*string{
		l.PreSignUp, l.PreAuthentication, l.PostConfirmation, l.PostAuthentication, l.PreTokenGeneration,
		l.DefineAuthChallenge, l.CreateAuthChallenge, l.VerifyAuthChallengeResponse,
	}

	if cfg := l.PreTokenGenerationConfig; cfg != nil {
		arns = append(arns, cfg.LambdaArn)
	}

	for _, arn := range arns {
		if arn != nil && !strings.HasPrefix(*arn, "arn:") {
			return invalidParameter("Lambda trigger %s is not an ARN.", *arn)
		}
	}

	cfg := l.PreTokenGenerationConfig
	switch {
	case cfg == nil && l.PreTokenGeneration != nil:
		l.PreTokenGenerationConfig = &api.PreTokenGenerationVersionConfigType{
			LambdaArn: l.PreTokenGeneration, LambdaVersion: new(preTokenV1),
		}
	case cfg == nil:
	case cfg.LambdaArn == nil || !oneOf(cfg.LambdaVersion, preTokenV1, preTokenV2, preTokenV3):
		return invalidParameter(
			"PreTokenGenerationConfig needs a LambdaArn and a LambdaVersion of V1_0, V2_0, or V3_0.",
		)
	case l.PreTokenGeneration != nil && *l.PreTokenGeneration != *cfg.LambdaArn:
		return invalidParameter("PreTokenGeneration and PreTokenGenerationConfig.LambdaArn must be the same function.")
	default:
		l.PreTokenGeneration = cfg.LambdaArn
	}

	return nil
}

// triggers returns the pool's Lambda triggers, which may all be unset.
func (p *pool) triggers() api.LambdaConfigType {
	if l := p.Settings.LambdaConfig; l != nil {
		return *l
	}

	return api.LambdaConfigType{}
}

// event is the shape every trigger's event shares.
type event struct {
	CallerContext map[string]string `json:"callerContext"`
	Request       map[string]any    `json:"request"`
	Response      map[string]any    `json:"response"`
	Version       string            `json:"version"`
	TriggerSource string            `json:"triggerSource"`
	Region        string            `json:"region"`
	UserPoolID    string            `json:"userPoolId"`
	UserName      string            `json:"userName"`
}

// call is a trigger invocation a transaction decided on.
type call struct {
	event    event
	scope    request.Scope
	function string
	// name is the trigger's name in error messages, such as PreSignUp.
	name string
}

// newCall prepares a call of function for u, through the client with ID
// clientID. The request carries the user's attributes and status, and
// metadata when it is not nil.
func newCall(
	scope request.Scope, p *pool, clientID, function, name, source string, u *user, metadata map[string]string,
) *call {
	attrs := cloneMap(u.Attributes)
	attrs[attrUserStatus] = u.Status
	req := map[string]any{"userAttributes": attrs}
	if metadata != nil {
		req["clientMetadata"] = metadata
	}

	return &call{scope: scope, function: function, name: name, event: event{
		Version: "1", TriggerSource: source, Region: scope.Region, UserPoolID: p.ID, UserName: u.Username,
		CallerContext: map[string]string{"awsSdkVersion": "aws-sdk-unknown-unknown", "clientId": clientID},
		Request:       req, Response: map[string]any{},
	}}
}

// postConfirmation prepares the post confirmation call for a user just
// confirmed, if the pool has the trigger.
func postConfirmation(
	scope request.Scope, p *pool, clientID, source string, u *user, metadata map[string]string,
) *call {
	function := common.Value(p.triggers().PostConfirmation)
	if function == "" {
		return nil
	}

	return newCall(scope, p, clientID, function, "PostConfirmation", source, u, metadata)
}

// invoke calls a trigger in its pool's account and decodes the response
// member of the event it returns into response, unless response is nil.
func (s *Service) invoke(ctx context.Context, c *call, response any) error {
	if c == nil {
		return nil
	}

	payload, err := json.Marshal(c.event)
	if err != nil {
		return err
	}

	m, _ := request.FromContext(ctx)
	m.Scope = c.scope
	out, err := s.functions.Invoke(
		request.WithMetadata(ctx, m),
		&lambdaapi.InvocationRequest{FunctionName: &c.function, Payload: payload},
	)
	if err != nil {
		return awserror.New("UnexpectedLambdaException",
			c.name+" invocation failed due to error "+awserror.From(err).Message+".")
	}

	if out.FunctionError != nil {
		var failure struct {
			ErrorMessage string `json:"errorMessage"`
		}

		_ = json.Unmarshal(out.Payload, &failure)
		return awserror.New("UserLambdaValidationException", c.name+" failed with error "+failure.ErrorMessage+".")
	}

	var returned struct {
		Response json.RawMessage `json:"response"`
	}

	if err = json.Unmarshal(out.Payload, &returned); err != nil {
		return awserror.New("InvalidLambdaResponseException", "Unrecognizable lambda output")
	}

	if response == nil || len(returned.Response) == 0 || string(returned.Response) == "null" {
		return nil
	}

	if err = json.Unmarshal(returned.Response, response); err != nil {
		return awserror.New("InvalidLambdaResponseException", "Unrecognizable lambda output")
	}

	return nil
}

// preSignUp is a pre sign-up trigger's response.
type preSignUp struct {
	AutoConfirmUser bool `json:"autoConfirmUser"`
	AutoVerifyEmail bool `json:"autoVerifyEmail"`
	AutoVerifyPhone bool `json:"autoVerifyPhone"`
}

// claimChanges are a pre token generation trigger's changes to one token.
type claimChanges struct {
	ClaimsToAddOrOverride map[string]any `json:"claimsToAddOrOverride"`
	ClaimsToSuppress      []string       `json:"claimsToSuppress"`
	ScopesToAdd           []string       `json:"scopesToAdd"`
	ScopesToSuppress      []string       `json:"scopesToSuppress"`
}

type groupChanges struct {
	PreferredRole      *string  `json:"preferredRole"`
	GroupsToOverride   []string `json:"groupsToOverride"`
	IamRolesToOverride []string `json:"iamRolesToOverride"`
}

// preToken is a pre token generation trigger's response: version 1 changes
// the ID token, and versions 2 and 3 either token.
type preToken struct {
	ClaimsOverrideDetails *struct {
		GroupOverrideDetails *groupChanges `json:"groupOverrideDetails"`
		claimChanges
	} `json:"claimsOverrideDetails"`
	ClaimsAndScopeOverrideDetails *struct {
		IDTokenGeneration     *claimChanges `json:"idTokenGeneration"`
		AccessTokenGeneration *claimChanges `json:"accessTokenGeneration"`
		GroupOverrideDetails  *groupChanges `json:"groupOverrideDetails"`
	} `json:"claimsAndScopeOverrideDetails"`
}

// reserved are the claims a pre token generation trigger cannot change,
// which Cognito ignores.
var reserved = []string{
	"acr",
	"amr",
	"aud",
	"at_hash",
	claimAuthTime,
	"azp",
	claimClientID,
	"cognito:groups",
	"cognito:preferred_role",
	"cognito:roles",
	"cognito:username",
	"event_id",
	claimExpires,
	claimIssuedAt,
	attrIdentities,
	claimIssuer,
	claimJTI,
	"nbf",
	"nonce",
	"origin_jti",
	claimScope,
	attrSub,
	claimTokenUse,
	claimUsername,
	"version",
}

// preTokenCall prepares the pre token generation call for a grant, if the
// pool has the trigger.
func preTokenCall(scope request.Scope, p *pool, c *client, u *user, g *grant, source string) *call {
	cfg := p.triggers().PreTokenGenerationConfig
	if cfg == nil {
		return nil
	}

	pc := newCall(scope, p, c.ID, *cfg.LambdaArn, "PreTokenGeneration", "TokenGeneration_"+source, u, nil)
	pc.event.Request["groupConfiguration"] = map[string]any{
		"groupsToOverride": append([]string{}, u.Groups...), "iamRolesToOverride": []string{}, "preferredRole": nil,
	}

	if common.Value(cfg.LambdaVersion) != preTokenV1 {
		pc.event.Version = "2"
		pc.event.Request[triggerScopes] = strings.Fields(g.access[claimScope].(string))
	}

	return pc
}

// apply makes a pre token generation trigger's changes to the grant's claims.
func (r *preToken) apply(g *grant) {
	var groups *groupChanges
	if d := r.ClaimsOverrideDetails; d != nil {
		changeClaims(g.id, &d.claimChanges)
		groups = d.GroupOverrideDetails
	}

	if d := r.ClaimsAndScopeOverrideDetails; d != nil {
		changeClaims(g.id, d.IDTokenGeneration)
		changeClaims(g.access, d.AccessTokenGeneration)
		if a := d.AccessTokenGeneration; a != nil {
			scopes := slices.DeleteFunc(strings.Fields(g.access[claimScope].(string)), func(s string) bool {
				return slices.Contains(a.ScopesToSuppress, s) || slices.Contains(a.ScopesToAdd, s)
			})
			g.access[claimScope] = strings.Join(append(scopes, a.ScopesToAdd...), " ")
		}

		groups = d.GroupOverrideDetails
	}

	if groups == nil {
		return
	}

	if groups.GroupsToOverride != nil {
		for _, claims := range []map[string]any{g.id, g.access} {
			delete(claims, "cognito:groups")
			if len(groups.GroupsToOverride) > 0 {
				claims["cognito:groups"] = groups.GroupsToOverride
			}
		}
	}

	if len(groups.IamRolesToOverride) > 0 {
		g.id["cognito:roles"] = groups.IamRolesToOverride
	}

	if groups.PreferredRole != nil {
		g.id["cognito:preferred_role"] = *groups.PreferredRole
	}
}

// changeClaims adds, overrides, and then suppresses claims, leaving the
// reserved ones alone.
func changeClaims(claims map[string]any, c *claimChanges) {
	if c == nil {
		return
	}

	for name, v := range c.ClaimsToAddOrOverride {
		if !slices.Contains(reserved, name) {
			claims[name] = v
		}
	}

	for _, name := range c.ClaimsToSuppress {
		if !slices.Contains(reserved, name) {
			delete(claims, name)
		}
	}
}
