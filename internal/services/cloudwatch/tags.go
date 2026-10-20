package cloudwatch

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagged finds the record key an alarm or dashboard ARN names in scope.
func tagged(scope request.Scope, arn string) (string, error) {
	if name, ok := strings.CutPrefix(arn, "arn:aws:cloudwatch:"+scope.Region+":"+scope.AccountID+":alarm:"); ok {
		return alarmKey(scope, name), nil
	}

	if name, ok := strings.CutPrefix(arn, "arn:aws:cloudwatch::"+scope.AccountID+":dashboard/"); ok {
		return dashboardKey(scope, name), nil
	}

	if name, ok := strings.CutPrefix(arn, streamARN(scope, "")); ok {
		return streamKey(scope, name), nil
	}

	if !strings.HasPrefix(arn, "arn:") {
		return "", invalidValue("The value %s for parameter ResourceARN is not a valid ARN.", arn)
	}

	return "", notFound(arn, nil)
}

// retag applies change to the tags of the alarm or dashboard arn names.
func (s *Service) retag(ctx context.Context, arn string, change func(map[string]string) (map[string]string, error),
) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	key, err := tagged(m.Scope, arn)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if strings.HasPrefix(key, m.Prefix(service, kindStream)) {
			st, found, readErr := storage.Read[metricStream](tx, key)
			if readErr != nil || !found {
				return notFound(arn, readErr)
			}

			if st.Tags, readErr = change(st.Tags); readErr != nil {
				return readErr
			}

			return storage.Write(tx, key, st)
		}

		if strings.HasPrefix(key, m.Prefix(service, kindAlarm)) {
			a, found, readErr := storage.Read[alarm](tx, key)
			if readErr != nil || !found {
				return notFound(arn, readErr)
			}

			if a.Tags, readErr = change(a.Tags); readErr != nil {
				return readErr
			}

			return storage.Write(tx, key, a)
		}

		d, found, readErr := storage.Read[dashboard](tx, key)
		if readErr != nil || !found {
			return notFound(arn, readErr)
		}

		if d.Tags, readErr = change(d.Tags); readErr != nil {
			return readErr
		}

		return storage.Write(tx, key, d)
	})
}

func notFound(arn string, err error) error {
	if err != nil {
		return err
	}

	return awserror.New("ResourceNotFoundException", "The resource "+arn+" does not exist.")
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	if len(in.Tags) == 0 {
		return nil, missing("Tags")
	}

	return &api.TagResourceOutput{}, s.retag(ctx, common.Value(in.ResourceARN),
		func(tags map[string]string) (map[string]string, error) {
			merged, err := tagging.Merge(tags, tagsOf(in.Tags))
			if err != nil {
				return nil, awserror.New("LimitExceededFault", err.Error())
			}

			return merged, nil
		})
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	return &api.UntagResourceOutput{}, s.retag(ctx, common.Value(in.ResourceARN),
		func(tags map[string]string) (map[string]string, error) {
			return tagging.Remove(tags, in.TagKeys), nil
		})
}

func (s *Service) ListTagsForResource(
	ctx context.Context, in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	key, err := tagged(m.Scope, common.Value(in.ResourceARN))
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if strings.HasPrefix(key, m.Prefix(service, kindStream)) {
			st, found, readErr := storage.Read[metricStream](tx, key)
			if readErr != nil || !found {
				return notFound(*in.ResourceARN, readErr)
			}

			tags = st.Tags
			return nil
		}

		if strings.HasPrefix(key, m.Prefix(service, kindAlarm)) {
			a, found, readErr := storage.Read[alarm](tx, key)
			if readErr != nil || !found {
				return notFound(*in.ResourceARN, readErr)
			}

			tags = a.Tags
			return nil
		}

		d, found, readErr := storage.Read[dashboard](tx, key)
		if readErr != nil || !found {
			return notFound(*in.ResourceARN, readErr)
		}

		tags = d.Tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceOutput{
		Tags: tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} }),
	}, nil
}
