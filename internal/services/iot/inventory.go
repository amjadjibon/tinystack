package iot

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every thing, thing type, thing group, certificate, CA
// certificate, and policy, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			r, ok, err := resource(tx, scope, key, rest)
			if err != nil {
				return err
			}

			if ok {
				r.Account, r.Region = account, region
				out = append(out, r)
			}
		}

		return nil
	})

	return out, err
}

// resource describes the record at key, whose path after the service is rest,
// when it is one the browser lists.
func resource(tx storage.ReadTx, scope request.Scope, key, rest string) (catalog.Resource, bool, error) {
	kind, name, _ := strings.Cut(rest, "/")
	switch kind {
	case kindThing:
		t, _, err := storage.Read[thing](tx, key)
		if err != nil {
			return catalog.Resource{}, false, err
		}

		return catalog.Resource{
			Type: kind, Name: name, ARN: arn(scope, kindThing, name),
			Details: map[string]any{"ThingType": t.Type, "Version": t.Version, "Principals": len(t.Principals)},
		}, true, nil
	case kindThingType, kindThingGroup, kindPolicy:
		r, _, err := storage.Read[struct{ Tags map[string]string }](tx, key)
		if err != nil {
			return catalog.Resource{}, false, err
		}

		arnKind := map[string]string{kindThingType: "thingtype", kindThingGroup: "thinggroup", kindPolicy: kindPolicy}
		return catalog.Resource{Type: kind, Name: name, ARN: arn(scope, arnKind[kind], name), Tags: r.Tags}, true, nil
	case kindCertificate:
		c, _, err := storage.Read[certificate](tx, key)
		if err != nil {
			return catalog.Resource{}, false, err
		}

		return catalog.Resource{
			Type: kind, Name: name, ARN: arn(scope, "cert", name),
			Details: map[string]any{"Status": c.Status, "Mode": c.Mode},
		}, true, nil
	case kindCA:
		c, _, err := storage.Read[caCertificate](tx, key)
		if err != nil {
			return catalog.Resource{}, false, err
		}

		return catalog.Resource{
			Type: kind, Name: name, ARN: arn(scope, "cacert", name), Tags: c.Tags,
			Details: map[string]any{"Status": c.Status, "Mode": c.Mode},
		}, true, nil
	}

	return catalog.Resource{}, false, nil
}
