package s3files

import (
	"context"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3files"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func apResponse(scope request.Scope, r *accessPoint) *api.GetAccessPointResponse {
	return &api.GetAccessPointResponse{
		AccessPointArn: new(apARN(scope, r.FileSystemID, r.ID)),
		AccessPointId:  new(r.ID),
		ClientToken:    new(r.ClientToken),
		FileSystemId:   new(r.FileSystemID),
		Status:         new(r.State),
		OwnerId:        new(scope.AccountID),
		PosixUser:      r.PosixUser,
		RootDirectory:  r.RootDirectory,
		Tags:           tagList(r.Tags),
		Name:           nameTag(r.Tags),
	}
}

func (s *Service) CreateAccessPoint(
	ctx context.Context,
	in *api.CreateAccessPointRequest,
) (*api.CreateAccessPointResponse, error) {
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	token := newToken(in.ClientToken)

	var out *api.GetAccessPointResponse
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		existing, e := storage.Records[accessPoint](tx, scope.Prefix(serviceName, kindAP))
		if e != nil {
			return e
		}

		for _, r := range existing {
			if r.ClientToken != token {
				continue
			}

			out = apResponse(scope, r)
			return nil
		}

		fs, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		if fs.State == deleting {
			return conflict("File system %s is being deleted", fs.ID)
		}

		r := &accessPoint{
			ID:            "fsap-" + common.RandomID(17, "0123456789abcdef"),
			FileSystemID:  fs.ID,
			ClientToken:   token,
			PosixUser:     in.PosixUser,
			RootDirectory: in.RootDirectory,
			Tags:          tags,
			State:         creating,
			Due:           time.Now().UTC().Add(transitionDelay),
		}

		out = apResponse(scope, r)

		return queue(tx, scope, kindAP, r.ID, r)
	})
	if err != nil {
		return nil, err
	}

	return new(api.CreateAccessPointResponse(*out)), nil
}

func (s *Service) GetAccessPoint(
	ctx context.Context,
	in *api.GetAccessPointRequest,
) (*api.GetAccessPointResponse, error) {
	var out *api.GetAccessPointResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, e := readAP(tx, scope, common.Value(in.AccessPointId))
		if e != nil {
			return e
		}

		out = apResponse(scope, r)
		return nil
	})

	return out, err
}

func (s *Service) ListAccessPoints(
	ctx context.Context,
	in *api.ListAccessPointsRequest,
) (*api.ListAccessPointsResponse, error) {
	out := &api.ListAccessPointsResponse{AccessPoints: []api.ListAccessPointsDescription{}}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		fs, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		all, e := storage.Records[accessPoint](tx, scope.Prefix(serviceName, kindAP))
		if e != nil {
			return e
		}

		all = slices.DeleteFunc(all, func(r *accessPoint) bool { return r.FileSystemID != fs.ID })
		all, out.NextToken, e = common.Page(all, func(r *accessPoint) string { return r.ID }, in.NextToken,
			pageLimit(in.MaxResults, listPageSize), errBadToken)
		if e != nil {
			return e
		}

		for _, r := range all {
			out.AccessPoints = append(out.AccessPoints, api.ListAccessPointsDescription{
				AccessPointArn: new(apARN(scope, r.FileSystemID, r.ID)),
				AccessPointId:  new(r.ID),
				FileSystemId:   new(r.FileSystemID),
				Status:         new(r.State),
				OwnerId:        new(scope.AccountID),
				PosixUser:      r.PosixUser,
				RootDirectory:  r.RootDirectory,
				Name:           nameTag(r.Tags),
			})
		}

		return nil
	})

	return out, err
}

func (s *Service) DeleteAccessPoint(
	ctx context.Context,
	in *api.DeleteAccessPointRequest,
) (*api.DeleteAccessPointOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readAP(tx, scope, common.Value(in.AccessPointId))
		if e != nil {
			return e
		}

		if r.State == deleting {
			return nil
		}

		r.State = deleting
		r.Due = time.Now().UTC().Add(transitionDelay)

		return queue(tx, scope, kindAP, r.ID, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAccessPointOutput{}, nil
}
