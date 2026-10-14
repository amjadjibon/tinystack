package cognitoidp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- Cognito TOTP uses SHA-1, as specified in RFC 6238.
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	challengeTOTP   = "SOFTWARE_TOKEN_MFA"
	challengeSMS    = "SMS_MFA"
	challengeSelect = "SELECT_MFA_TYPE"
	challengeSetup  = "MFA_SETUP"
	setupAssociate  = "associate"
	setupVerify     = "verify"
	setupComplete   = "complete"
)

// sessionIndex locates opaque sessions for the token setup APIs, which have no client ID.
type sessionIndex struct {
	Scope request.Scope
	Pool  string
}

func sessionIndexKey(id string) string { return "aws/" + service + "/session/" + id }

func writeAuthSession(tx storage.WriteTx, scope request.Scope, p *pool, id string, value any) error {
	if err := storage.Write(tx, sessionKey(scope, p.ID, id), value); err != nil {
		return err
	}

	return storage.Write(tx, sessionIndexKey(id), sessionIndex{Scope: scope, Pool: p.ID})
}

func deleteAuthSession(tx storage.WriteTx, scope request.Scope, p *pool, id string) {
	tx.Delete(sessionKey(scope, p.ID, id))
	tx.Delete(sessionIndexKey(id))
}

func expiredSession() error {
	return notAuthorized("Invalid session for the user, session is expired.")
}

func (s *Service) newChallenge(
	tx storage.WriteTx, scope request.Scope, p *pool, c *client, u *user, sess *authSession,
	parameters map[string]string,
) (*outcome, error) {
	id := common.RandomID(512, alphaNum)
	sess.Username, sess.Client, sess.Sub = u.Username, c.ID, u.Attributes[attrSub]
	if sess.Expires.IsZero() {
		sess.Expires = s.now().UTC().Add(time.Duration(common.Value(c.Settings.AuthSessionValidity)) * time.Minute)
	}

	if err := writeAuthSession(tx, scope, p, id, sess); err != nil {
		return nil, err
	}

	if parameters == nil {
		parameters = map[string]string{}
	}

	parameters[paramUsername] = u.Username
	return &outcome{ChallengeName: new(sess.Challenge), Session: &id, ChallengeParameters: parameters}, nil
}

func (u *user) mfaSettings() []string {
	out := []string{}
	if u.SMSMFA {
		out = append(out, challengeSMS)
	}

	if u.TOTPMFA {
		out = append(out, challengeTOTP)
	}

	return out
}

func (u *user) legacyMFA() []api.MFAOptionType {
	if !u.SMSMFA {
		return nil
	}

	return []api.MFAOptionType{{DeliveryMedium: new(mediumSMS), AttributeName: new(attrPhone)}}
}

//nolint:nilnil // nil outcome means that this sign-in does not require an MFA challenge.
func (s *Service) requireMFA(
	tx storage.WriteTx, scope request.Scope, p *pool, c *client, u *user, sess *authSession,
) (*outcome, error) {
	if common.Value(p.Settings.MfaConfiguration) == mfaOff {
		return nil, nil
	}

	factors := []string{}
	if u.TOTPMFA && u.TOTPSecret != "" {
		factors = append(factors, challengeTOTP)
	}

	if (u.SMSMFA || common.Value(p.Settings.MfaConfiguration) == "ON") &&
		p.Settings.SmsConfiguration != nil && u.Attributes[attrPhone] != "" {
		factors = append(factors, challengeSMS)
	}

	switch {
	case len(factors) == 0 && common.Value(p.Settings.MfaConfiguration) == mfaOptional && !u.SMSMFA && !u.TOTPMFA:
		return nil, nil
	case len(factors) == 0:
		available := []string{}
		if p.SoftwareTokenMFA {
			available = append(available, challengeTOTP)
		}

		if p.Settings.SmsConfiguration != nil {
			available = append(available, challengeSMS)
		}

		encoded, _ := json.Marshal(available)
		sess.Challenge, sess.Setup = challengeSetup, setupAssociate
		return s.newChallenge(tx, scope, p, c, u, sess, map[string]string{"MFAS_CAN_SETUP": string(encoded)})
	case len(factors) > 1 && !slices.Contains(factors, u.PreferredMFA):
		encoded, _ := json.Marshal(factors)
		sess.Challenge, sess.Factors = challengeSelect, factors
		return s.newChallenge(tx, scope, p, c, u, sess, map[string]string{"MFAS_CAN_CHOOSE": string(encoded)})
	default:
		factor := factors[0]
		if slices.Contains(factors, u.PreferredMFA) {
			factor = u.PreferredMFA
		}

		return s.mfaChallenge(tx, scope, p, c, u, sess, factor)
	}
}

