package kms

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every key with its aliases, for the resource browser. Key
// material is left out.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, record := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(record, "kms")
			if _, isKey := strings.CutPrefix(rest, "key/"); !ok || !isKey {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			k, _, err := read[key](tx, record)
			if err != nil {
				return err
			}

			if k.deleted(s.now()) {
				continue
			}

			var aliases []string
			for _, aliasRecord := range tx.Keys(prefix(scope, "alias")) {
				a, _, readErr := read[alias](tx, aliasRecord)
				if readErr != nil {
					return readErr
				}

				if a.Target == k.ID {
					aliases = append(aliases, "alias/"+a.Name)
				}
			}

			out = append(out, catalog.Resource{
				Type: "key", Account: account, Region: region, Name: k.ID, ARN: keyARN(scope, k.ID),
				Tags: k.Tags, Details: map[string]any{"KeyMetadata": k.metadata(scope), "Aliases": aliases},
			})
		}

		return nil
	})

	return out, err
}
