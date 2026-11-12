package memorydb

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the stored resources. The built-in default user and open-access ACL are not stored.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			scope := request.Scope{AccountID: account, Region: region}
			var (
				name           string
				configuration  any
				tags           map[string]string
				executionReady bool
			)

			switch kind {
			case kindParameterGroup:
				r, _, e := storage.Read[parameterGroupRecord](tx, k)
				if e != nil {
					return e
				}

				name, tags, configuration = r.Name, r.Tags, r.output(scope)
			case kindCluster:
				r, _, e := storage.Read[clusterRecord](tx, k)
				if e != nil {
					return e
				}

				az, e := zones(tx, scope, r)
				if e != nil {
					return e
				}

				name, tags, configuration = r.Name, r.Tags, r.output(scope, az, true, statusAvailable)
				executionReady = s.runtime != nil && r.RuntimeStatus == statusAvailable
			case kindUser:
				r, _, e := storage.Read[userRecord](tx, k)
				if e != nil {
					return e
				}

				acls, e := aclNames(tx, scope, r.Name)
				if e != nil {
					return e
				}

				name, tags, configuration = r.Name, r.Tags, r.output(scope, acls)
			case kindACL:
				r, _, e := storage.Read[aclRecord](tx, k)
				if e != nil {
					return e
				}

				clusters, e := clustersOf(tx, scope, r.Name)
				if e != nil {
					return e
				}

				name, tags, configuration = r.Name, r.Tags, r.output(scope, clusters)
			case kindSubnetGroup:
				r, _, e := storage.Read[subnetGroupRecord](tx, k)
				if e != nil {
					return e
				}

				name, tags, configuration = r.Name, r.Tags, r.output(scope)
			default:
				continue
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: name,
				ARN:  arn(scope, kind, name),
				Tags: maps.Clone(tags),
				Details: map[string]any{
					"RecordsOnly":    s.runtime == nil || kind != kindCluster,
					"ExecutionReady": executionReady,
					"Configuration":  configuration,
				},
			})
		}

		return nil
	})
	return out, err
}