func (s *Service) mfaChallenge(
	tx storage.WriteTx, scope request.Scope, p *pool, c *client, u *user, sess *authSession, factor string,
) (*outcome, error) {
	sess.Challenge = factor
	var messages outbox
	parameters := map[string]string{}
	if factor == challengeSMS {
		sess.SMSCode, sess.SMSDestination = newCode(), u.Attributes[attrPhone]
		template := "Your authentication code is {####}."
		template = first(p.Settings.SmsAuthenticationMessage, &template)
		d := messages.send(scope, p, u, mediumSMS, "", template, sess.SMSCode)
		parameters["CODE_DELIVERY_DELIVERY_MEDIUM"], parameters["CODE_DELIVERY_DESTINATION"] = d.Medium, d.Destination
	}

	out, err := s.newChallenge(tx, scope, p, c, u, sess, parameters)
	if err != nil {
		return nil, err
	}

	out.messages = messages
	return out, nil
}

// validTOTP implements RFC 6238 with Cognito's SHA-1/six-digit/30-second profile and ±1 step.
func validTOTP(secret, code string, now time.Time) bool {
	if len(code) != 6 {
		return false
	}

	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return false
	}

	for offset := int64(-1); offset <= 1; offset++ {
		counter := make([]byte, 8)
		binary.BigEndian.PutUint64(counter, uint64(now.Unix()/30+offset)) // #nosec G115 -- positive Unix step.
		mac := hmac.New(sha1.New, key)
		mac.Write(counter)
		sum := mac.Sum(nil)
		i := sum[len(sum)-1] & 15
		value := binary.BigEndian.Uint32(sum[i:i+4]) & 0x7fffffff
		if hmac.Equal([]byte(fmt.Sprintf("%06d", value%1_000_000)), []byte(code)) {
			return true
		}
	}

	return false
}

// maxCodeFailures is how many wrong MFA codes one sign-in session accepts
// before it ends, so a six-digit code cannot be guessed within the session's
// lifetime. AWS does not document its limit; this is TinyStack's.
const maxCodeFailures = 5

// wrongCode records a wrong MFA code and answers CodeMismatchException after
// the transaction commits, ending the session at maxCodeFailures.
func wrongCode(tx storage.WriteTx, scope request.Scope, p *pool, id string, sess *authSession) (*outcome, error) {
	sess.Failures++
	if sess.Failures >= maxCodeFailures {
		deleteAuthSession(tx, scope, p, id)
	} else if err := writeAuthSession(tx, scope, p, id, sess); err != nil {
		return nil, err
	}

	return &outcome{failure: codeMismatch()}, nil
}

func codeMismatch() error {
	return awserror.New("CodeMismatchException", "Invalid code or auth state for the user.")
}

