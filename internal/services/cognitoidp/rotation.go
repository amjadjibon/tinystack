package cognitoidp

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (c *clientSettings) rotates() bool {
	return c.RefreshTokenRotation != nil && common.Value(c.RefreshTokenRotation.Feature) == featureEnabled
}

func (s *Service) GetTokensFromRefreshToken(
	ctx context.Context, in *api.GetTokensFromRefreshTokenRequest,
) (*api.GetTokensFromRefreshTokenResponse, error) {
	var out *outcome
	err := s.byClient(ctx, in.ClientId, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		if !c.acceptsSecret(common.Value(in.ClientSecret)) {
			return notAuthorized("Client secret is invalid")
		}

		// Reuse the same token/user/revocation checks without SECRET_HASH: this operation uses ClientSecret.
		params := map[string]string{"REFRESH_TOKEN": common.Value(in.RefreshToken)}
		r, _, readErr := storage.Read[refresh](tx, refreshKey(scope, p.ID, common.Value(in.RefreshToken)))
		if readErr != nil {
			return readErr
		}

		if r != nil {
			params[paramSecretHash] = secretHash(c, r.Username)
		}

		var refreshErr error
		out, refreshErr = s.refresh(ctx, tx, scope, p, c, params)
		if refreshErr != nil {
			return refreshErr
		}

		if out.grant.preToken != nil && in.ClientMetadata != nil {
			out.grant.preToken.event.Request["clientMetadata"] = in.ClientMetadata
		}

		if !c.Settings.rotates() {
			return nil
		}

		now := s.now().UTC()
		if r.RotatedUntil.IsZero() {
			r.RotatedUntil = now.Add(
				time.Duration(common.Value(c.Settings.RefreshTokenRotation.RetryGracePeriodSeconds)) * time.Second,
			)
			if err := storage.Write(tx, refreshKey(scope, p.ID, *in.RefreshToken), r); err != nil {
				return err
			}
		}

		replacement := *r
		replacement.RotatedUntil = time.Time{}
		token := common.RandomID(256, alphaNum)
		if err := storage.Write(tx, refreshKey(scope, p.ID, token), replacement); err != nil {
			return err
		}

		out.grant.refresh = token
		return nil
	})
	return respondWith[api.GetTokensFromRefreshTokenResponse](ctx, s, out, err)
}
