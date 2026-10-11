package efs

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/efs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateFileSystem(
	ctx context.Context,
	in *api.CreateFileSystemRequest,
) (*api.FileSystemDescription, error) {
	if err := tokenOK(in.CreationToken); err != nil {
		return nil, err
	}
	performance := common.Value(common.OrElse(in.PerformanceMode, new(generalPurpose)))
	mode := common.Value(common.OrElse(in.ThroughputMode, new(bursting)))
	if err := throughputOK(performance, mode, in.ProvisionedThroughputInMibps); err != nil {
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
	if guardErr := kmsOK(scope, in.KmsKeyId, common.Value(in.Encrypted)); guardErr != nil {
		return nil, guardErr
	}
	id := "fs-" + common.RandomID(17, "0123456789abcdef")
	created := common.Unix(time.Now().UTC())
	r := &record{ID: id, ARN: arn(scope, kindFS, id), Kind: kindFS, Tags: tags}
	r.FS = &api.FileSystemDescription{
		OwnerId: new(scope.AccountID), CreationToken: in.CreationToken,
		FileSystemId: new(id), FileSystemArn: new(r.ARN), CreationTime: new(created), LifeCycleState: new(available),
		PerformanceMode: new(performance), ThroughputMode: new(mode), Encrypted: new(common.Value(in.Encrypted)),
		KmsKeyId: in.KmsKeyId, ProvisionedThroughputInMibps: in.ProvisionedThroughputInMibps,
		SizeInBytes: &api.FileSystemSize{
			Value: new(int64(0)), Timestamp: new(created), ValueInIA: new(int64(0)),
			ValueInStandard: new(int64(0)), ValueInArchive: new(int64(0)),
		},
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		all, e := rows(tx, scope, kindFS)
		if e != nil {
			return e
		}
		for _, old := range all {
			if common.Value(old.FS.CreationToken) == *in.CreationToken {
				e := failure("FileSystemAlreadyExists", "CreationToken already identifies a file system")
				awse := awserror.From(e)
				awse.Fields["FileSystemId"] = old.ID
				return awse
			}
		}
		if len(all) >= 1000 {
			return failure("FileSystemLimitExceeded", "At most 1000 file systems per account and Region")
		}
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}
	return fsOutput(r, 0), nil
}

func (s *Service) UpdateFileSystem(
	ctx context.Context,
	in *api.UpdateFileSystemRequest,
) (*api.FileSystemDescription, error) {
	if in.ThroughputMode == nil && in.ProvisionedThroughputInMibps == nil {
		return nil, invalid("Specify a throughput update")
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var out *api.FileSystemDescription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, in.FileSystemId, kindFS)
		if readErr != nil {
			return readErr
		}
		mode := common.Value(common.OrElse(in.ThroughputMode, r.FS.ThroughputMode))
		amount := in.ProvisionedThroughputInMibps
		if mode == provisioned && amount == nil && common.Value(r.FS.ThroughputMode) == provisioned {
			amount = r.FS.ProvisionedThroughputInMibps
		}
		if e := throughputOK(common.Value(r.FS.PerformanceMode), mode, amount); e != nil {
			return e
		}
		r.FS.ThroughputMode = new(mode)
		r.FS.ProvisionedThroughputInMibps = amount
		n, e := mountCount(tx, scope, r.ID)
		if e != nil {
			return e
		}
		out = fsOutput(r, n)
		return save(tx, scope, r)
	})
	return out, err
}

func (s *Service) DescribeFileSystems(
	ctx context.Context,
	in *api.DescribeFileSystemsRequest,
) (*api.DescribeFileSystemsResponse, error) {
	if in.FileSystemId != nil && in.CreationToken != nil {
		return nil, invalid("Specify FileSystemId or CreationToken, not both")
	}
	if in.CreationToken != nil {
		if err := tokenOK(in.CreationToken); err != nil {
			return nil, err
		}
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	out := &api.DescribeFileSystemsResponse{Marker: in.Marker, FileSystems: []api.FileSystemDescription{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := rows(tx, scope, kindFS)
		if e != nil {
			return e
		}
		if in.FileSystemId != nil {
			r, readErr := read(tx, scope, in.FileSystemId, kindFS)
			if readErr != nil {
				return readErr
			}
			all = []*record{r}
		} else if in.CreationToken != nil {
			selected := []*record{}
			for _, r := range all {
				if common.Value(r.FS.CreationToken) == *in.CreationToken {
					selected = append(selected, r)
				}
			}
			if len(selected) == 0 {
				return missing(kindFS)
			}
			all = selected
		}
		all, out.NextMarker, e = page(all, func(r *record) string { return r.ID }, in.Marker, in.MaxItems, scope,
			"files/"+common.Value(in.FileSystemId)+"/"+common.Value(in.CreationToken))
		if e != nil {
			return e
		}
		for _, r := range all {
			n, e := mountCount(tx, scope, r.ID)
			if e != nil {
				return e
			}
			out.FileSystems = append(out.FileSystems, *fsOutput(r, n))
		}
		return nil
	})
	return out, err
}
