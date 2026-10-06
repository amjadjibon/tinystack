package cognitoidentity

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidentity"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// identity is an identity's record.
type identity struct {
	// Logins are the subjects of the logins linked to it, by provider; a
	// developer provider's subject is the developer user identifier.
	Logins   map[string]string
	ID       string
	Pool     string
	Created  float64
	Modified float64
}

// located is an identity's partition-wide index entry.
type located struct {
	Account string
	Pool    string
}

func loginKey(scope request.Scope, pool, provider, subject string) string {
	return scope.Key(service, kindLogin, pool+"/"+url.PathEscape(provider)+"/"+url.PathEscape(subject))
}

func identityIndex(id string) string { return indexPrefix + kindIdentity + "/" + id }

func errNoIdentity(id string) error { return notFound("Identity '%s' not found.", id) }

func (i *identity) describe() api.IdentityDescription {
	d := api.IdentityDescription{
		IdentityId: new(i.ID), CreationDate: new(i.Created), LastModifiedDate: new(i.Modified),
	}
	for provider := range i.Logins {
		d.Logins = append(d.Logins, provider)
	}

	slices.Sort(d.Logins)
	return d
}

// removeIdentity deletes an identity, its logins, and its index entry.
func removeIdentity(tx storage.WriteTx, scope request.Scope, i *identity) {
	for provider, subject := range i.Logins {
		tx.Delete(loginKey(scope, i.Pool, provider, subject))
	}

	tx.Delete(identityKey(scope, i.Pool, i.ID))
	tx.Delete(identityIndex(i.ID))
}

// link finds the identity verified logins belong to (id, when it is set),
// or creates one, and links the logins to it.
func link(tx storage.WriteTx, scope request.Scope, poolID string, logins []login, id string, now float64) (
	*identity, error,
) {
	for _, l := range logins {
		var linked string
		if l.provider == developerLogin {
			linked = l.subject
		} else if v, ok := tx.Get(loginKey(scope, poolID, l.provider, l.subject)); ok {
			linked = string(v)
		}

		switch {
		case linked == "":
		case id != "" && linked != id:
			return nil, awserror.New("ResourceConflictException",
				"The logins are linked to another identity, "+linked+".")
		default:
			id = linked
		}
	}

	i := &identity{ID: scope.Region + ":" + uuid.NewV4().String(), Pool: poolID, Created: now}
	if id != "" {
		found, ok, err := storage.Read[identity](tx, identityKey(scope, poolID, id))
		if err != nil || !ok {
			return nil, common.ErrorsOr(err, errNoIdentity(id))
		}

		i = found
	}

	if i.Logins == nil {
		i.Logins = map[string]string{}
	}

	for _, l := range logins {
		if l.provider != developerLogin {
			i.Logins[l.provider] = l.subject
			tx.Put(loginKey(scope, poolID, l.provider, l.subject), []byte(i.ID))
		}
	}

	i.Modified = now
	if err := storage.Write(tx, identityIndex(i.ID), located{Account: scope.AccountID, Pool: poolID}); err != nil {
		return nil, err
	}

	return i, storage.Write(tx, identityKey(scope, poolID, i.ID), i)
}

func (s *Service) GetId(ctx context.Context, in *api.GetIdInput) (*api.GetIdResponse, error) {
	var scope request.Scope
	var p *pool
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		scope, p, err = locatePool(tx, in.IdentityPoolId)
		return err
	})
	if err != nil {
		return nil, err
	}

	if in.AccountId != nil && *in.AccountId != scope.AccountID {
		return nil, notFound("IdentityPool '%s' not found.", *in.IdentityPoolId)
	}

	logins, err := s.verifyLogins(ctx, scope, p, in.Logins)
	if err != nil {
		return nil, err
	}

	if len(logins) == 0 && !common.Value(p.Pool.AllowUnauthenticatedIdentities) {
		return nil, notAuthorized("Unauthenticated access is not supported for this identity pool.")
	}

	var i *identity
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var linkErr error
		i, linkErr = link(tx, scope, *p.Pool.IdentityPoolId, logins, "", common.Unix(s.now()))
		return linkErr
	})
	if err != nil {
		return nil, err
	}

	return &api.GetIdResponse{IdentityId: new(i.ID)}, nil
}

