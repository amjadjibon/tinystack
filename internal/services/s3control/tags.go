package s3control

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3control"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) pointTags(
	ctx context.Context,
	account, arn *string,
	add []api.Tag,
	remove []string,
) (bool, []api.Tag, error) {
	if !strings.Contains(common.Value(arn), ":accesspoint/") {
		return false, nil, nil
	}

	scope, err := scopeFor(ctx, account)
	if err != nil {
		return true, nil, err
	}

	prefix := fmt.Sprintf("arn:aws:s3:%s:%s:accesspoint/", scope.Region, scope.AccountID)
	name, ok := strings.CutPrefix(common.Value(arn), prefix)
	if !ok {
		return true, nil, missingPoint()
	}

	var out []api.Tag
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, txErr := readPoint(tx, scope, new(name))
		if txErr != nil {
			return txErr
		}

		tags := common.CloneTags(r.Tags)
		if tags == nil {
			tags = map[string]string{}
		}

		seen := map[string]bool{}
		for _, t := range add {
			key := common.Value(t.Key)
			if seen[key] {
				return awserror.New("InvalidTag", "Duplicate tag key")
			}

			seen[key] = true
			tags[key] = common.Value(t.Value)
		}

		for _, key := range remove {
			if tagging.Reserved(key) {
				return awserror.New("InvalidTag", "System tags cannot be removed")
			}

			delete(tags, key)
		}

		if txErr := tagging.CheckSet(tags); txErr != nil {
			return awserror.New("InvalidTag", txErr.Error())
		}

		keys := make([]string, 0, len(tags))
		for k := range tags {
			keys = append(keys, k)
		}

		slices.Sort(keys)
		for _, k := range keys {
			out = append(out, api.Tag{Key: new(k), Value: new(tags[k])})
		}

		r.Tags = tags
		return storage.Write(tx, scope.Key(serviceName, "accesspoint", name), r)
	})
	return true, out, err
}
