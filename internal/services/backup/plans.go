package backup

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateBackupPlan(
	ctx context.Context,
	in *api.CreateBackupPlanInput,
) (*api.CreateBackupPlanOutput, error) {
	if err := validatePlan(in.BackupPlan); err != nil {
		return nil, err
	}
	if err := tokenOK(in.CreatorRequestId); err != nil {
		return nil, err
	}
	if err := tagsOK(in.BackupPlanTags); err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, e := records(tx, scope, kindPlan)
		if e != nil {
			return e
		}
		for _, old := range rows {
			if in.CreatorRequestId != nil && old.Token != nil && *old.Token == *in.CreatorRequestId {
				r = old
				return nil
			}
		}
		for _, old := range rows {
			if old.Deleted == nil && old.Name == *in.BackupPlan.BackupPlanName {
				return failure("AlreadyExistsException", "Backup plan name already exists")
			}
		}
		count := 0
		for _, old := range rows {
			if old.Deleted == nil {
				count++
			}
		}
		if count >= 300 {
			return failure("LimitExceededException", "At most 300 active backup plans per account and Region")
		}
		snapshot, e := planSnapshot(tx, scope, in.BackupPlan, nil)
		if e != nil {
			return e
		}
		planID := id()
		now := stamp()
		r = &record{
			Kind: kindPlan, Name: *in.BackupPlan.BackupPlanName, ID: planID, ARN: arn(scope, "backup-plan", planID),
			Created: now, Token: in.CreatorRequestId, Tags: in.BackupPlanTags, Selections: map[string]selection{},
			Versions: []version{{ID: id(), Created: now, Plan: snapshot}},
		}
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}
	v := r.Versions[0]
	return &api.CreateBackupPlanOutput{
		BackupPlanId:  new(r.ID),
		BackupPlanArn: new(r.ARN),
		CreationDate:  new(v.Created),
		VersionId:     new(v.ID),
	}, nil
}

func (s *Service) GetBackupPlan(ctx context.Context, in *api.GetBackupPlanInput) (*api.GetBackupPlanOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var r *record
	var v version
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = read(tx, scope, kindPlan, common.Value(in.BackupPlanId))
		if e != nil {
			return e
		}
		v = r.Versions[len(r.Versions)-1]
		if in.VersionId != nil {
			if *in.VersionId == "" {
				return invalid("VersionId must not be empty")
			}
			found := false
			for _, item := range r.Versions {
				if item.ID == *in.VersionId {
					v = item
					found = true
					break
				}
			}
			if !found {
				return missing()
			}
		}
		if v.Deleted != nil {
			return missing()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &api.GetBackupPlanOutput{
		BackupPlan: &v.Plan, BackupPlanId: new(r.ID), BackupPlanArn: new(r.ARN),
		VersionId: new(v.ID), CreationDate: new(v.Created), CreatorRequestId: r.Token,
	}, nil
}

func (s *Service) UpdateBackupPlan(
	ctx context.Context,
	in *api.UpdateBackupPlanInput,
) (*api.UpdateBackupPlanOutput, error) {
	if err := validatePlan(in.BackupPlan); err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var r *record
	var v version
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = activePlan(tx, scope, common.Value(in.BackupPlanId))
		if e != nil {
			return e
		}
		rows, e := records(tx, scope, kindPlan)
		if e != nil {
			return e
		}
		for _, old := range rows {
			if old.ID != r.ID && old.Deleted == nil && old.Name == *in.BackupPlan.BackupPlanName {
				return invalid("Backup plan name already exists")
			}
		}
		if len(r.Versions) >= 2000 {
			return invalid("At most 2000 versions per plan")
		}
		snapshot, e := planSnapshot(tx, scope, in.BackupPlan, &r.Versions[len(r.Versions)-1].Plan)
		if e != nil {
			return e
		}
		v = version{ID: id(), Created: stamp(), Plan: snapshot}
		r.Versions = append(r.Versions, v)
		r.Name = *in.BackupPlan.BackupPlanName
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}
	return &api.UpdateBackupPlanOutput{
		BackupPlanId:  new(r.ID),
		BackupPlanArn: new(r.ARN),
		CreationDate:  new(v.Created),
		VersionId:     new(v.ID),
	}, nil
}

func (s *Service) DeleteBackupPlan(
	ctx context.Context,
	in *api.DeleteBackupPlanInput,
) (*api.DeleteBackupPlanOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = activePlan(tx, scope, common.Value(in.BackupPlanId))
		if e != nil {
			return e
		}
		if len(r.Selections) > 0 {
			return failure("InvalidRequestException", "Delete all resource selections before deleting a backup plan")
		}
		r.Deleted = new(stamp())
		r.Versions[len(r.Versions)-1].Deleted = r.Deleted
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}
	return &api.DeleteBackupPlanOutput{
		BackupPlanId: new(r.ID), BackupPlanArn: new(r.ARN), DeletionDate: r.Deleted,
		VersionId: new(r.Versions[len(r.Versions)-1].ID),
	}, nil
}

func planMember(r *record, v version) api.BackupPlansListMember {
	return api.BackupPlansListMember{
		BackupPlanId: new(r.ID), BackupPlanArn: new(r.ARN), BackupPlanName: v.Plan.BackupPlanName,
		CreationDate: new(v.Created), DeletionDate: v.Deleted, VersionId: new(v.ID), CreatorRequestId: r.Token,
	}
}

func (s *Service) ListBackupPlans(
	ctx context.Context,
	in *api.ListBackupPlansInput,
) (*api.ListBackupPlansOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	rows := []api.BackupPlansListMember{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		plans, e := records(tx, scope, kindPlan)
		if e != nil {
			return e
		}
		for _, r := range plans {
			if r.Deleted == nil || common.Value(in.IncludeDeleted) {
				rows = append(rows, planMember(r, r.Versions[len(r.Versions)-1]))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	query := "plans/active"
	if common.Value(in.IncludeDeleted) {
		query = "plans/all"
	}
	rows, next, err := page(
		rows,
		func(r api.BackupPlansListMember) string { return *r.BackupPlanId },
		in.NextToken,
		in.MaxResults,
		scope,
		query,
	)
	return &api.ListBackupPlansOutput{BackupPlansList: rows, NextToken: next}, err
}

func (s *Service) ListBackupPlanVersions(
	ctx context.Context,
	in *api.ListBackupPlanVersionsInput,
) (*api.ListBackupPlanVersionsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	rows := []api.BackupPlansListMember{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read(tx, scope, kindPlan, common.Value(in.BackupPlanId))
		if e != nil {
			return e
		}
		for _, v := range r.Versions {
			rows = append(rows, planMember(r, v))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(
		rows,
		func(a, b api.BackupPlansListMember) int { return strings.Compare(*a.VersionId, *b.VersionId) },
	)
	rows, next, err := page(
		rows,
		func(r api.BackupPlansListMember) string { return *r.VersionId },
		in.NextToken,
		in.MaxResults,
		scope,
		"versions/"+common.Value(in.BackupPlanId),
	)
	return &api.ListBackupPlanVersionsOutput{BackupPlanVersionsList: rows, NextToken: next}, err
}