// session is an identity authenticated with the logins its request
// presents, or none for an unauthenticated identity.
type session struct {
	scope    request.Scope
	pool     *pool
	identity *identity
	logins   []login
}

// methods are how the session authenticated, as tokens' amr claims say.
func (ses *session) methods() []string {
	if len(ses.logins) == 0 {
		return []string{roleUnauth}
	}

	out := []string{roleAuth}
	for _, l := range ses.logins {
		provider := l.provider
		if provider == developerLogin {
			provider = common.Value(ses.pool.Pool.DeveloperProviderName)
		}

		out = append(out, provider)
	}

	return out
}

// authenticate checks that logins authenticate the identity id names, which
// with none must be an unauthenticated identity of a pool that allows them,
// and links any new logins to it.
func (s *Service) authenticate(ctx context.Context, id *string, logins map[string]string) (*session, error) {
	ses := &session{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		loc, ok, err := storage.Read[located](tx, identityIndex(common.Value(id)))
		if err != nil || !ok {
			return common.ErrorsOr(err, errNoIdentity(common.Value(id)))
		}

		if ses.scope, ses.pool, err = locatePool(tx, &loc.Pool); err != nil {
			return err
		}

		var found bool
		ses.identity, found, err = storage.Read[identity](tx, identityKey(ses.scope, loc.Pool, *id))
		if err == nil && !found {
			err = errNoIdentity(*id)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	if ses.logins, err = s.verifyLogins(ctx, ses.scope, ses.pool, logins); err != nil {
		return nil, err
	}

	if len(ses.logins) == 0 {
		switch {
		case len(ses.identity.Logins) > 0:
			return nil, notAuthorized("Logins don't match. Please include at least one valid login for this " +
				"identity or identity pool.")
		case !common.Value(ses.pool.Pool.AllowUnauthenticatedIdentities):
			return nil, notAuthorized("Unauthenticated access is not supported for this identity pool.")
		}

		return ses, nil
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var linkErr error
		ses.identity, linkErr = link(tx, ses.scope, ses.identity.Pool, ses.logins, ses.identity.ID,
			common.Unix(s.now()))
		return linkErr
	})

	return ses, err
}

func (s *Service) GetOpenIdToken(
	ctx context.Context,
	in *api.GetOpenIdTokenInput,
) (*api.GetOpenIdTokenResponse, error) {
	ses, err := s.authenticate(ctx, in.IdentityId, in.Logins)
	if err != nil {
		return nil, err
	}

	token, err := ses.pool.token(ses.identity.ID, ses.methods(), tokenLife, s.now())
	if err != nil {
		return nil, err
	}

	return &api.GetOpenIdTokenResponse{IdentityId: new(ses.identity.ID), Token: new(token)}, nil
}

func (s *Service) GetCredentialsForIdentity(
	ctx context.Context,
	in *api.GetCredentialsForIdentityInput,
) (*api.GetCredentialsForIdentityResponse, error) {
	ses, err := s.authenticate(ctx, in.IdentityId, in.Logins)
	if err != nil {
		return nil, err
	}

	role, err := chooseRole(ses.pool, ses.logins, common.Value(in.CustomRoleArn))
	if err != nil {
		return nil, err
	}

	token, err := ses.pool.token(ses.identity.ID, ses.methods(), tokenLife, s.now())
	if err != nil {
		return nil, err
	}

	credentials, err := s.assume(ctx, ses.scope, role, token)
	if err != nil {
		return nil, err
	}

	return &api.GetCredentialsForIdentityResponse{IdentityId: new(ses.identity.ID), Credentials: credentials}, nil
}

// assume exchanges an identity's OpenID token for a role's credentials with
// STS, which checks that the role trusts cognito-identity.amazonaws.com.
func (s *Service) assume(ctx context.Context, scope request.Scope, role, token string) (*api.Credentials, error) {
	if s.caller == nil {
		return nil, errors.New("cognitoidentity: no caller to reach STS")
	}

	out, err := s.caller.Call(request.WithMetadata(ctx, request.Metadata{Scope: scope}), "sts",
		"AssumeRoleWithWebIdentity", map[string]any{
			"RoleArn": role, "RoleSessionName": "CognitoIdentityCredentials", "WebIdentityToken": token,
		})
	if ae, ok := errors.AsType[*awserror.Error](err); ok {
		return nil, awserror.New("InvalidIdentityPoolConfigurationException", fmt.Sprintf(
			"Invalid identity pool configuration. Check assigned IAM roles for this pool. (%s: %s)", ae.Code,
			ae.Message))
	}

	if err != nil {
		return nil, err
	}

	c, _ := out["Credentials"].(map[string]any)
	value := func(name string) *string {
		v, _ := c[name].(string)
		return new(v)
	}
	expires, err := time.Parse(time.RFC3339Nano, *value("Expiration"))
	if err != nil {
		return nil, err
	}

	return &api.Credentials{
		AccessKeyId: value("AccessKeyId"), SecretKey: value("SecretAccessKey"), SessionToken: value("SessionToken"),
		Expiration: new(common.Unix(expires)),
	}, nil
}

func (s *Service) GetOpenIdTokenForDeveloperIdentity(
	ctx context.Context,
	in *api.GetOpenIdTokenForDeveloperIdentityInput,
) (*api.GetOpenIdTokenForDeveloperIdentityResponse, error) {
	life := time.Duration(common.Value(common.OrElse(in.TokenDuration, new(int64(900))))) * time.Second
	if life < time.Second || life > 24*time.Hour {
		return nil, invalid("TokenDuration must be between 1 and 86400 seconds.")
	}

	var out *api.GetOpenIdTokenForDeveloperIdentityResponse
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		p, err := loadPool(tx, scope, in.IdentityPoolId)
		if err != nil {
			return err
		}

		developer := common.Value(p.Pool.DeveloperProviderName)
		user, ok := in.Logins[developer]
		if developer == "" || !ok || len(in.Logins) != 1 {
			return invalid("Logins must name only the identity pool's developer provider.")
		}

		id := common.Value(in.IdentityId)
		if linked, isLinked := tx.Get(loginKey(scope, *in.IdentityPoolId, developer, user)); isLinked &&
			id != "" && string(linked) != id {
			return awserror.New("DeveloperUserAlreadyRegisteredException",
				"The developer user identifier is already registered with another identity.")
		}

		i, err := link(tx, scope, *in.IdentityPoolId, []login{{provider: developer, subject: user}}, id,
			common.Unix(s.now()))
		if err != nil {
			return err
		}

		token, err := p.token(i.ID, []string{roleAuth, developer}, life, s.now())
		out = &api.GetOpenIdTokenForDeveloperIdentityResponse{IdentityId: new(i.ID), Token: new(token)}
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) LookupDeveloperIdentity(
	ctx context.Context,
	in *api.LookupDeveloperIdentityInput,
) (*api.LookupDeveloperIdentityResponse, error) {
	if in.IdentityId == nil && in.DeveloperUserIdentifier == nil {
		return nil, invalid("Either IdentityId or DeveloperUserIdentifier is required.")
	}

	out := &api.LookupDeveloperIdentityResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		p, err := loadPool(tx, scope, in.IdentityPoolId)
		if err != nil {
			return err
		}

		developer := common.Value(p.Pool.DeveloperProviderName)
		id := common.Value(in.IdentityId)
		if in.DeveloperUserIdentifier != nil {
			linked, ok := tx.Get(loginKey(scope, *in.IdentityPoolId, developer, *in.DeveloperUserIdentifier))
			if !ok {
				return errNoIdentity(*in.DeveloperUserIdentifier)
			}

			id = string(linked)
		}

		i, ok, err := storage.Read[identity](tx, identityKey(scope, *in.IdentityPoolId, id))
		if err != nil || !ok {
			return common.ErrorsOr(err, errNoIdentity(id))
		}

		out.IdentityId, out.DeveloperUserIdentifierList = new(i.ID), []string{}
		if user, linked := i.Logins[developer]; linked && developer != "" {
			out.DeveloperUserIdentifierList = []string{user}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UnlinkIdentity(ctx context.Context, in *api.UnlinkIdentityInput) (*api.UnlinkIdentityOutput, error) {
	ses, err := s.authenticate(ctx, in.IdentityId, in.Logins)
	if err != nil {
		return nil, err
	}

	if len(ses.logins) == 0 {
		return nil, notAuthorized("Logins don't match. Please include at least one valid login for this identity.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		i, ok, readErr := storage.Read[identity](tx, identityKey(ses.scope, ses.identity.Pool, ses.identity.ID))
		if readErr != nil || !ok {
			return common.ErrorsOr(readErr, errNoIdentity(ses.identity.ID))
		}

		for _, provider := range in.LoginsToRemove {
			if subject, linked := i.Logins[provider]; linked {
				tx.Delete(loginKey(ses.scope, i.Pool, provider, subject))
				delete(i.Logins, provider)
			}
		}

		i.Modified = common.Unix(s.now())
		return storage.Write(tx, identityKey(ses.scope, i.Pool, i.ID), i)
	})
	if err != nil {
		return nil, err
	}

	return &api.UnlinkIdentityOutput{}, nil
}

func (s *Service) ListIdentities(
	ctx context.Context,
	in *api.ListIdentitiesInput,
) (*api.ListIdentitiesResponse, error) {
	var all []*identity
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadPool(tx, scope, in.IdentityPoolId); err != nil {
			return err
		}

		var err error
		all, err = storage.Records[identity](tx, identityKey(scope, *in.IdentityPoolId, ""))
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := list(all, func(i *identity) string { return i.ID }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.ListIdentitiesResponse{
		IdentityPoolId: in.IdentityPoolId, Identities: []api.IdentityDescription{}, NextToken: next,
	}
	for _, i := range selected {
		out.Identities = append(out.Identities, i.describe())
	}

	return out, nil
}

// owned reads an identity of the caller's account.
func owned(tx storage.ReadTx, scope request.Scope, id string) (*identity, error) {
	loc, ok, err := storage.Read[located](tx, identityIndex(id))
	if err != nil || !ok || loc.Account != scope.AccountID {
		return nil, common.ErrorsOr(err, errNoIdentity(id))
	}

	if m := poolIDPattern.FindStringSubmatch(loc.Pool); m != nil {
		scope.Region = m[1]
	}

	i, ok, err := storage.Read[identity](tx, identityKey(scope, loc.Pool, id))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, errNoIdentity(id))
	}

	return i, nil
}

func (s *Service) DescribeIdentity(
	ctx context.Context,
	in *api.DescribeIdentityInput,
) (*api.IdentityDescription, error) {
	var i *identity
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		i, err = owned(tx, scope, common.Value(in.IdentityId))
		return err
	})
	if err != nil {
		return nil, err
	}

	d := i.describe()
	return &d, nil
}

func (s *Service) DeleteIdentities(
	ctx context.Context,
	in *api.DeleteIdentitiesInput,
) (*api.DeleteIdentitiesResponse, error) {
	if len(in.IdentityIdsToDelete) == 0 || len(in.IdentityIdsToDelete) > maxListResult {
		return nil, invalid("IdentityIdsToDelete must have 1 to %d identities.", maxListResult)
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		for _, id := range in.IdentityIdsToDelete {
			i, err := owned(tx, scope, id)
			var notFoundErr *awserror.Error
			if errors.As(err, &notFoundErr) {
				continue
			}

			if err != nil {
				return err
			}

			if m := poolIDPattern.FindStringSubmatch(i.Pool); m != nil {
				scope.Region = m[1]
			}

			removeIdentity(tx, scope, i)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteIdentitiesResponse{UnprocessedIdentityIds: []api.UnprocessedIdentityId{}}, nil
}
