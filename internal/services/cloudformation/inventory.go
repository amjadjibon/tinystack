package cloudformation

import (
	"context"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every stack that is not deleted, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, service)
			if !ok || !strings.HasPrefix(rest, kindStack+"/") {
				continue
			}

			st, _, err := storage.Read[stack](tx, k)
			if err != nil {
				return err
			}

			tags := map[string]string{}
			for _, t := range st.Tags {
				tags[t.Key] = t.Value
			}

			out = append(out, catalog.Resource{
				Type: "stack", Account: account, Region: region, Name: st.Name, ARN: st.ID, Tags: tags,
				Details: map[string]any{memberStatus: st.Status, keyResources: strconv.Itoa(len(st.Resources))},
			})
		}

		return nil
	})

	return out, err
}
