package s3files

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists file systems, mount targets and access points. Mount targets carry no tags.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			switch {
			case strings.HasPrefix(rest, kindFS+"/"):
				r, _, err := storage.Read[fileSystem](tx, k)
				if err != nil {
					return err
				}

				out = append(out, resource(scope, kindFS, r.ID, fsARN(scope, r.ID), r.Tags, fsResponse(scope, r)))
			case strings.HasPrefix(rest, kindMT+"/"):
				r, _, err := storage.Read[mountTarget](tx, k)
				if err != nil {
					return err
				}

				out = append(out, resource(scope, kindMT, r.ID, "", nil, mtResponse(scope, r)))
			case strings.HasPrefix(rest, kindAP+"/"):
				r, _, err := storage.Read[accessPoint](tx, k)
				if err != nil {
					return err
				}

				out = append(out, resource(scope, kindAP, r.ID, apARN(scope, r.FileSystemID, r.ID), r.Tags,
					apResponse(scope, r)))
			}
		}

		return nil
	})

	return out, err
}

func resource(scope request.Scope, kind, id, arn string, tags map[string]string, config any) catalog.Resource {
	return catalog.Resource{
		Account: scope.AccountID,
		Region:  scope.Region,
		Type:    kind,
		Name:    id,
		ARN:     arn,
		Tags:    maps.Clone(tags),
		Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": config},
	}
}
