package redshiftserverless

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func retention(p *int64) error {
	if p != nil && *p != -1 && (*p < 1 || *p > 3653) {
		return invalid("retentionPeriod must be -1 or 1-3653 days")
	}

	return nil
}

func snapshotRecord(
	tx storage.WriteTx,
	scope request.Scope,
	n *record,
	name *string,
	period *int64,
	tags map[string]string,
) (*record, error) {
	if validationErr := retention(period); validationErr != nil {
		return nil, validationErr
	}

	now := time.Now().UTC()
	nsp := n.Namespace
	snap := &api.Snapshot{
		SnapshotName:                name,
		SnapshotArn:                 new(arn(scope, kindSnapshot, common.Value(name))),
		NamespaceName:               nsp.NamespaceName,
		NamespaceArn:                nsp.NamespaceArn,
		AdminUsername:               nsp.AdminUsername,
		KmsKeyId:                    nsp.KmsKeyId,
		OwnerAccount:                new(scope.AccountID),
		Status:                      new(statusAvailable),
		SnapshotCreateTime:          new(now),
		SnapshotRetentionStartTime:  new(now),
		SnapshotRetentionPeriod:     common.OrElse(period, new(int64(-1))),
		SnapshotRemainingDays:       common.OrElse(period, new(int64(-1))),
		AdminPasswordSecretArn:      nsp.AdminPasswordSecretArn,
		AdminPasswordSecretKmsKeyId: nsp.AdminPasswordSecretKmsKeyId,
	}

	r := &record{
		Name:     common.Value(name),
		Kind:     kindSnapshot,
		ARN:      *snap.SnapshotArn,
		Snapshot: snap,
		Settings: common.Clone(nsp),
		Tags:     tags,
	}

	return r, insert(tx, scope, r)
}

func (s *Service) CreateSnapshot(
	ctx context.Context,
	in *api.CreateSnapshotRequest,
) (*api.CreateSnapshotResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		n, e := read(tx, scope, kindNamespace, common.Value(in.NamespaceName))
		if e != nil {
			return e
		}

		r, e = snapshotRecord(tx, scope, n, in.SnapshotName, in.RetentionPeriod, tags)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateSnapshotResponse{Snapshot: r.Snapshot}, nil
}

func (s *Service) snapshot(tx storage.ReadTx, scope request.Scope, name, a *string) (*record, error) {
	if (name == nil) == (a == nil) {
		return nil, invalid("Specify exactly one of snapshotName or snapshotArn")
	}

	if a != nil {
		r, err := byARN(tx, scope, *a)
		if err != nil {
			return nil, err
		}

		if r.Snapshot == nil {
			return nil, missing(kindSnapshot, *a)
		}

		return r, nil
	}

	return read(tx, scope, kindSnapshot, common.Value(name))
}

func (s *Service) UpdateSnapshot(
	ctx context.Context,
	in *api.UpdateSnapshotRequest,
) (*api.UpdateSnapshotResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if validationErr := retention(in.RetentionPeriod); validationErr != nil {
		return nil, validationErr
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindSnapshot, common.Value(in.SnapshotName))
		if e != nil {
			return e
		}

		if in.RetentionPeriod != nil {
			r.Snapshot.SnapshotRetentionPeriod = in.RetentionPeriod
			r.Snapshot.SnapshotRemainingDays = in.RetentionPeriod
		}

		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateSnapshotResponse{Snapshot: r.Snapshot}, nil
}

func (s *Service) RestoreFromSnapshot(
	ctx context.Context,
	in *api.RestoreFromSnapshotRequest,
) (*api.RestoreFromSnapshotResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.OwnerAccount != nil && *in.OwnerAccount != scope.AccountID {
		return nil, missing(kindSnapshot, common.Value(in.SnapshotName))
	}

	n, err := s.get(ctx, kindNamespace, in.NamespaceName)
	if err != nil {
		return nil, err
	}

	w, err := s.get(ctx, kindWorkgroup, in.WorkgroupName)
	if err != nil {
		return nil, err
	}

	if common.Value(w.Workgroup.NamespaceName) != n.Name {
		return nil, invalid("Workgroup belongs to a different namespace")
	}

	var snap *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		snap, e = s.snapshot(tx, scope, in.SnapshotName, in.SnapshotArn)
		return e
	})
	if err != nil {
		return nil, err
	}

	restored := common.Clone(snap.Settings)
	restored.NamespaceName = n.Namespace.NamespaceName
	restored.NamespaceId = n.Namespace.NamespaceId
	restored.NamespaceArn = n.Namespace.NamespaceArn
	restored.CreationDate = n.Namespace.CreationDate
	restored.Status = new(statusModifying)
	if in.AdminPasswordSecretKmsKeyId != nil {
		restored.AdminPasswordSecretKmsKeyId = in.AdminPasswordSecretKmsKeyId
	}

	if validationErr := s.namespaceReferences(ctx, restored); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := s.managedPassword(ctx, restored, in.ManageAdminPassword, nil); validationErr != nil {
		return nil, validationErr
	}

	n.Namespace = restored
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := read(tx, scope, n.Kind, n.Name); e != nil {
			return e
		}

		return queue(tx, scope, n)
	})
	return &api.RestoreFromSnapshotResponse{
		Namespace:    restored,
		SnapshotName: snap.Snapshot.SnapshotName,
		OwnerAccount: new(scope.AccountID),
	}, err
}
