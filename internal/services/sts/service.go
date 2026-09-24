// Package sts implements the AWS Security Token Service without HTTP
// dependencies. Callers are identified by the account the gateway resolved for
// their access key, so a long-term key is that account's root user.
package sts

import (
	"context"
	"fmt"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

var _ api.API = (*Service)(nil)

type Service struct{}

func New() *Service {
	return &Service{}
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" {
		return m, fmt.Errorf("missing request identity")
	}

	return m, nil
}

// GetCallerIdentity returns the root user of the caller's account: TinyStack
// maps each access key to an account, not to a user within it.
func (s *Service) GetCallerIdentity(
	ctx context.Context,
	_ *api.GetCallerIdentityRequest,
) (*api.GetCallerIdentityResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	return &api.GetCallerIdentityResponse{
		UserId:  new(m.AccountID),
		Account: new(m.AccountID),
		Arn:     new("arn:aws:iam::" + m.AccountID + ":root"),
	}, nil
}
