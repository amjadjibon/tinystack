// Package efs persists EFS metadata. No NFS listener, network interfaces,
// filesystem bytes, POSIX users or directories are created.
package efs

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/efs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "efs"
	kindFS         = "file-system"
	kindAP         = "access-point"
	kindMT         = "mount-target"
	available      = "available"
	bursting       = "bursting"
	provisioned    = "provisioned"
	elastic        = "elastic"
	generalPurpose = "generalPurpose"
	maxIO          = "maxIO"
)

var _ api.API = (*Service)(nil)

// Network only reads existing EC2 records in the same scoped transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}
type Service struct {
	store   storage.Store
	network Network
}

func New(store storage.Store, network Network) *Service {
	return &Service{store: store, network: network}
}

type record struct {
	FS                    *api.FileSystemDescription
	AP                    *api.AccessPointDescription
	MT                    *api.MountTargetDescription
	Tags                  map[string]string
	ID, ARN, Kind, Parent string
	Groups                []string
}

func failure(code, message string) error {
	e := awserror.New(code, message)
	e.Fields = map[string]any{"ErrorCode": code}
	return e
}
func invalid(message string) error { return failure("BadRequest", message) }
func missing(kind string) error {
	code := "FileSystemNotFound"
	if kind == kindAP {
		code = "AccessPointNotFound"
	}
	if kind == kindMT {
		code = "MountTargetNotFound"
	}
	return failure(code, "Resource does not exist in this account and Region")
}

func arn(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:elasticfilesystem:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

var (
	resourceID  = regexp.MustCompile(`^(fs|fsap|fsmt)-[0-9a-f]{8,40}$`)
	resourceARN = regexp.MustCompile(
		`^arn:aws:elasticfilesystem:([a-z0-9-]+):([0-9]{12}):(file-system|access-point)/(fs|fsap)-[0-9a-f]{8,40}$`,
	)
)

func identifier(scope request.Scope, value *string, kind string) (id, resourceKind string, err error) {
	if value == nil || len(*value) > 128 {
		return "", "", invalid("A valid resource identifier is required")
	}
	id = *value
	if strings.HasPrefix(id, "arn:") {
		m := resourceARN.FindStringSubmatch(id)
		if m == nil {
			return "", "", invalid("Malformed or unsupported EFS ARN")
		}
		if kind != "" && m[3] != kind {
			return "", "", invalid("Incorrect resource type")
		}
		kind = m[3]
		if m[1] != scope.Region || m[2] != scope.AccountID {
			return "", "", missing(kind)
		}
		id = id[strings.LastIndex(id, "/")+1:]
	}
	m := resourceID.FindStringSubmatch(id)
	if m == nil {
		return "", "", invalid("Malformed EFS resource ID")
	}
	actual := kindFS
	if m[1] == "fsap" {
		actual = kindAP
	}
	if m[1] == "fsmt" {
		actual = kindMT
	}
	if kind != "" && actual != kind {
		return "", "", invalid("Incorrect resource type")
	}
	return id, actual, nil
}

func read(tx storage.ReadTx, scope request.Scope, value *string, kind string) (*record, error) {
	id, kind, err := identifier(scope, value, kind)
	if err != nil {
		return nil, err
	}
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, missing(kind)
	}
	return r, nil
}

func rows(tx storage.ReadTx, scope request.Scope, kind string) ([]*record, error) {
	return storage.Records[record](tx, scope.Prefix(serviceName, kind))
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
}

func fsOutput(r *record, count int64) *api.FileSystemDescription {
	out := common.Clone(r.FS)
	out.Tags = tagList(r.Tags)
	out.Name = tagName(r.Tags)
	out.NumberOfMountTargets = new(count)
	return out
}

func apOutput(r *record) *api.AccessPointDescription {
	out := common.Clone(r.AP)
	out.Tags = tagList(r.Tags)
	out.Name = tagName(r.Tags)
	return out
}

func mountCount(tx storage.ReadTx, scope request.Scope, parent string) (int64, error) {
	mounts, err := rows(tx, scope, kindMT)
	var n int64
	for _, r := range mounts {
		if r.Parent == parent {
			n++
		}
	}
	return n, err
}

func (s *Service) remove(ctx context.Context, value *string, kind string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, value, kind)
		if e != nil {
			return e
		}
		if kind == kindFS {
			n, e := mountCount(tx, scope, r.ID)
			if e != nil {
				return e
			}
			if n > 0 {
				return failure("FileSystemInUse", "Delete mount targets before deleting the file system")
			}
			// The user guide requires access points to be removed first as well.
			points, e := rows(tx, scope, kindAP)
			if e != nil {
				return e
			}
			for _, point := range points {
				if point.Parent == r.ID {
					return failure("FileSystemInUse", "Delete access points before deleting the file system")
				}
			}
		}
		tx.Delete(scope.Key(serviceName, kind, r.ID))
		return nil
	})
}

func (s *Service) DeleteFileSystem(
	ctx context.Context,
	in *api.DeleteFileSystemRequest,
) (*api.DeleteFileSystemOutput, error) {
	return &api.DeleteFileSystemOutput{}, s.remove(ctx, in.FileSystemId, kindFS)
}

func (s *Service) DeleteAccessPoint(
	ctx context.Context,
	in *api.DeleteAccessPointRequest,
) (*api.DeleteAccessPointOutput, error) {
	return &api.DeleteAccessPointOutput{}, s.remove(ctx, in.AccessPointId, kindAP)
}

func (s *Service) DeleteMountTarget(
	ctx context.Context,
	in *api.DeleteMountTargetRequest,
) (*api.DeleteMountTargetOutput, error) {
	return &api.DeleteMountTargetOutput{}, s.remove(ctx, in.MountTargetId, kindMT)
}
