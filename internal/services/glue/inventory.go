package glue

import (
	"context"
	"net/url"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every database and table, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			kind, n, _ := strings.Cut(rest, "/")
			if !ok {
				continue
			}

			arn := "arn:aws:glue:" + region + ":" + account + ":" + kind + "/" + n
			switch kind {
			case kindClassifier:
				decoded, err := url.PathUnescape(n)
				if err != nil {
					return err
				}

				out = append(
					out,
					catalog.Resource{
						Type:    kindClassifier,
						Account: account,
						Region:  region,
						Name:    decoded,
						ARN:     "arn:aws:glue:" + region + ":" + account + ":classifier/" + decoded,
					},
				)
			case kindJob, kindTrigger, kindWorkflow, kindCrawler:
				decoded, decodeErr := url.PathUnescape(n)
				if decodeErr != nil {
					return decodeErr
				}

				n = decoded

				arn = "arn:aws:glue:" + region + ":" + account + ":" + kind + "/" + n
				_, tags, err := loadTagged(tx, request.Scope{AccountID: account, Region: region}, kind, n)
				if err != nil {
					return err
				}

				out = append(
					out,
					catalog.Resource{Type: kind, Account: account, Region: region, Name: n, ARN: arn, Tags: tags},
				)
			case kindDatabase:
				d, _, err := storage.Read[database](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "database", Account: account, Region: region, Name: n, ARN: arn, Tags: d.Tags,
					Details: map[string]any{"LocationUri": common.Value(d.Database.LocationUri)},
				})
			case kindTable:
				t, _, err := storage.Read[api.Table](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "table", Account: account, Region: region, Name: n, ARN: arn,
					Details: map[string]any{
						"Location": common.Value(common.OrElse(t.StorageDescriptor, &api.StorageDescriptor{}).Location),
						"Columns":  len(common.OrElse(t.StorageDescriptor, &api.StorageDescriptor{}).Columns),
					},
				})
			}
		}

		return nil
	})

	return out, err
}
