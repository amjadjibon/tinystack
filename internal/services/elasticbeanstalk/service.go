// Package elasticbeanstalk implements a records-only Elastic Beanstalk control plane.
// Environments never deploy code, provision infrastructure, or serve application traffic.
package elasticbeanstalk

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticbeanstalk"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindApplication           = "application"
	kindEnvironment           = "environment"
	kindTemplate              = "template"
	kindVersion               = "version"
	kindConfigurationTemplate = "configurationtemplate"
	kindApplicationVersion    = "applicationversion"
)

const (
	serviceName      = "elasticbeanstalk"
	ready            = "Ready"
	terminated       = "Terminated"
	terminating      = "Terminating"
	updating         = "Updating"
	launching        = "Launching"
	invalidCode      = "InvalidParameterValue"
	webServer        = "WebServer"
	scalar           = "Scalar"
	scalingNamespace = "aws:autoscaling:asg"
)

var (
	_            api.API = (*Service)(nil)
	envPattern           = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{2,38}[A-Za-z0-9]$`)
	cnamePattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{2,61}[A-Za-z0-9]$`)
)

type Buckets interface {
	HeadObject(context.Context, *s3api.HeadObjectRequest) (*s3api.HeadObjectOutput, error)
	DeleteObject(context.Context, *s3api.DeleteObjectRequest) (*s3api.DeleteObjectOutput, error)
	CreateBucket(context.Context, *s3api.CreateBucketRequest) (*s3api.CreateBucketOutput, error)
	HeadBucket(context.Context, *s3api.HeadBucketRequest) (*s3api.HeadBucketOutput, error)
}

type Service struct {
	store   storage.Store
	buckets Buckets
	bg      *background.Group
}

type record struct {
	Application     *api.ApplicationDescription
	Version         *api.ApplicationVersionDescription
	Environment     *api.EnvironmentDescription
	Template        *api.ConfigurationSettingsDescription
	Tags            map[string]string
	Options         []api.ConfigurationOptionSetting
	Events          []api.EventDescription
	RetainResources bool
}

func New(store storage.Store, buckets Buckets) *Service {
	s := &Service{store: store, buckets: buckets, bg: background.New()}
	s.bg.Every(time.Second, s.tick)
	return s
}

func (s *Service) Stop() { s.bg.Stop() }

func invalid(message string) error { return awserror.New(invalidCode, message) }

func validName(v *string) error {
	if strings.TrimSpace(common.Value(v)) == "" || len(common.Value(v)) > 100 ||
		strings.ContainsAny(common.Value(v), "/:\\") {
		return invalid("Invalid name or version label.")
	}

	return nil
}

func tags(in []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		in,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag key and value required."),
		func(string) error { return invalid("Duplicate tag key.") },
	)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		if errors.Is(err, tagging.ErrLimit) {
			return nil, awserror.New("TooManyTagsException", err.Error())
		}

		return nil, invalid(err.Error())
	}

	return out, nil
}

func arn(sc request.Scope, kind, name string) *string {
	return new(fmt.Sprintf("arn:aws:elasticbeanstalk:%s:%s:%s/%s", sc.Region, sc.AccountID, kind, name))
}

func read(tx storage.ReadTx, sc request.Scope, kind, name string) (*record, error) {
	r, ok, err := storage.Read[record](tx, sc.Key(serviceName, kind, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, invalid("No such " + kind + ": " + name)
	}

	return r, nil
}

func (s *Service) change(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	sc, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, sc) })
}

func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	sc, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, sc) })
}

func limit(v *int64) (int, error) {
	n := *common.OrElse(v, new(int64(100)))
	if n < 1 || n > 1000 {
		return 0, invalid("MaxRecords must be between 1 and 1000.")
	}

	return int(n), nil
}

func matches(filter, value *string) bool {
	return filter == nil || common.Value(filter) == common.Value(value)
}

func contains(filter []string, value *string) bool {
	return len(filter) == 0 || slices.Contains(filter, common.Value(value))
}

func put(tx storage.WriteTx, sc request.Scope, kind, name string, r *record) error {
	return storage.Write(tx, sc.Key(serviceName, kind, name), r)
}

func (s *Service) CreateStorageLocation(
	ctx context.Context,
	_ *api.CreateStorageLocationInput,
) (*api.CreateStorageLocationResultMessage, error) {
	sc, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := new("elasticbeanstalk-" + sc.Region + "-" + sc.AccountID)
	if _, err = s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: name}); err != nil {
		in := &s3api.CreateBucketRequest{Bucket: name}
		if sc.Region != "us-east-1" {
			in.CreateBucketConfiguration = &s3api.CreateBucketConfiguration{LocationConstraint: new(sc.Region)}
		}

		if _, err = s.buckets.CreateBucket(ctx, in); err != nil {
			return nil, err
		}
	}

	return &api.CreateStorageLocationResultMessage{S3Bucket: name}, nil
}
