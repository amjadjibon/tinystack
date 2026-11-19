package rds

import (
	"context"
	"maps"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const restoreAttribute = "restore"

var snapshotAccountID = regexp.MustCompile(`^\d{12}$`)

func snapshotStateError(kind, message string) error {
	if kind == kindClusterSnapshot {
		return awserror.New("InvalidDBClusterSnapshotStateFault", message)
	}

	return awserror.New("InvalidDBSnapshotStateFault", message)
}

func (s *Service) DescribeDBSnapshotAttributes(
	ctx context.Context, in *api.DescribeDBSnapshotAttributesMessage,
) (*api.DescribeDBSnapshotAttributesResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.snapshotRecord(ctx, scope, kindSnapshot, common.Value(in.DBSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	return &api.DescribeDBSnapshotAttributesResult{DBSnapshotAttributesResult: snapshotAttributes(r)}, nil
}

func (s *Service) DescribeDBClusterSnapshotAttributes(
	ctx context.Context, in *api.DescribeDBClusterSnapshotAttributesMessage,
) (*api.DescribeDBClusterSnapshotAttributesResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.snapshotRecord(ctx, scope, kindClusterSnapshot, common.Value(in.DBClusterSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	return &api.DescribeDBClusterSnapshotAttributesResult{
		DBClusterSnapshotAttributesResult: clusterSnapshotAttributes(r),
	}, nil
}

func (s *Service) ModifyDBSnapshotAttribute(
	ctx context.Context, in *api.ModifyDBSnapshotAttributeMessage,
) (*api.ModifyDBSnapshotAttributeResult, error) {
	r, err := s.modifySnapshotRestoreAccounts(ctx, kindSnapshot, common.Value(in.DBSnapshotIdentifier),
		common.Value(in.AttributeName), in.ValuesToAdd, in.ValuesToRemove)
	if err != nil {
		return nil, err
	}

	return &api.ModifyDBSnapshotAttributeResult{DBSnapshotAttributesResult: snapshotAttributes(r)}, nil
}

func (s *Service) ModifyDBClusterSnapshotAttribute(
	ctx context.Context, in *api.ModifyDBClusterSnapshotAttributeMessage,
) (*api.ModifyDBClusterSnapshotAttributeResult, error) {
	r, err := s.modifySnapshotRestoreAccounts(ctx, kindClusterSnapshot, common.Value(in.DBClusterSnapshotIdentifier),
		common.Value(in.AttributeName), in.ValuesToAdd, in.ValuesToRemove)
	if err != nil {
		return nil, err
	}

	return &api.ModifyDBClusterSnapshotAttributeResult{
		DBClusterSnapshotAttributesResult: clusterSnapshotAttributes(r),
	}, nil
}

func snapshotAttributes(r *record) *api.DBSnapshotAttributesResult {
	return &api.DBSnapshotAttributesResult{
		DBSnapshotIdentifier: new(r.Name),
		DBSnapshotAttributes: []api.DBSnapshotAttribute{{
			AttributeName: new(restoreAttribute), AttributeValues: r.RestoreAccounts,
		}},
	}
}

func clusterSnapshotAttributes(r *record) *api.DBClusterSnapshotAttributesResult {
	return &api.DBClusterSnapshotAttributesResult{
		DBClusterSnapshotIdentifier: new(r.Name),
		DBClusterSnapshotAttributes: []api.DBClusterSnapshotAttribute{{
			AttributeName: new(restoreAttribute), AttributeValues: r.RestoreAccounts,
		}},
	}
}

func (s *Service) modifySnapshotRestoreAccounts(
	ctx context.Context, kind, identifier, attribute string, add, remove []string,
) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := resourceID(scope, kind, identifier)
	if err != nil {
		return nil, err
	}

	if attribute != restoreAttribute {
		return nil, invalid("Only the restore snapshot attribute is supported")
	}

	for _, value := range slices.Concat(add, remove) {
		if value != "all" && !snapshotAccountID.MatchString(value) {
			return nil, invalid("Snapshot restore values must be 12-digit AWS account IDs or all")
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var readErr error
		r, readErr = readRecord(tx, scope, kind, id)
		if readErr != nil {
			return readErr
		}

		if shareErr := checkSnapshotShareable(r, add); shareErr != nil {
			return shareErr
		}

		accounts := make(map[string]bool)
		for _, value := range slices.Concat(r.RestoreAccounts, add) {
			accounts[value] = true
		}

		for _, value := range remove {
			delete(accounts, value)
		}

		count := len(accounts)
		if accounts["all"] {
			count--
		}

		if count > 20 {
			return awserror.New("SharedSnapshotQuotaExceededFault", "A snapshot can be shared with at most 20 accounts")
		}

		r.RestoreAccounts = slices.Sorted(maps.Keys(accounts))
		return writeRecord(tx, scope, r)
	})
	return r, err
}

func checkSnapshotShareable(r *record, add []string) error {
	var typ string
	var encrypted bool
	if r.Kind == kindClusterSnapshot {
		if err := clusterSnapshotReady(r); err != nil {
			return err
		}

		typ, encrypted = common.Value(r.ClusterSnapshot.SnapshotType), common.Value(r.ClusterSnapshot.StorageEncrypted)
	} else {
		if err := snapshotReady(r); err != nil {
			return err
		}

		typ, encrypted = common.Value(r.Snapshot.SnapshotType), common.Value(r.Snapshot.Encrypted)
	}

	if typ != manualSnapshot {
		return snapshotStateError(r.Kind, "Only manual snapshots support sharing")
	}

	if encrypted && slices.Contains(add, "all") {
		return invalid("Encrypted snapshots cannot be shared publicly")
	}

	return nil
}