// tokenSetup resolves exactly one authorization method and checks the setup session's phase.
func (s *Service) tokenSetup(
	ctx context.Context, token, id *string, stage string,
	fn func(storage.WriteTx, *accessToken, *authSession) error,
) error {
	if (common.Value(token) == "") == (common.Value(id) == "") {
		return invalidParameter("Provide exactly one of AccessToken or Session.")
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		var a *accessToken
		var sess *authSession
		var err error
		if token != nil {
			a, err = s.checkAccess(tx, token)
			if err != nil {
				return err
			}
		} else {
			index, ok, readErr := storage.Read[sessionIndex](tx, sessionIndexKey(*id))
			if readErr != nil {
				return readErr
			}

			if !ok {
				return expiredSession()
			}

			p, loadErr := loadPool(tx, index.Scope, &index.Pool)
			if loadErr != nil {
				return expiredSession()
			}

			sess, ok, err = storage.Read[authSession](tx, sessionKey(index.Scope, p.ID, *id))
			if err != nil {
				return err
			}

			if !ok || sess.Challenge != challengeSetup || sess.Setup != stage || !s.now().Before(sess.Expires) {
				return expiredSession()
			}

			c, loadErr := loadClient(tx, index.Scope, p, &sess.Client)
			if loadErr != nil {
				return expiredSession()
			}

			u, found, userErr := storage.Read[user](tx, userKey(index.Scope, p, sess.Username))
			if userErr != nil {
				return userErr
			}

			if !found || u.Attributes[attrSub] != sess.Sub || u.Disabled {
				return expiredSession()
			}

			a = &accessToken{scope: index.Scope, pool: p, client: c, user: u}
		}

		if !a.pool.SoftwareTokenMFA {
			return awserror.New(
				"SoftwareTokenMFANotFoundException",
				"Software Token MFA has not been enabled by the userPool.",
			)
		}

		if err := fn(tx, a, sess); err != nil {
			return err
		}

		if sess != nil {
			deleteAuthSession(tx, a.scope, a.pool, *id)
		}

		return storage.Write(tx, userKey(a.scope, a.pool, a.user.Username), a.user)
	})
}

func (s *Service) AssociateSoftwareToken(
	ctx context.Context, in *api.AssociateSoftwareTokenRequest,
) (*api.AssociateSoftwareTokenResponse, error) {
	out := &api.AssociateSoftwareTokenResponse{}
	err := s.tokenSetup(ctx, in.AccessToken, in.Session, setupAssociate,
		func(tx storage.WriteTx, a *accessToken, sess *authSession) error {
			key := make([]byte, 20)
			_, _ = rand.Read(key)
			secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
			out.SecretCode = new(secret)
			if sess == nil {
				a.user.PendingTOTP = secret
				a.user.PendingTOTPExpires = s.now().
					Add(time.Duration(common.Value(a.client.Settings.AuthSessionValidity)) * time.Minute)
				a.user.PendingTOTPClient, a.user.PendingTOTPOrigin = a.client.ID, a.origin
				return nil
			}

			sess.Setup, sess.TOTPSecret = setupVerify, secret
			next, nextErr := s.newChallenge(tx, a.scope, a.pool, a.client, a.user, sess, nil)
			if nextErr != nil {
				return nextErr
			}

			out.Session = next.Session
			return nil
		})
	return out, err
}

func (s *Service) VerifySoftwareToken(
	ctx context.Context, in *api.VerifySoftwareTokenRequest,
) (*api.VerifySoftwareTokenResponse, error) {
	if len(common.Value(in.UserCode)) != 6 || !digitsOnly(common.Value(in.UserCode)) {
		return nil, invalidParameter("UserCode must contain exactly six digits.")
	}

	out := &api.VerifySoftwareTokenResponse{Status: new("SUCCESS")}
	err := s.tokenSetup(ctx, in.AccessToken, in.Session, setupVerify,
		func(tx storage.WriteTx, a *accessToken, sess *authSession) error {
			secret := a.user.PendingTOTP
			if sess == nil {
				if secret == "" || !s.now().Before(a.user.PendingTOTPExpires) ||
					a.user.PendingTOTPClient != a.client.ID ||
					a.user.PendingTOTPOrigin != a.origin {
					return expiredSession()
				}
			} else {
				secret = sess.TOTPSecret
			}

			if !validTOTP(secret, common.Value(in.UserCode), s.now()) {
				return codeMismatch()
			}

			a.user.TOTPSecret, a.user.TOTPMFA, a.user.TOTPDeviceName = secret, true, common.Value(in.FriendlyDeviceName)
			a.user.PendingTOTP, a.user.PendingTOTPClient, a.user.PendingTOTPOrigin = "", "", ""
			a.user.Modified = s.now().UTC()
			if sess != nil {
				sess.Setup, sess.TOTPSecret = setupComplete, ""
				next, nextErr := s.newChallenge(tx, a.scope, a.pool, a.client, a.user, sess, nil)
				if nextErr != nil {
					return nextErr
				}

				out.Session = next.Session
			}

			return nil
		})
	return out, err
}

