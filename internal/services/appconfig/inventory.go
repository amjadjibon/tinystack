package appconfig

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind := ""
			for _, v := range []string{
				kindApplication, kindEnvironment, kindProfile, kindStrategy, kindVersion, kindDeployment,
			} {
				if len(rest) > len(v) && rest[:len(v)+1] == v+"/" {
					kind = v
					break
				}
			}

			if kind == "" {
				continue
			}

			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			typ := map[string]string{
				kindApplication: kindApplication, kindEnvironment: kindEnvironment, kindProfile: "configuration-profile",
				kindStrategy: "deployment-strategy", kindVersion: "hosted-configuration-version", kindDeployment: kindDeployment,
			}[kind]
			out = append(out, catalog.Resource{
				Type: typ, Name: r.Name, Account: account, Region: region,
				ARN: resourceARN(request.Scope{AccountID: account, Region: region}, r), Tags: r.Tags,
				Details: map[string]any{"Id": r.ID, "ApplicationId": r.ApplicationID},
			})
		}

		return nil
	})
	return out, err
}
