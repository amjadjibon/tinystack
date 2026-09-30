package acm

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every certificate, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, service)
			if !ok || !strings.HasPrefix(rest, kindCert+"/") {
				continue
			}

			c, _, err := read[certificate](tx, k)
			if err != nil {
				return err
			}

			d := &c.Detail
			out = append(out, catalog.Resource{
				Type: "certificate", Account: account, Region: region, Name: value(d.DomainName),
				ARN: value(d.CertificateArn), Tags: c.Tags,
				Details: map[string]any{
					"Type": value(d.Type), "Status": value(d.Status), "KeyAlgorithm": value(d.KeyAlgorithm),
					"Names": d.SubjectAlternativeNames,
				},
			})
		}

		return nil
	})

	return out, err
}