// setMFA builds and validates the entire candidate before replacing the user's settings.
func setMFA(p *pool, u *user, sms *api.SMSMfaSettingsType, totp *api.SoftwareTokenMfaSettingsType) error {
	next := *u
	if sms != nil {
		next.SMSMFA = common.Value(sms.Enabled)
		if common.Value(sms.PreferredMfa) {
			next.PreferredMFA = challengeSMS
		} else if next.PreferredMFA == challengeSMS {
			next.PreferredMFA = ""
		}
	}

	if totp != nil {
		next.TOTPMFA = common.Value(totp.Enabled)
		if common.Value(totp.PreferredMfa) {
			next.PreferredMFA = challengeTOTP
		} else if next.PreferredMFA == challengeTOTP {
			next.PreferredMFA = ""
		}
	}

	switch {
	case sms != nil && common.Value(sms.PreferredMfa) && !next.SMSMFA,
		totp != nil && common.Value(totp.PreferredMfa) && !next.TOTPMFA:
		return invalidParameter("A preferred MFA factor must be enabled.")
	case sms != nil && totp != nil && common.Value(sms.PreferredMfa) && common.Value(totp.PreferredMfa):
		return invalidParameter("Only one MFA factor can be preferred.")
	case next.TOTPMFA && next.TOTPSecret == "":
		return invalidParameter("User has not verified software token MFA.")
	case next.SMSMFA && (p.Settings.SmsConfiguration == nil || next.Attributes[attrPhone] == ""):
		return invalidParameter("SMS MFA requires pool SMS configuration and the user's phone_number.")
	case common.Value(p.Settings.MfaConfiguration) == "ON" &&
		((u.SMSMFA && !next.SMSMFA) || (u.TOTPMFA && !next.TOTPMFA)):
		return invalidParameter("Cannot disable an MFA factor when MFA is required.")
	}

	*u = next
	return nil
}

func (s *Service) SetUserMFAPreference(
	ctx context.Context, in *api.SetUserMFAPreferenceRequest,
) (*api.SetUserMFAPreferenceResponse, error) {
	err := s.asUser(ctx, in.AccessToken, func(_ storage.WriteTx, a *accessToken, _ *outbox) error {
		return setMFA(a.pool, a.user, in.SMSMfaSettings, in.SoftwareTokenMfaSettings)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetUserMFAPreferenceResponse{}, nil
}

func (s *Service) AdminSetUserMFAPreference(
	ctx context.Context, in *api.AdminSetUserMFAPreferenceRequest,
) (*api.AdminSetUserMFAPreferenceResponse, error) {
	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, _ request.Scope, p *pool, u *user, _ *outbox) error {
			return setMFA(p, u, in.SMSMfaSettings, in.SoftwareTokenMfaSettings)
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminSetUserMFAPreferenceResponse{}, nil
}

func (s *Service) AdminSetUserSettings(
	ctx context.Context, in *api.AdminSetUserSettingsRequest,
) (*api.AdminSetUserSettingsResponse, error) {
	for _, option := range in.MFAOptions {
		if common.Value(option.DeliveryMedium) != mediumSMS || common.Value(option.AttributeName) != attrPhone {
			return nil, invalidParameter("MFAOptions supports only SMS and phone_number.")
		}
	}

	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, _ request.Scope, p *pool, u *user, _ *outbox) error {
			return setMFA(p, u, &api.SMSMfaSettingsType{Enabled: new(len(in.MFAOptions) > 0)}, nil)
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminSetUserSettingsResponse{}, nil
}

func digitsOnly(code string) bool {
	for _, digit := range code {
		if digit < '0' || digit > '9' {
			return false
		}
	}

	return true
}
