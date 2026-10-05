package eks

import (
	"context"
	"slices"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// update is an update's record: the update, which always succeeded, and the
// node group or add-on it changed, if any.
type update struct {
	Created   time.Time
	Nodegroup string `json:",omitempty"`
	Addon     string `json:",omitempty"`
	Update    api.Update
}

// record stores a successful update of a cluster, or of its node group or
// add-on, and returns it.
func (s *Service) record(
	tx storage.WriteTx,
	scope request.Scope,
	clusterName, nodegroupName, addonName, kind string,
	params []api.UpdateParam,
) (*api.Update, error) {
	now := s.now().UTC()
	u := update{Created: now, Nodegroup: nodegroupName, Addon: addonName, Update: api.Update{
		Id: new(
			uuid.NewV4().String(),
		),
		Status:    new("Successful"),
		Type:      new(kind),
		Params:    params,
		CreatedAt: common.SecondsMilli(now),
		Errors:    []api.ErrorDetail{},
	}}

	return &u.Update, storage.Write(tx, owned(scope, "update", clusterName, *u.Update.Id), u)
}

func (s *Service) DescribeUpdate(
	ctx context.Context,
	in *api.DescribeUpdateRequest,
) (*api.DescribeUpdateResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.NodegroupName != nil && in.AddonName != nil {
		return nil, invalidParameter("nodegroupName and addonName cannot be combined.")
	}

	var out *api.Update
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.Name)
		if loadErr != nil {
			return loadErr
		}

		u, ok, readErr := storage.Read[update](tx, owned(scope, "update", c.Name, common.Value(in.UpdateId)))
		if readErr != nil || !ok || u.Nodegroup != common.Value(in.NodegroupName) ||
			u.Addon != common.Value(in.AddonName) {
			return common.ErrorsOr(readErr, notFound("No update found for ID: %s", common.Value(in.UpdateId)))
		}

		out = &u.Update
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeUpdateResponse{Update: out}, nil
}

// ListUpdates lists a cluster's updates, or its node group's or add-on's,
// oldest first.
func (s *Service) ListUpdates(ctx context.Context, in *api.ListUpdatesRequest) (*api.ListUpdatesResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListUpdatesResponse{UpdateIds: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.Name)
		if loadErr != nil {
			return loadErr
		}

		updates, readErr := storage.Records[update](tx, prefix(scope, "update")+c.Name+"/")
		if readErr != nil {
			return readErr
		}

		slices.SortFunc(updates, func(a, b *update) int { return a.Created.Compare(b.Created) })
		var ids []string
		for _, u := range updates {
			if u.Nodegroup == common.Value(in.NodegroupName) && u.Addon == common.Value(in.AddonName) {
				ids = append(ids, *u.Update.Id)
			}
		}

		ids, out.NextToken, readErr = page(ids, func(id string) string { return id }, in.NextToken, in.MaxResults)
		out.UpdateIds = append(out.UpdateIds, ids...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
