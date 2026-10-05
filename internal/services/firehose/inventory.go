package firehose

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every delivery stream with its buffered record count, for
// the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindStream+"/") {
				continue
			}

			st, _, err := storage.Read[deliveryStream](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			out = append(out, catalog.Resource{
				Type: "delivery-stream", Account: account, Region: region, Name: st.Name,
				ARN: streamARN(scope, st.Name), Tags: st.Tags,
				Details: map[string]any{
					"Type": st.Type, "Bucket": common.Value(st.Destination.BucketARN),
					"Buffered": len(tx.Keys(bufferPrefix(scope, st.Name))),
				},
			})
		}

		return nil
	})

	return out, err
}
