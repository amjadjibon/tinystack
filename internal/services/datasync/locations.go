package datasync

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func locationPrefix(path *string) (string, error) {
	p := strings.Trim(common.Value(path), "/")
	if p != "" {
		for _, component := range strings.Split(p, "/") {
			if component == "" || component == "." || component == ".." {
				return "", invalid("Subdirectory cannot contain empty or relative path components")
			}
		}
	}

	if p != "" {
		p += "/"
	}

	return p, nil
}

func (s *Service) CreateLocationS3(
	ctx context.Context,
	in *api.CreateLocationS3Request,
) (*api.CreateLocationS3Response, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	bucket := strings.TrimPrefix(common.Value(in.S3BucketArn), "arn:aws:s3:::")
	if _, err = s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: new(bucket)}); err != nil {
		return nil, invalid("S3 bucket %s does not exist: %v", bucket, err)
	}

	if roleErr := s.requireRole(ctx, common.Value(in.S3Config.BucketAccessRoleArn)); roleErr != nil {
		return nil, roleErr
	}

	prefix, err := locationPrefix(in.Subdirectory)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	arn := resourceARN(scope, kindLocation)
	r := locationRecord{
		ARN:          arn,
		URI:          "s3://" + bucket + "/" + prefix,
		Kind:         locationS3,
		Bucket:       bucket,
		Prefix:       prefix,
		Created:      timestamp(),
		Tags:         tags,
		S3Config:     common.Clone(in.S3Config),
		StorageClass: common.OrElse(in.S3StorageClass, new("STANDARD")),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error { return writeRecord(tx, scope, kindLocation, arn, r) })
	return &api.CreateLocationS3Response{LocationArn: new(arn)}, err
}

func (s *Service) DescribeLocationS3(
	ctx context.Context,
	in *api.DescribeLocationS3Request,
) (*api.DescribeLocationS3Response, error) {
	r, err := s.location(ctx, common.Value(in.LocationArn), locationS3)
	if err != nil {
		return nil, err
	}

	return &api.DescribeLocationS3Response{
		LocationArn: new(r.ARN), LocationUri: new(r.URI), S3StorageClass: r.StorageClass,
		S3Config: r.S3Config, CreationTime: new(r.Created),
	}, nil
}

func (s *Service) location(ctx context.Context, arn, kind string) (locationRecord, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return locationRecord{}, err
	}

	var r locationRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readRecord[locationRecord](tx, scope, kindLocation, arn)
		if e != nil {
			return e
		}

		if kind != "" && r.Kind != kind {
			return invalid("Location %s is not a %s location", arn, kind)
		}

		return nil
	})
	return r, err
}

func (s *Service) UpdateLocationS3(
	ctx context.Context,
	in *api.UpdateLocationS3Request,
) (*api.UpdateLocationS3Response, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.S3Config != nil {
		if roleErr := s.requireRole(ctx, common.Value(in.S3Config.BucketAccessRoleArn)); roleErr != nil {
			return nil, roleErr
		}
	}

	prefix, err := locationPrefix(in.Subdirectory)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord[locationRecord](tx, scope, kindLocation, common.Value(in.LocationArn))
		if e != nil {
			return e
		}

		if r.Kind != locationS3 {
			return invalid("Expected S3 location")
		}

		if in.Subdirectory != nil {
			r.Prefix = prefix
			r.URI = "s3://" + r.Bucket + "/" + prefix
		}

		if in.S3Config != nil {
			r.S3Config = common.Clone(in.S3Config)
		}

		if in.S3StorageClass != nil {
			r.StorageClass = in.S3StorageClass
		}

		return writeRecord(tx, scope, kindLocation, r.ARN, r)
	})
	return &api.UpdateLocationS3Response{}, err
}

func (s *Service) validateEfs(ctx context.Context, r *locationRecord) error {
	_, err := s.caller.Call(ctx, "efs", "DescribeFileSystems", map[string]any{"FileSystemId": r.Filesystem})
	if err != nil {
		return invalid("EFS file system does not exist: %v", err)
	}

	if r.Efs.FileSystemAccessRoleArn != nil {
		if roleErr := s.requireRole(ctx, *r.Efs.FileSystemAccessRoleArn); roleErr != nil {
			return roleErr
		}
	}

	if r.Efs.AccessPointArn != nil || r.Efs.FileSystemAccessRoleArn != nil {
		if common.Value(r.Efs.InTransitEncryption) != "TLS1_2" {
			return invalid("Access points and IAM roles require TLS1_2")
		}
	}

	if r.Efs.AccessPointArn != nil {
		scope, scopeErr := request.RegionalScope(ctx)
		if scopeErr != nil {
			return scopeErr
		}

		prefix := "arn:aws:elasticfilesystem:" + scope.Region + ":" + scope.AccountID + ":access-point/"
		id, local := strings.CutPrefix(*r.Efs.AccessPointArn, prefix)
		if !local {
			return invalid("EFS access point must be in this account and Region")
		}

		out, e := s.caller.Call(ctx, "efs", "DescribeAccessPoints", map[string]any{"AccessPointId": id})
		if e != nil {
			return invalid("EFS access point does not exist: %v", e)
		}

		items, _ := out["AccessPoints"].([]any)
		if len(items) != 1 {
			return invalid("EFS access point does not exist")
		}

		ap, _ := items[0].(map[string]any)
		if ap["FileSystemId"] != r.Filesystem {
			return invalid("Access point belongs to another file system")
		}
	}

	return nil
}

