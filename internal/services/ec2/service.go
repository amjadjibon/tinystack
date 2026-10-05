// Package ec2 implements a subset of Amazon EC2 without HTTP dependencies. It
// keeps resources as records and runs no virtual machines.
package ec2

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store    storage.Store
	now      func() time.Time
	profiles InstanceProfiles
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

func invalidParameter(message string) error {
	return awserror.New("InvalidParameterValue", message)
}

// caller resolves the caller and answers DryRun.
func caller(ctx context.Context, dry *bool) (request.Metadata, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return m, err
	}

	return m, dryRun(dry)
}

// dryRun answers a request with DryRun set: TinyStack authorizes every
// caller, so it would have succeeded.
func dryRun(flag *bool) error {
	if !common.Value(flag) {
		return nil
	}

	return &awserror.Error{
		Code:       "DryRunOperation",
		Message:    "Request would have succeeded, but DryRun flag is set.",
		StatusCode: 412,
	}
}
