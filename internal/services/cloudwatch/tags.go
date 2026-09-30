package cloudwatch

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
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

	if !strings.HasPrefix(arn, "arn:") {
		return "", invalidValue("The value %s for parameter ResourceARN is not a valid ARN.", arn)
	}

	return "", notFound(arn, nil)
}

// retag applies change to the tags of the alarm or dashboard arn names.
func (s *Service) retag(ctx context.Context, arn string, change func(map[string]string) (map[string]string, error),
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	key, err := tagged(m.Scope, arn)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if strings.HasPrefix(key, prefix(m.Scope, kindAlarm)) {
			a, found, readErr := read[alarm](tx, key)
			if readErr != nil || !found {
				return notFound(arn, readErr)
			}

			if a.Tags, readErr = change(a.Tags); readErr != nil {
				return readErr
			}

			return write(tx, key, a)
		}

		d, found, readErr := read[dashboard](tx, key)
		if readErr != nil || !found {
			return notFound(arn, readErr)
		}

		if d.Tags, readErr = change(d.Tags); readErr != nil {
			return readErr
		}

		return write(tx, key, d)
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

	return &api.TagResourceOutput{}, s.retag(ctx, value(in.ResourceARN),
		func(tags map[string]string) (map[string]string, error) {
			merged, err := tagging.Merge(tags, tagsOf(in.Tags))
			if err != nil {
				return nil, awserror.New("LimitExceededFault", err.Error())
			}

			return merged, nil
		})
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	return &api.UntagResourceOutput{}, s.retag(ctx, value(in.ResourceARN),
		func(tags map[string]string) (map[string]string, error) {
			return tagging.Remove(tags, in.TagKeys), nil
		})
}

func (s *Service) ListTagsForResource(
	ctx context.Context, in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	key, err := tagged(m.Scope, value(in.ResourceARN))
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if strings.HasPrefix(key, prefix(m.Scope, kindAlarm)) {
			a, found, readErr := read[alarm](tx, key)
			if readErr != nil || !found {
				return notFound(*in.ResourceARN, readErr)
			}

			tags = a.Tags
			return nil
		}

		d, found, readErr := read[dashboard](tx, key)
		if readErr != nil || !found {
			return notFound(*in.ResourceARN, readErr)
		}

		tags = d.Tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceOutput{Tags: []api.Tag{}}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out.Tags = append(out.Tags, api.Tag{Key: new(k), Value: new(tags[k])})
	}

	return out, nil
}