func (s *Service) CreateLocationEfs(
	ctx context.Context,
	in *api.CreateLocationEfsRequest,
) (*api.CreateLocationEfsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	fs := common.Value(in.EfsFilesystemArn)
	expected := "arn:aws:elasticfilesystem:" + scope.Region + ":" + scope.AccountID + ":file-system/"
	id, ok := strings.CutPrefix(fs, expected)
	if !ok {
		return nil, invalid("EFS file system must be in this account and Region")
	}

	prefix, err := locationPrefix(in.Subdirectory)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	arn := resourceARN(scope, kindLocation)
	r := locationRecord{
		ARN:        arn,
		Kind:       locationEfs,
		URI:        "efs://" + scope.Region + "." + id + "/" + prefix,
		Filesystem: id,
		Prefix:     prefix,
		Created:    timestamp(),
		Tags:       tags,
		Efs: api.DescribeLocationEfsResponse{
			Ec2Config:               common.Clone(in.Ec2Config),
			AccessPointArn:          in.AccessPointArn,
			FileSystemAccessRoleArn: in.FileSystemAccessRoleArn,
			InTransitEncryption:     common.OrElse(in.InTransitEncryption, new(optionNone)),
		},
	}

	if referenceErr := s.validateEfs(ctx, new(r)); referenceErr != nil {
		return nil, referenceErr
	}

	subnet := common.Value(in.Ec2Config.SubnetArn)
	if !strings.HasPrefix(subnet, "arn:aws:ec2:"+scope.Region+":"+scope.AccountID+":subnet/") {
		return nil, invalid("Subnet must be local")
	}

	if _, err = s.caller.Call(
		ctx,
		"ec2",
		"DescribeSubnets",
		map[string]any{"SubnetIds": []any{subnet[strings.LastIndex(subnet, "/")+1:]}},
	); err != nil {
		return nil, invalid("Subnet does not exist: %v", err)
	}

	for _, sg := range in.Ec2Config.SecurityGroupArns {
		if !strings.HasPrefix(sg, "arn:aws:ec2:"+scope.Region+":"+scope.AccountID+":security-group/") {
			return nil, invalid("Security group must be local")
		}

		if _, err = s.caller.Call(
			ctx,
			"ec2",
			"DescribeSecurityGroups",
			map[string]any{"GroupIds": []any{sg[strings.LastIndex(sg, "/")+1:]}},
		); err != nil {
			return nil, invalid("Security group does not exist: %v", err)
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error { return writeRecord(tx, scope, kindLocation, arn, r) })
	return &api.CreateLocationEfsResponse{LocationArn: new(arn)}, err
}

func (s *Service) DescribeLocationEfs(
	ctx context.Context,
	in *api.DescribeLocationEfsRequest,
) (*api.DescribeLocationEfsResponse, error) {
	r, err := s.location(ctx, common.Value(in.LocationArn), locationEfs)
	if err != nil {
		return nil, err
	}

	out := r.Efs
	out.LocationArn = new(r.ARN)
	out.LocationUri = new(r.URI)
	out.CreationTime = new(r.Created)
	return new(out), nil
}

func (s *Service) updateLocationEfs(
	ctx context.Context,
	in *api.UpdateLocationEfsRequest,
	replace bool,
) (*api.UpdateLocationEfsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.location(ctx, common.Value(in.LocationArn), locationEfs)
	if err != nil {
		return nil, err
	}

	if replace {
		r.Efs.AccessPointArn = nil
		r.Efs.FileSystemAccessRoleArn = nil
		r.Efs.InTransitEncryption = new(optionNone)
	}

	if in.Subdirectory != nil {
		r.Prefix, err = locationPrefix(in.Subdirectory)
		if err != nil {
			return nil, err
		}

		r.URI = "efs://" + scope.Region + "." + r.Filesystem + "/" + r.Prefix
	}

	if in.AccessPointArn != nil {
		r.Efs.AccessPointArn = in.AccessPointArn
	}

	if in.FileSystemAccessRoleArn != nil {
		r.Efs.FileSystemAccessRoleArn = in.FileSystemAccessRoleArn
	}

	if in.InTransitEncryption != nil {
		r.Efs.InTransitEncryption = in.InTransitEncryption
	}

	if referenceErr := s.validateEfs(ctx, new(r)); referenceErr != nil {
		return nil, referenceErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, e := readRecord[locationRecord](tx, scope, kindLocation, r.ARN)
		if e != nil {
			return e
		}

		if current.Version != r.Version {
			return invalid("Location was concurrently modified; retry the update")
		}

		r.Version++
		r.Tags = current.Tags
		return writeRecord(tx, scope, kindLocation, r.ARN, r)
	})
	return &api.UpdateLocationEfsResponse{}, err
}

func (s *Service) DeleteLocation(
	ctx context.Context,
	in *api.DeleteLocationRequest,
) (*api.DeleteLocationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.LocationArn)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := readRecord[locationRecord](tx, scope, kindLocation, arn); e != nil {
			return e
		}

		tasks, e := storage.Records[taskRecord](tx, scope.Prefix(serviceName, kindTask))
		if e != nil {
			return e
		}

		for _, t := range tasks {
			if common.Value(t.SourceLocationArn) == arn || common.Value(t.DestinationLocationArn) == arn {
				return invalid("Location is used by a task")
			}
		}

		key, e := recordKey(scope, kindLocation, arn)
		if e != nil {
			return e
		}

		tx.Delete(key)
		return nil
	})
	return &api.DeleteLocationResponse{}, err
}
