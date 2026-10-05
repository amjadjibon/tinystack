package secretsmanager

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every secret's metadata, for the resource browser. Values
// are never read.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	now := s.now()
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, "secretsmanager")
			if !ok || !strings.HasPrefix(rest, "secret/") {
				continue
			}

			sec, _, err := storage.Read[secret](tx, key)
			if err != nil {
				return err
			}

			if expired(sec, now) {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			details := map[string]any{"Description": sec.Description, "VersionIdsToStages": sec.stagesOf()}
			if sec.scheduled() {
				details["DeletionDate"] = *seconds(sec.DeletionAt)
			}

			out = append(out, catalog.Resource{
				Type: "secret", Account: account, Region: region, Name: sec.Name, ARN: sec.arn(scope),
				Tags: sec.Tags, Details: details,
			})
		}

		return nil
	})

	return out, err
}
