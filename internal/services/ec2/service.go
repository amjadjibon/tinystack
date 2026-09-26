// Package ec2 implements a subset of Amazon EC2 without HTTP dependencies. It
// keeps resources as records and runs no virtual machines.
package ec2

import (
	"context"
	"errors"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}

	return v
}

func invalidParameter(message string) error {
	return awserror.New("InvalidParameterValue", message)
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" {
		return m, errors.New("missing request identity")
	}

	return m, nil
}

// dryRun answers a request with DryRun set: TinyStack authorizes every
// caller, so it would have succeeded.
func dryRun(flag *bool) error {
	if !value(flag) {
		return nil
	}

	return &awserror.Error{
		Code:       "DryRunOperation",
		Message:    "Request would have succeeded, but DryRun flag is set.",
		StatusCode: 412,
	}
}
