package transcribe

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || region == "" {
				continue
			}

			kind, name, _ := strings.Cut(rest, "/")
			resource := catalog.Resource{Account: account, Region: region, Type: kind, Name: name}
			if kind != kindVocabulary && kind != kindFilter && kind != kindJob {
				continue
			}

			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			resource.ARN = arn(request.Scope{AccountID: account, Region: region}, kind, name)
			resource.Tags = r.Tags
			resource.Details = map[string]any{
				"RecordsOnly":   kind != kindJob,
				"FixtureBacked": true,
				"Configuration": r,
			}

			out = append(out, resource)
		}

		return nil
	})
	return out, err
}
