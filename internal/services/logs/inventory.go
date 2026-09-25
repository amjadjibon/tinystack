package logs

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every log group with its streams, for the resource browser.
// Event messages are left out.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, "logs")
			name, isGroup := strings.CutPrefix(rest, "group/")
			if !ok || !isGroup {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			g, _, err := read[group](tx, key)
			if err != nil {
				return err
			}

			var streams []string
			for _, streamRecord := range tx.Keys(streamPrefix(scope, name)) {
				streams = append(streams, streamRecord[len(streamPrefix(scope, name)):])
			}

			out = append(out, catalog.Resource{
				Type: "log-group", Account: account, Region: region, Name: name, ARN: groupARN(scope, name),
				Tags: g.Tags, Details: map[string]any{"LogGroup": g.describe(scope), "Streams": streams},
			})
		}

		return nil
	})

	return out, err
}
