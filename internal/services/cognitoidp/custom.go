package cognitoidp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	challengeCustom = "CUSTOM_CHALLENGE"
	challengeSRPA   = "SRP_A"
)

type challengeResult struct {
	Name     string `json:"challengeName"`
	Metadata string `json:"challengeMetadata,omitempty"`
	Result   bool   `json:"challengeResult"`
}

type authExecution struct {
	cached  map[string]json.RawMessage
	pending *call
	key     string
}

type authExecutionKey struct{}

// runAuth retries the transaction with a trigger response. All external calls occur after rollback;
// each retry rechecks the live session, user and client. Cache keys include the entire event and function.
func (s *Service) runAuth(ctx context.Context, fn func(context.Context) error) error {
	run := &authExecution{cached: map[string]json.RawMessage{}}
	ctx = context.WithValue(ctx, authExecutionKey{}, run)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := fn(ctx)
		if !errors.Is(err, errTrigger) {
			return err
		}

		var response json.RawMessage
		if err := s.invoke(ctx, run.pending, &response); err != nil {
			return err
		}

		run.cached[run.key] = response
	}
}

func customInvoke(ctx context.Context, c *call, response any) error {
	run := ctx.Value(authExecutionKey{}).(*authExecution)
	payload, err := json.Marshal(c.event)
	if err != nil {
		return err
	}

	key := fmt.Sprintf("%s:%x", c.function, sha256.Sum256(payload))
	raw, ok := run.cached[key]
	if !ok {
		run.pending, run.key = c, key
		return errTrigger
	}

	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, response) != nil {
		return awserror.New("InvalidLambdaResponseException", "Unrecognizable lambda output")
	}

	return nil
}

func customTriggers(p *pool) error {
	l := p.triggers()
	if common.Value(l.DefineAuthChallenge) == "" || common.Value(l.CreateAuthChallenge) == "" ||
		common.Value(l.VerifyAuthChallengeResponse) == "" {
		return invalidParameter(
			"CUSTOM_AUTH requires DefineAuthChallenge, CreateAuthChallenge and VerifyAuthChallengeResponse.",
		)
	}

	return nil
}

func customCall(
	scope request.Scope, p *pool, c *client, u *user, sess *authSession, name, function string,
	metadata map[string]string,
) *call {
	call := newCall(scope, p, c.ID, function, name, name+"_Authentication", u, metadata)
	call.event.Request["session"] = append([]challengeResult{}, sess.History...)
	call.event.Request["userNotFound"] = sess.UserNotFound
	return call
}

func (s *Service) customAuth(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, params map[string]string,
) (*outcome, error) {
	if err := requireFlow(c, flowCustom, "CUSTOM_AUTH"); err != nil {
		return nil, err
	}

	if err := customTriggers(p); err != nil {
		return nil, err
	}

	u, err := findUser(tx, scope, p, new(params[paramUsername]))
	missing := awserror.From(err).Code == errorUserNotFound &&
		common.Value(c.Settings.PreventUserExistenceErrors) == featureEnabled
	if missing {
		u, err = &user{Username: params[paramUsername], Status: statusConfirmed, Attributes: map[string]string{}}, nil
	}

	if err != nil {
		return nil, err
	}

	hash := params[paramSecretHash]
	if err := checkSecretHash(c, &hash, params[paramUsername], u.Username); err != nil {
		return nil, err
	}

	if u.Disabled {
		return nil, notAuthorized("User is disabled.")
	}

	if u.Status == statusUnconfirmed {
		return nil, awserror.New("UserNotConfirmedException", "User is not confirmed.")
	}

	if u.Status == statusReset {
		return nil, awserror.New("PasswordResetRequiredException", "Password reset required for the user")
	}

	sess := authSession{Custom: true, UserNotFound: missing}
	if params["CHALLENGE_NAME"] == challengeSRPA {
		if params[challengeSRPA] == "" {
			return nil, invalidParameter("Missing required parameter SRP_A")
		}

		sess.History = []challengeResult{{Name: challengeSRPA, Result: true}}
	} else if params["CHALLENGE_NAME"] != "" && params["CHALLENGE_NAME"] != challengeCustom {
		return nil, invalidParameter("CUSTOM_AUTH must start with SRP_A or CUSTOM_CHALLENGE.")
	}

	return s.defineCustom(ctx, tx, scope, p, c, u, &sess, params[challengeSRPA], nil)
}

