package rekognition

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
			kind, id, _ := strings.Cut(rest, "/")
			if !ok || region == "" || kind != kindCollection {
				continue
			}

			row, e := readCollection(tx, key)
			if e != nil {
				return e
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: id, Tags: row.Tags,
				ARN: collectionARN(request.Scope{AccountID: account, Region: region}, id),
			})
		}

		return nil
	})
	return out, err
}
