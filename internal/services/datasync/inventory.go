package datasync

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources exposes locations, tasks and executions to the shared inventory.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			r := catalog.Resource{Account: account, Region: region, Type: kind}
			switch kind {
			case kindLocation:
				rec, _, e := storage.Read[locationRecord](tx, key)
				if e != nil {
					return e
				}

				r.ARN = rec.ARN
				r.Name = rec.URI
				r.Type = kindLocation + strings.ToLower(rec.Kind)
				r.Tags = maps.Clone(rec.Tags)
				r.Details = map[string]any{
					detailConfiguration:  rec,
					detailRecordsOnly:    true,
					detailExecutionReady: false,
				}
			case kindTask:
				rec, _, e := storage.Read[taskRecord](tx, key)
				if e != nil {
					return e
				}

				scope := request.Scope{AccountID: account, Region: region}
				source, sourceErr := readRecord[locationRecord](
					tx,
					scope,
					kindLocation,
					common.Value(rec.SourceLocationArn),
				)
				if sourceErr != nil {
					return sourceErr
				}

				destination, destinationErr := readRecord[locationRecord](
					tx,
					scope,
					kindLocation,
					common.Value(rec.DestinationLocationArn),
				)
				if destinationErr != nil {
					return destinationErr
				}

				ready := source.Kind == locationS3 && destination.Kind == locationS3
				r.ARN = common.Value(rec.TaskArn)
				r.Name = common.Value(rec.Name)
				r.Tags = maps.Clone(rec.Tags)
				r.Details = map[string]any{
					detailConfiguration:  rec,
					detailRecordsOnly:    !ready,
					detailExecutionReady: ready,
				}
			case kindExecution:
				rec, _, e := storage.Read[executionRecord](tx, key)
				if e != nil {
					return e
				}

				r.ARN = common.Value(rec.TaskExecutionArn)
				r.Name = r.ARN
				r.Tags = maps.Clone(rec.Tags)
				r.Details = map[string]any{
					detailConfiguration:  rec,
					detailRecordsOnly:    false,
					detailExecutionReady: true,
				}
			default:
				continue
			}

			out = append(out, r)
		}

		return nil
	})
	return out, err
}
