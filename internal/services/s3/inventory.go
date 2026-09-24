package s3

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// bucketView summarizes a bucket for the resource browser.
type bucketView struct {
	CreationDate time.Time
	Objects      int64
	Size         int64
}

// Resources lists every bucket with its object count and total size, for the
// resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		// Bucket records are "<scope>/s3/<bucket>"; object metadata is
		// "<scope>/s3/<bucket>/o/<key>".
		views := map[string]*bucketView{}
		view := func(bucketKey string) *bucketView {
			if views[bucketKey] == nil {
				views[bucketKey] = &bucketView{}
			}

			return views[bucketKey]
		}

		for _, key := range tx.Keys() {
			account, region, rest, ok := storage.Scoped(key, "s3")
			if !ok {
				continue
			}

			bucket, suffix, isObject := strings.Cut(rest, "/")
			if isObject {
				if !strings.HasPrefix(suffix, "o/") {
					continue
				}

				var obj object
				if _, err := readJSON(tx, key, &obj); err != nil {
					return err
				}

				v := view(strings.TrimSuffix(key, "/"+suffix))
				v.Objects++
				v.Size += obj.Size
				continue
			}

			var meta bucketMeta
			if _, err := readJSON(tx, key, &meta); err != nil {
				return err
			}

			view(key).CreationDate = time.UnixMilli(meta.CreatedAt).UTC()
			out = append(out, catalog.Resource{
				Type:    "bucket",
				Account: account,
				Region:  region,
				Name:    bucket,
				ARN:     "arn:aws:s3:::" + bucket,
				Tags:    meta.Tags,
				Details: views[key],
			})
		}

		return nil
	})

	return out, err
}
