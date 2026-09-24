package lambda

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every function and event source mapping with its
// configuration, for the resource browser. Function code is not listed.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys() {
			account, region, rest, ok := storage.Scoped(key, "lambda")
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			res := catalog.Resource{Account: account, Region: region}
			kind, name, _ := strings.Cut(rest, "/")
			switch kind {
			case kindFunction:
				fn, _, err := loadFunction(tx, key)
				if err != nil {
					return err
				}

				res.Type, res.Name, res.ARN = kindFunction, name, functionARN(scope, name)
				res.Tags, res.Details = fn.Tags, fn.configuration(scope)
			case kindMapping:
				mp, _, err := loadMapping(tx, key)
				if err != nil {
					return err
				}

				res.Type, res.Name = "event-source-mapping", name
				res.ARN = "arn:aws:lambda:" + region + ":" + account + ":event-source-mapping:" + name
				res.Details = mp.configuration(scope)
			default:
				continue
			}

			out = append(out, res)
		}

		return nil
	})

	return out, err
}
