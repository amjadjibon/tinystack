package efs

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/efs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateAccessPoint(
	ctx context.Context,
	in *api.CreateAccessPointRequest,
) (*api.AccessPointDescription, error) {
	if err := accessOK(in); err != nil {
		return nil, err
	}
	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	id := "fsap-" + common.RandomID(17, "0123456789abcdef")
	r := &record{ID: id, Kind: kindAP, ARN: arn(scope, kindAP, id), Tags: tags}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		parent, e := read(tx, scope, in.FileSystemId, kindFS)
		if e != nil {
			return e
		}
		all, e := rows(tx, scope, kindAP)
		if e != nil {
			return e
		}
		count := 0
		for _, old := range all {
			if common.Value(old.AP.ClientToken) == *in.ClientToken {
				// The pinned API models AccessPointAlreadyExists (not a fabricated
				// idempotent success). Conflicting retries cannot mutate the first record.
				e := awserror.From(
					failure("AccessPointAlreadyExists", "ClientToken already identifies an access point"),
				)
				e.Fields["AccessPointId"] = old.ID
				return e
			}
			if old.Parent == parent.ID {
				count++
			}
		}
		if count >= 10000 {
			return failure("AccessPointLimitExceeded", "At most 10000 access points per file system")
		}
		r.Parent = parent.ID
		root := common.Clone(in.RootDirectory)
		if root == nil {
			root = &api.RootDirectory{}
		}
		if root.Path == nil {
			root.Path = new("/")
		}
		r.AP = &api.AccessPointDescription{
			AccessPointId: new(id), AccessPointArn: new(r.ARN), ClientToken: in.ClientToken,
			FileSystemId: new(parent.ID), OwnerId: new(scope.AccountID), LifeCycleState: new(available),
			PosixUser: common.Clone(in.PosixUser), RootDirectory: root,
		}
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}
	return apOutput(r), nil
}

func (s *Service) DescribeAccessPoints(
	ctx context.Context,
	in *api.DescribeAccessPointsRequest,
) (*api.DescribeAccessPointsResponse, error) {
	if in.AccessPointId != nil && in.FileSystemId != nil {
		return nil, invalid("Specify AccessPointId or FileSystemId, not both")
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	out := &api.DescribeAccessPointsResponse{AccessPoints: []api.AccessPointDescription{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := rows(tx, scope, kindAP)
		if e != nil {
			return e
		}
		if in.AccessPointId != nil {
			r, readErr := read(tx, scope, in.AccessPointId, kindAP)
			if readErr != nil {
				return readErr
			}
			all = []*record{r}
		} else if in.FileSystemId != nil {
			r, readErr := read(tx, scope, in.FileSystemId, kindFS)
			if readErr != nil {
				return readErr
			}
			all = slices.DeleteFunc(all, func(p *record) bool { return p.Parent != r.ID })
		}
		all, out.NextToken, e = page(all, func(r *record) string { return r.ID }, in.NextToken, in.MaxResults, scope,
			"points/"+common.Value(in.AccessPointId)+"/"+common.Value(in.FileSystemId))
		if e != nil {
			return e
		}
		for _, r := range all {
			out.AccessPoints = append(out.AccessPoints, *apOutput(r))
		}
		return nil
	})
	return out, err
}
