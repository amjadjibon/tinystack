// Package cloudtrail implements regional trail records and bounded SDK event history.
package cloudtrail

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudtrail"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName           = "cloudtrail"
	attributeReadOnly     = "ReadOnly"
	attributeResourceName = "ResourceName"
	attributeResourceType = "ResourceType"
)

var (
	_         api.API = (*Service)(nil)
	trailName         = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{1,126}[a-zA-Z0-9]$`)
)

// Records: <account>/<region>/cloudtrail/trail/<name> and history/events.
// Event history persists with the configured store, independently of trails.
type record struct {
	Trail            api.Trail
	Tags             map[string]string
	Started, Stopped *float64
	Selectors        []api.EventSelector
	Advanced         []api.AdvancedEventSelector
	Logging          bool
}

type Service struct {
	store   storage.Store
	buckets *s3.Service
}

func New(store storage.Store, buckets *s3.Service) *Service {
	return &Service{store: store, buckets: buckets}
}

func invalid(message string) error { return awserror.New("InvalidParameterException", message) }
func notFound() error              { return awserror.New("TrailNotFoundException", "Trail does not exist") }

func arn(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:cloudtrail:%s:%s:trail/%s", scope.Region, scope.AccountID, name)
}

func nameOf(scope request.Scope, name string) (string, error) {
	if strings.HasPrefix(name, "arn:") {
		prefix := fmt.Sprintf("arn:aws:cloudtrail:%s:%s:trail/", scope.Region, scope.AccountID)
		if !strings.HasPrefix(name, prefix) {
			return "", notFound()
		}

		return strings.TrimPrefix(name, prefix), nil
	}

	return name, nil
}

func read(tx storage.ReadTx, scope request.Scope, name string) (*record, error) {
	name, err := nameOf(scope, name)
	if err != nil {
		return nil, err
	}

	r, found, err := storage.Read[record](tx, scope.Key(serviceName, "trail", name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, notFound()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, "trail", common.Value(r.Trail.Name)), r)
}

func (s *Service) get(ctx context.Context, name string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error { var e error; r, e = read(tx, scope, name); return e })
	return r, err
}

func (s *Service) edit(ctx context.Context, name string, change func(*record) error) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, name)
		if e != nil {
			return e
		}

		if changeErr := change(r); changeErr != nil {
			return changeErr
		}

		out = r
		return save(tx, scope, r)
	})
	return out, err
}

func (s *Service) checkBucket(ctx context.Context, bucket *string) error {
	if common.Value(bucket) == "" {
		return invalid("S3BucketName is required")
	}

	_, err := s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: bucket})
	if err != nil {
		return awserror.New("S3BucketDoesNotExistException", "S3 bucket does not exist")
	}

	return nil
}

func (s *Service) CreateTrail(ctx context.Context, in *api.CreateTrailRequest) (*api.CreateTrailResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if !trailName.MatchString(name) {
		return nil, awserror.New("InvalidTrailNameException", "Invalid trail name")
	}

	if bucketErr := s.checkBucket(ctx, in.S3BucketName); bucketErr != nil {
		return nil, bucketErr
	}

	tags, err := parseTags(in.TagsList, false)
	if err != nil {
		return nil, err
	}

	r := &record{
		Trail: api.Trail{
			Name: in.Name, S3BucketName: in.S3BucketName, S3KeyPrefix: in.S3KeyPrefix,
			TrailARN: new(arn(scope, name)), HomeRegion: new(scope.Region),
		}, Tags: tags,
		Selectors: []api.EventSelector{{ReadWriteType: new("All"), IncludeManagementEvents: new(true)}},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found, e := storage.Read[record](tx, scope.Key(serviceName, "trail", name)); e != nil {
			return e
		} else if found {
			return awserror.New("TrailAlreadyExistsException", "Trail already exists")
		}

		return save(tx, scope, r)
	})
	return &api.CreateTrailResponse{
		Name: r.Trail.Name, S3BucketName: r.Trail.S3BucketName, S3KeyPrefix: r.Trail.S3KeyPrefix,
		TrailARN: r.Trail.TrailARN,
	}, err
}

func (s *Service) GetTrail(ctx context.Context, in *api.GetTrailRequest) (*api.GetTrailResponse, error) {
	r, err := s.get(ctx, common.Value(in.Name))
	if err != nil {
		return nil, err
	}

	return &api.GetTrailResponse{Trail: &r.Trail}, nil
}

func (s *Service) DescribeTrails(
	ctx context.Context,
	in *api.DescribeTrailsRequest,
) (*api.DescribeTrailsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTrailsResponse{TrailList: []api.Trail{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if len(in.TrailNameList) > 0 {
			for _, name := range in.TrailNameList {
				r, e := read(tx, scope, name)
				if e != nil {
					return e
				}

				out.TrailList = append(out.TrailList, r.Trail)
			}

			return nil
		}

		for _, k := range tx.Keys(scope.Prefix(serviceName, "trail")) {
			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			out.TrailList = append(out.TrailList, r.Trail)
		}

		return nil
	})
	return out, err
}

func (s *Service) UpdateTrail(ctx context.Context, in *api.UpdateTrailRequest) (*api.UpdateTrailResponse, error) {
	if in.S3BucketName != nil {
		if err := s.checkBucket(ctx, in.S3BucketName); err != nil {
			return nil, err
		}
	}

	r, err := s.edit(ctx, common.Value(in.Name), func(r *record) error {
		if in.S3BucketName != nil {
			r.Trail.S3BucketName = in.S3BucketName
		}

		if in.S3KeyPrefix != nil {
			r.Trail.S3KeyPrefix = in.S3KeyPrefix
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateTrailResponse{
		Name: r.Trail.Name, S3BucketName: r.Trail.S3BucketName, S3KeyPrefix: r.Trail.S3KeyPrefix,
		TrailARN: r.Trail.TrailARN,
	}, nil
}

func (s *Service) DeleteTrail(ctx context.Context, in *api.DeleteTrailRequest) (*api.DeleteTrailResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, common.Value(in.Name))
		if e != nil {
			return e
		}

		tx.Delete(scope.Key(serviceName, "trail", common.Value(r.Trail.Name)))
		return nil
	})
	return &api.DeleteTrailResponse{}, err
}

func (s *Service) GetTrailStatus(
	ctx context.Context,
	in *api.GetTrailStatusRequest,
) (*api.GetTrailStatusResponse, error) {
	r, err := s.get(ctx, common.Value(in.Name))
	if err != nil {
		return nil, err
	}

	return &api.GetTrailStatusResponse{
		IsLogging:        new(r.Logging),
		StartLoggingTime: r.Started,
		StopLoggingTime:  r.Stopped,
	}, nil
}

func (s *Service) StartLogging(ctx context.Context, in *api.StartLoggingRequest) (*api.StartLoggingResponse, error) {
	_, err := s.edit(ctx, common.Value(in.Name), func(r *record) error {
		r.Logging = true
		r.Started = new(float64(time.Now().Unix()))
		return nil
	})
	return &api.StartLoggingResponse{}, err
}

func (s *Service) StopLogging(ctx context.Context, in *api.StopLoggingRequest) (*api.StopLoggingResponse, error) {
	_, err := s.edit(ctx, common.Value(in.Name), func(r *record) error {
		r.Logging = false
		r.Stopped = new(float64(time.Now().Unix()))
		return nil
	})
	return &api.StopLoggingResponse{}, err
}
