package kinesisanalyticsv2

import (
	"context"
	"fmt"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesisanalyticsv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func readSnapshot(tx storage.ReadTx, scope request.Scope, app, name *string) (*api.SnapshotDetails, error) {
	if !snapshotNamePattern.MatchString(common.Value(name)) {
		return nil, invalid("Invalid snapshot name.")
	}

	r, ok, err := storage.Read[api.SnapshotDetails](tx, snapshotPrefix(scope, *app)+*name)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

// CreateApplicationSnapshot stores snapshot metadata for a RUNNING application.
func (s *Service) CreateApplicationSnapshot(
	ctx context.Context,
	in *api.CreateApplicationSnapshotRequest,
) (*api.CreateApplicationSnapshotResponse, error) {
	_, err := s.change(ctx, in.ApplicationName, nil, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if *r.Detail.ApplicationStatus != running {
			return inUse()
		}

		if !snapshotNamePattern.MatchString(common.Value(in.SnapshotName)) {
			return invalid("Invalid snapshot name.")
		}

		k := snapshotPrefix(scope, *in.ApplicationName) + *in.SnapshotName
		if _, ok := tx.Get(k); ok {
			return inUse()
		}

		return storage.Write(
			tx,
			k,
			&api.SnapshotDetails{
				SnapshotName:              in.SnapshotName,
				SnapshotStatus:            new("READY"),
				ApplicationVersionId:      r.Detail.ApplicationVersionId,
				SnapshotCreationTimestamp: common.Seconds(time.Now()),
				RuntimeEnvironment:        r.Detail.RuntimeEnvironment,
			},
		)
	})
	return &api.CreateApplicationSnapshotResponse{}, err
}

// DescribeApplicationSnapshot returns snapshot metadata without any job state.
func (s *Service) DescribeApplicationSnapshot(
	ctx context.Context,
	in *api.DescribeApplicationSnapshotRequest,
) (*api.DescribeApplicationSnapshotResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.SnapshotDetails
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, e := read(tx, scope, in.ApplicationName); e != nil {
			return e
		}

		var e error
		out, e = readSnapshot(tx, scope, in.ApplicationName, in.SnapshotName)
		return e
	})
	return &api.DescribeApplicationSnapshotResponse{SnapshotDetails: out}, err
}

// DeleteApplicationSnapshot deletes metadata after checking the snapshot creation timestamp.
func (s *Service) DeleteApplicationSnapshot(
	ctx context.Context,
	in *api.DeleteApplicationSnapshotRequest,
) (*api.DeleteApplicationSnapshotResponse, error) {
	_, err := s.change(ctx, in.ApplicationName, nil, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		snap, e := readSnapshot(tx, scope, in.ApplicationName, in.SnapshotName)
		if e != nil {
			return e
		}

		if in.SnapshotCreationTimestamp == nil || *in.SnapshotCreationTimestamp != *snap.SnapshotCreationTimestamp {
			return invalid("SnapshotCreationTimestamp does not match.")
		}

		tx.Delete(snapshotPrefix(scope, *in.ApplicationName) + *in.SnapshotName)
		return nil
	})
	return &api.DeleteApplicationSnapshotResponse{}, err
}

// ListApplicationSnapshots lists snapshot metadata for an application.
func (s *Service) ListApplicationSnapshots(
	ctx context.Context,
	in *api.ListApplicationSnapshotsRequest,
) (*api.ListApplicationSnapshotsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := listLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	var rows []api.SnapshotDetails
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, e := read(tx, scope, in.ApplicationName); e != nil {
			return e
		}

		var e error
		records, e := storage.Records[api.SnapshotDetails](tx, snapshotPrefix(scope, *in.ApplicationName))
		for _, r := range records {
			rows = append(rows, *r)
		}

		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(a api.SnapshotDetails) string { return *a.SnapshotName },
		in.NextToken,
		limit,
		invalid("Invalid NextToken."),
	)
	return &api.ListApplicationSnapshotsResponse{SnapshotSummaries: page, NextToken: next}, err
}

// DescribeApplicationVersion returns the configuration retained for an application version.
func (s *Service) DescribeApplicationVersion(
	ctx context.Context,
	in *api.DescribeApplicationVersionRequest,
) (*api.DescribeApplicationVersionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var detail *api.ApplicationDetail
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read(tx, scope, in.ApplicationName)
		if e != nil {
			return e
		}

		for i := range r.Versions {
			v := &r.Versions[i]
			if common.Value(v.ApplicationVersionId) == common.Value(in.ApplicationVersionId) {
				detail = common.Clone(v)
				return nil
			}
		}

		return missing()
	})
	return &api.DescribeApplicationVersionResponse{ApplicationVersionDetail: detail}, err
}

// ListApplicationVersions lists retained application versions.
func (s *Service) ListApplicationVersions(
	ctx context.Context,
	in *api.ListApplicationVersionsRequest,
) (*api.ListApplicationVersionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := listLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	var rows []api.ApplicationVersionSummary
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read(tx, scope, in.ApplicationName)
		if e != nil {
			return e
		}

		for i := range r.Versions {
			v := &r.Versions[i]
			rows = append(
				rows,
				api.ApplicationVersionSummary{
					ApplicationVersionId: v.ApplicationVersionId,
					ApplicationStatus:    v.ApplicationStatus,
				},
			)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(a api.ApplicationVersionSummary) string { return fmt.Sprintf("%020d", *a.ApplicationVersionId) },
		in.NextToken,
		limit,
		invalid("Invalid NextToken."),
	)
	return &api.ListApplicationVersionsResponse{ApplicationVersionSummaries: page, NextToken: next}, err
}
