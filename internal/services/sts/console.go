//go:build console

package sts

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// ConsoleSession issues a short-lived copy of an already verified console identity.
// It has no HTTP API; only the console gateway calls it after authenticating.
func (s *Service) ConsoleSession(ctx context.Context, verified *request.Credentials) (*api.Credentials, error) {
	now := s.now()
	expires := now.Add(15 * time.Minute)
	if !verified.Expires.IsZero() && verified.Expires.Before(expires) {
		expires = verified.Expires
	}

	p := verified.Principal
	issued := &session{
		AccountID: verified.AccountID, UserARN: p.ARN, UserID: p.ID,
		Federated: verified.Federated, CognitoIdentity: verified.CognitoIdentity, Expires: expires.Unix(),
	}

	if p.Kind == request.PrincipalRole {
		issued.RoleARN, issued.RoleName, issued.RoleID = p.PolicyARN, p.Name, p.ID
		_, issued.Name, _ = strings.Cut(p.UserID, ":")
	}

	var out *api.Credentials
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		var err error
		out, err = issue(tx, issued, now)
		return err
	})
	return out, err
}
