package efs

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, _, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}
			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}
			details := map[string]any{"RecordsOnly": true, "Mountable": false}
			if r.FS != nil {
				n, e := mountCount(tx, request.Scope{AccountID: account, Region: region}, r.ID)
				if e != nil {
					return e
				}
				details["FileSystem"] = fsOutput(r, n)
			}
			if r.AP != nil {
				details["AccessPoint"] = apOutput(r)
			}
			if r.MT != nil {
				details["MountTarget"] = r.MT
				details["SecurityGroups"] = r.Groups
			}
			name := r.ID
			if tagName(r.Tags) != nil {
				name = *tagName(r.Tags)
			}
			out = append(
				out,
				catalog.Resource{
					Type:    r.Kind,
					Name:    name,
					ARN:     r.ARN,
					Account: account,
					Region:  region,
					Tags:    r.Tags,
					Details: details,
				},
			)
		}
		return nil
	})
	return out, err
}
