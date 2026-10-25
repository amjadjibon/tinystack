package redshift

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshift"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// LookupCluster resolves a provisioned cluster for the Data API in this account and Region.
func (s *Service) LookupCluster(ctx context.Context, name string) (*api.Cluster, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var e error; r, e = read(tx, scope, clusterKind, name); return e },
	)
	if err != nil {
		return nil, err
	}

	return r.Cluster, nil
}