func (s *Service) defineCustom(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, u *user, sess *authSession,
	srpA string, metadata map[string]string,
) (*outcome, error) {
	if err := customTriggers(p); err != nil {
		return nil, err
	}

	call := customCall(
		scope,
		p,
		c,
		u,
		sess,
		"DefineAuthChallenge",
		common.Value(p.triggers().DefineAuthChallenge),
		metadata,
	)
	var defined struct {
		Issue *bool  `json:"issueTokens"`
		Fail  *bool  `json:"failAuthentication"`
		Name  string `json:"challengeName"`
	}

	if err := customInvoke(ctx, call, &defined); err != nil {
		return nil, err
	}

	if defined.Issue == nil || defined.Fail == nil || (*defined.Issue && *defined.Fail) {
		return nil, awserror.New("InvalidLambdaResponseException", "Invalid DefineAuthChallenge response")
	}

	if *defined.Fail {
		return &outcome{failure: notAuthorized("Incorrect username or password.")}, nil
	}

	if *defined.Issue {
		if sess.UserNotFound {
			return &outcome{failure: notAuthorized("Incorrect username or password.")}, nil
		}

		if u.Status == statusForceChange {
			return &outcome{
				failure: notAuthorized("User must change their temporary password before authentication."),
			}, nil
		}

		g, err := s.signIn(ctx, tx, scope, p, c, u, sourceAuth)
		return &outcome{grant: g, ChallengeParameters: map[string]string{}}, err
	}

	switch defined.Name {
	case challengePasswordVerifier, challengeSRPA:
		if srpA == "" || len(sess.History) != 1 || sess.History[0].Name != challengeSRPA {
			return nil, awserror.New("InvalidLambdaResponseException", "SRP must begin the custom authentication flow")
		}

		out, err := s.beginSRP(tx, scope, p, c, u, srpA)
		if err != nil {
			return nil, err
		}

		stored, _, err := storage.Read[srpSession](tx, sessionKey(scope, p.ID, *out.Session))
		if err != nil {
			return nil, err
		}

		stored.Custom, stored.History = true, sess.History
		if err := writeAuthSession(tx, scope, p, *out.Session, stored); err != nil {
			return nil, err
		}

		return out, nil
	case challengeCustom:
		call = customCall(
			scope,
			p,
			c,
			u,
			sess,
			"CreateAuthChallenge",
			common.Value(p.triggers().CreateAuthChallenge),
			metadata,
		)
		call.event.Request["challengeName"] = challengeCustom
		var created struct {
			Public   map[string]string `json:"publicChallengeParameters"`
			Private  map[string]string `json:"privateChallengeParameters"`
			Metadata string            `json:"challengeMetadata"`
		}

		if err := customInvoke(ctx, call, &created); err != nil {
			return nil, err
		}

		if created.Private == nil || created.Public == nil {
			return nil, awserror.New("InvalidLambdaResponseException", "Invalid CreateAuthChallenge response")
		}

		sess.Challenge, sess.CustomPrivate, sess.Metadata = challengeCustom, created.Private, created.Metadata
		sess.Expires = s.now().UTC().Add(time.Duration(common.Value(c.Settings.AuthSessionValidity)) * time.Minute)
		return s.newChallenge(tx, scope, p, c, u, sess, created.Public)
	default:
		return nil, awserror.New("InvalidLambdaResponseException", "Unsupported custom authentication challenge")
	}
}

func (s *Service) verifyCustom(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, u *user, sess *authSession,
	answer string, metadata map[string]string,
) (*outcome, error) {
	if err := customTriggers(p); err != nil {
		return nil, err
	}

	call := newCall(scope, p, c.ID, common.Value(p.triggers().VerifyAuthChallengeResponse),
		"VerifyAuthChallengeResponse", "VerifyAuthChallengeResponse_Authentication", u, metadata)
	call.event.Request["privateChallengeParameters"] = sess.CustomPrivate
	call.event.Request["challengeAnswer"] = answer
	call.event.Request["userNotFound"] = sess.UserNotFound
	var verified struct {
		Correct *bool `json:"answerCorrect"`
	}

	if err := customInvoke(ctx, call, &verified); err != nil {
		return nil, err
	}

	if verified.Correct == nil {
		return nil, awserror.New("InvalidLambdaResponseException", "Invalid VerifyAuthChallengeResponse response")
	}

	sess.History = append(
		sess.History,
		challengeResult{Name: challengeCustom, Result: *verified.Correct, Metadata: sess.Metadata},
	)
	return s.defineCustom(ctx, tx, scope, p, c, u, sess, "", metadata)
}

// afterPassword preserves the custom history across password changes and native MFA challenges.
func (s *Service) afterPassword(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, u *user, sess *authSession,
	metadata map[string]string,
) (*outcome, error) {
	if !sess.Custom {
		return s.finishSignIn(ctx, tx, scope, p, c, u)
	}

	if u.Status == statusForceChange {
		out, err := challengeNew(tx, scope, p, c, u, s.now().UTC())
		if err != nil {
			return nil, err
		}

		pending, _, err := storage.Read[authSession](tx, sessionKey(scope, p.ID, *out.Session))
		if err != nil {
			return nil, err
		}

		pending.Custom, pending.History = true, sess.History
		if err := writeAuthSession(tx, scope, p, *out.Session, pending); err != nil {
			return nil, err
		}

		return out, nil
	}

	if !slices.ContainsFunc(sess.History, func(result challengeResult) bool {
		return result.Result &&
			(result.Name == challengeSMS || result.Name == challengeTOTP || result.Name == challengeSetup)
	}) {
		if out, err := s.requireMFA(tx, scope, p, c, u, sess); err != nil || out != nil {
			return out, err
		}
	}

	return s.defineCustom(ctx, tx, scope, p, c, u, sess, "", metadata)
}
