package athena

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every workgroup that was created or changed, for the
// resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindWorkGroup+"/") {
				continue
			}

			w, _, err := storage.Read[workGroup](tx, key)
			if err != nil {
				return err
			}

			name := common.Value(w.WorkGroup.Name)
			out = append(out, catalog.Resource{
				Type: "workgroup", Account: account, Region: region, Name: name, Tags: w.Tags,
				ARN:     "arn:aws:athena:" + region + ":" + account + ":workgroup/" + name,
				Details: map[string]any{"State": common.Value(w.WorkGroup.State)},
			})
		}

		return nil
	})

	return out, err
}
