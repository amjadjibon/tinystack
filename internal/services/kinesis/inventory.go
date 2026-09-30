package kinesis

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every stream, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindStream+"/") {
				continue
			}

			st, _, err := read[stream](tx, key)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Type: kindStream, Account: account, Region: region, Name: st.Name,
				ARN: StreamARN(request.Scope{AccountID: account, Region: region}, st.Name), Tags: st.Tags,
				Details: map[string]any{
					"Mode": st.Mode, "OpenShards": len(st.open()), "RetentionHours": st.Retention,
				},
			})
		}

		return nil
	})

	return out, err
}
