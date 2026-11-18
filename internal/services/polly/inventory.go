package polly

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/polly"
	"github.com/amjadjibon/tinystack/internal/common"
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

			kind, id, _ := strings.Cut(rest, "/")
			item := catalog.Resource{Account: account, Region: region, Type: kind, Name: id}
			switch kind {
			case kindLexicon:
				row, _, e := storage.Read[lexiconRecord](tx, key)
				if e != nil {
					return e
				}

				item.ARN = common.Value(row.Attributes.LexiconArn)
				item.Details = map[string]any{"Attributes": row.Attributes}
			case kindTask:
				row, _, e := storage.Read[api.SynthesisTask](tx, key)
				if e != nil {
					return e
				}

				item.Details = map[string]any{"Status": common.Value(row.TaskStatus)}
			default:
				continue
			}

			out = append(out, item)
		}

		return nil
	})
	return out, err
}
