package ec2

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindSnapshot   = "snapshot"
	stateCompleted = "completed"
	filterStatus   = "status"
	permissionAll  = "all"
)

type snapshot struct {
	Started     time.Time
	Tags        map[string]string `json:",omitempty"`
	ID          string
	Owner       string
	VolumeID    string
	Description string
	State       string
	Permissions []string
	Size        int64
	Encrypted   bool
}

func snapshotNotFound(id string) error { return notFound("InvalidSnapshot.NotFound", kindSnapshot, id) }

func assetIndex(scope request.Scope, kind, id string) string {
	return "aws/ec2/" + kind + "/" + scope.Region + "/" + id
}

func readAsset[T any](tx storage.ReadTx, scope request.Scope, kind, id string) (*T, error) {
	owner, found, err := storage.Read[string](tx, assetIndex(scope, kind, id))
	if err != nil || !found {
		return nil, err
	}

	scope.AccountID = *owner
	r, _, err := storage.Read[T](tx, scope.Key("ec2", kind, id))
	return r, err
}

func writeAsset(
	tx storage.WriteTx, scope request.Scope, kind, id string, record any,
) error {
	if err := storage.Write(tx, assetIndex(scope, kind, id), scope.AccountID); err != nil {
		return err
	}

	return writePendingStorage(tx, scope, kind, id, record)
}

func deleteAsset(tx storage.WriteTx, scope request.Scope, kind, id string) {
	tx.Delete(assetIndex(scope, kind, id))
	tx.Delete(storageTransitionKey(scope, kind, id))
	tx.Delete(scope.Key("ec2", kind, id))
}

func listAssets[T any](tx storage.ReadTx, scope request.Scope, kind string) ([]*T, error) {
	prefix := assetIndex(scope, kind, "")
	var out []*T
	for _, key := range tx.Keys(prefix) {
		r, err := readAsset[T](tx, scope, kind, strings.TrimPrefix(key, prefix))
		if err != nil {
			return nil, err
		}

		if r != nil {
			out = append(out, r)
		}
	}

	return out, nil
}

func catalogSnapshots(region string) []*snapshot {
	out := make([]*snapshot, 0, len(images))
	for i := range images {
		img := &images[i]
		created, _ := time.Parse(time.RFC3339Nano, img.Created)
		out = append(out, &snapshot{
			ID: derivedID("snap", region, img.Name), Owner: img.Owner, State: stateCompleted, Size: rootVolumeSize,
			Description: img.Description, Started: created, Permissions: []string{permissionAll},
		})
	}

	return out
}

func snapshotForUse(tx storage.ReadTx, scope request.Scope, id string, authorizedByImage bool) (*snapshot, error) {
	snap, err := readAsset[snapshot](tx, scope, kindSnapshot, id)
	if err != nil {
		return nil, err
	}

	if snap == nil {
		for _, candidate := range catalogSnapshots(scope.Region) {
			if candidate.ID == id {
				snap = candidate
				break
			}
		}
	}

	if snap == nil || (!authorizedByImage && !authorized(scope.AccountID, snap.Owner, snap.Permissions)) {
		return nil, snapshotNotFound(id)
	}

	if snap.State != stateCompleted {
		return nil, awserror.New("IncorrectState", "The snapshot must be completed")
	}

	return snap, nil
}

func (snap *snapshot) output() *api.Snapshot {
	progress := "0%"
	if snap.State == stateCompleted {
		progress = "100%"
	}

	return &api.Snapshot{
		SnapshotId: new(snap.ID), OwnerId: new(snap.Owner), VolumeId: optional(snap.VolumeID),
		Description: new(snap.Description), State: new(snap.State), VolumeSize: new(snap.Size),
		Encrypted: new(snap.Encrypted), StartTime: new(snap.Started), Progress: new(progress),
		StorageTier: new("standard"), Tags: tagList(snap.Tags),
	}
}

func (snap *snapshot) values() values {
	out := snap.output()
	return values{
		filterSnapshotID: {snap.ID},
		filterVolumeID:   {snap.VolumeID},
		filterOwnerID:    {snap.Owner},
		"owner-alias": {
			snapshotOwnerAlias(snap.Owner),
		},
		filterDescription: {snap.Description},
		filterStatus:      {snap.State},
		filterEncrypted:   boolValue(snap.Encrypted),
		"volume-size": {
			strconv.FormatInt(snap.Size, 10),
		},
		"progress":     {*out.Progress},
		"storage-tier": {*out.StorageTier},
		"start-time":   {snap.Started.Format(time.RFC3339)},
	}.withTags(snap.Tags)
}

func snapshotFromVolume(
	tx storage.WriteTx, scope request.Scope, v *volume, now time.Time, description string, tags map[string]string,
) (*snapshot, error) {
	if v.State == stateCreating {
		return nil, awserror.New("IncorrectState", "The volume is still creating")
	}

	snap := &snapshot{
		ID: newID("snap"), Owner: scope.AccountID, VolumeID: v.ID, State: statePending, Started: now,
		Size: v.Size, Encrypted: v.Encrypted, Description: description, Tags: tags,
	}

	return snap, writeAsset(tx, scope, kindSnapshot, snap.ID, snap)
}

func (s *Service) CreateSnapshot(ctx context.Context, in *api.CreateSnapshotRequest) (*api.Snapshot, error) {
	tags, tagErr := specTags(in.TagSpecifications, kindSnapshot)
	if tagErr != nil {
		return nil, tagErr
	}

	var out *api.Snapshot
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := readVolume(tx, scope, common.Value(in.VolumeId))
		if err != nil {
			return err
		}

		snap, err := snapshotFromVolume(tx, scope, v, s.now().UTC(), common.Value(in.Description), tags)
		if err != nil {
			return err
		}

		out = snap.output()
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeSnapshots(
	ctx context.Context, in *api.DescribeSnapshotsRequest,
) (*api.DescribeSnapshotsResult, error) {
	match, filterErr := compileFilters(in.Filters, (&snapshot{}).values().names()...)
	if filterErr != nil {
		return nil, filterErr
	}

	if err := validatePrincipals(in.OwnerIds, "self", amazonAlias); err != nil {
		return nil, err
	}

	if err := validatePrincipals(in.RestorableByUserIds, "self", permissionAll); err != nil {
		return nil, err
	}

	out := &api.DescribeSnapshotsResult{}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := listAssets[snapshot](tx, scope, kindSnapshot)
		if err != nil {
			return err
		}

		all = append(all, catalogSnapshots(scope.Region)...)
		all = slices.DeleteFunc(all, func(snap *snapshot) bool {
			return !authorized(scope.AccountID, snap.Owner, snap.Permissions)
		})

		id := func(snap *snapshot) string { return snap.ID }
		all, err = selectIDs(all, id, in.SnapshotIds, snapshotNotFound)
		if err != nil {
			return err
		}

		all = slices.DeleteFunc(all, func(snap *snapshot) bool {
			return !matchesOwner(scope.AccountID, snap.Owner, snapshotOwnerAlias(snap.Owner), in.OwnerIds) ||
				!matchesPermission(scope.AccountID, snap.Owner, snap.Permissions, in.RestorableByUserIds) ||
				!match.match(snap.values())
		})
		slices.SortFunc(all, func(a, b *snapshot) int { return strings.Compare(a.ID, b.ID) })
		selected, next, err := page(all, id, in.NextToken, in.MaxResults, maxDescribe)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, snap := range selected {
			out.Snapshots = append(out.Snapshots, *snap.output())
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func snapshotOwnerAlias(owner string) string {
	if owner == amazonOwner {
		return amazonAlias
	}

	return ""
}

func (s *Service) DeleteSnapshot(
	ctx context.Context,
	in *api.DeleteSnapshotRequest,
) (*api.DeleteSnapshotOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.SnapshotId)
		snap, err := ownedSnapshot(tx, scope, id)
		if err != nil {
			return err
		}

		used, err := snapshotInUse(tx, scope, id)
		if err != nil {
			return err
		}

		if used {
			return awserror.New("InvalidSnapshot.InUse", "The snapshot is in use by a registered AMI")
		}

		deleteAsset(tx, scope, kindSnapshot, snap.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSnapshotOutput{}, nil
}

func ownedSnapshot(tx storage.ReadTx, scope request.Scope, id string) (*snapshot, error) {
	snap, found, err := storage.Read[snapshot](tx, scope.Key("ec2", kindSnapshot, id))
	if err == nil && !found {
		err = snapshotNotFound(id)
	}

	return snap, err
}

func sourceRegion(scope request.Scope, region *string) (request.Scope, error) {
	if !slices.Contains(regions, common.Value(region)) && common.Value(region) != scope.Region {
		return scope, invalidParameter("Invalid source region: " + common.Value(region))
	}

	scope.Region = common.Value(region)
	return scope, nil
}

func copyVolumeSnapshot(
	tx storage.WriteTx, scope request.Scope, source *snapshot, now time.Time, description string,
	requestEncryption bool, tags map[string]string,
) (*snapshot, error) {
	enabled, _, err := storage.Read[bool](tx, scope.Key("ec2", "settings", "ebs-encryption"))
	if err != nil {
		return nil, err
	}

	copied := &snapshot{
		ID: newID("snap"), Owner: scope.AccountID, State: statePending, Started: now, Size: source.Size,
		Encrypted: source.Encrypted || requestEncryption || common.Value(enabled), Description: description, Tags: tags,
	}

	return copied, writeAsset(tx, scope, kindSnapshot, copied.ID, copied)
}

// The SDK automatically attaches a source-region presigned URL. Check its
// copy parameters locally and authorize the source against the caller's scope.
func checkCopySnapshotURL(raw *string, destination, source, id string) error {
	if raw == nil {
		return nil
	}

	target, err := url.Parse(*raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return invalidParameter("Invalid CopySnapshot presigned URL")
	}

	query := target.Query()
	if query.Get("Action") != "CopySnapshot" || query.Get("SourceRegion") != source ||
		query.Get("SourceSnapshotId") != id || query.Get("DestinationRegion") != destination ||
		query.Get("X-Amz-Signature") == "" {
		return invalidParameter("CopySnapshot presigned URL parameters do not match the request")
	}

	return nil
}

func (s *Service) CopySnapshot(ctx context.Context, in *api.CopySnapshotRequest) (*api.CopySnapshotResult, error) {
	tags, tagErr := specTags(in.TagSpecifications, kindSnapshot)
	if tagErr != nil {
		return nil, tagErr
	}

	out := &api.CopySnapshotResult{}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		if in.DestinationRegion != nil && *in.DestinationRegion != scope.Region {
			return invalidParameter("DestinationRegion must match the endpoint region")
		}

		from, err := sourceRegion(scope, in.SourceRegion)
		if err != nil {
			return err
		}

		id := common.Value(in.SourceSnapshotId)
		if urlErr := checkCopySnapshotURL(in.PresignedUrl, scope.Region, from.Region, id); urlErr != nil {
			return urlErr
		}

		snap, err := snapshotForUse(tx, from, id, false)
		if err != nil {
			return err
		}

		copied, err := copyVolumeSnapshot(tx, scope, snap, s.now().UTC(), common.Value(in.Description),
			common.Value(in.Encrypted), tags)
		if err != nil {
			return err
		}

		out.SnapshotId, out.Tags = new(copied.ID), tagList(copied.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeSnapshotAttribute(
	ctx context.Context, in *api.DescribeSnapshotAttributeRequest,
) (*api.DescribeSnapshotAttributeResult, error) {
	out := &api.DescribeSnapshotAttributeResult{SnapshotId: in.SnapshotId}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		snap, err := ownedSnapshot(tx, scope, common.Value(in.SnapshotId))
		if err != nil {
			return err
		}

		switch common.Value(in.Attribute) {
		case "createVolumePermission":
			for _, p := range snap.Permissions {
				user, group := permissionFields(p)
				out.CreateVolumePermissions = append(out.CreateVolumePermissions, api.CreateVolumePermission{
					UserId: user, Group: group,
				})
			}
		case attrProductCodes:
			out.ProductCodes = []api.ProductCode{}
		default:
			return invalidParameter("Unsupported snapshot attribute: " + common.Value(in.Attribute))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ModifySnapshotAttribute(
	ctx context.Context, in *api.ModifySnapshotAttributeRequest,
) (*api.ModifySnapshotAttributeOutput, error) {
	if in.Attribute != nil && *in.Attribute != "createVolumePermission" {
		return nil, invalidParameter("Unsupported snapshot attribute: " + *in.Attribute)
	}

	var add, remove []api.CreateVolumePermission
	if p := in.CreateVolumePermission; p != nil {
		add, remove = p.Add, p.Remove
	}

	change, permissionErr := parsePermissionChange(add, remove, in.UserIds, in.GroupNames, in.OperationType,
		func(p api.CreateVolumePermission) (*string, *string) { return p.UserId, p.Group })
	if permissionErr != nil {
		return nil, permissionErr
	}

	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		snap, err := ownedSnapshot(tx, scope, common.Value(in.SnapshotId))
		if err != nil {
			return err
		}

		if snap.Encrypted && len(change.add) != 0 {
			return awserror.New("OperationNotPermitted", "Sharing encrypted snapshots requires a customer-managed key")
		}

		snap.Permissions = change.apply(snap.Permissions)
		return storage.Write(tx, scope.Key("ec2", kindSnapshot, snap.ID), snap)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifySnapshotAttributeOutput{}, nil
}

func validatePrincipals(ids []string, aliases ...string) error {
	for _, id := range ids {
		if !accountID.MatchString(id) && !slices.Contains(aliases, id) {
			return invalidParameter(fmt.Sprintf("Invalid account or group: %q", id))
		}
	}

	return nil
}

func authorized(account, owner string, permissions []string) bool {
	return account == owner || slices.Contains(permissions, account) || slices.Contains(permissions, permissionAll)
}

func matchesOwner(account, owner, alias string, owners []string) bool {
	return len(owners) == 0 || slices.ContainsFunc(owners, func(want string) bool {
		return want == owner || want == alias || (want == "self" && account == owner)
	})
}

func matchesPermission(account, owner string, permissions, principals []string) bool {
	return len(principals) == 0 || slices.ContainsFunc(principals, func(p string) bool {
		switch p {
		case "self":
			return account == owner || slices.Contains(permissions, account)
		case permissionAll:
			return slices.Contains(permissions, permissionAll)
		default:
			return p == owner || slices.Contains(permissions, p)
		}
	})
}

func permissionFields(p string) (user, group *string) {
	if p == permissionAll {
		return nil, new(p)
	}

	return new(p), nil
}

type permissionChange struct {
	add, remove []string
}

func parsePermissionChange[T any](
	add, remove []T, users, groups []string, operation *string, fields func(T) (*string, *string),
) (permissionChange, error) {
	var change permissionChange
	if len(add)+len(remove) > 0 {
		if len(users)+len(groups) > 0 || operation != nil {
			return change, invalidParameter("Use one form of permission modification per request")
		}

		var err error
		change.add, err = permissionPrincipals(add, fields)
		if err != nil {
			return change, err
		}

		change.remove, err = permissionPrincipals(remove, fields)
		return change, err
	}

	if len(users)+len(groups) == 0 {
		return change, invalidParameter("Specify a user or group to modify")
	}

	if err := validatePrincipals(users); err != nil {
		return change, err
	}

	for _, group := range groups {
		if group != permissionAll {
			return change, invalidParameter("Invalid permission group: " + group)
		}
	}

	principals := append(slices.Clone(users), groups...)
	switch common.Value(operation) {
	case "add":
		change.add = principals
	case "remove":
		change.remove = principals
	default:
		return change, invalidParameter("OperationType must be add or remove")
	}

	return change, nil
}

func permissionPrincipals[T any](entries []T, fields func(T) (*string, *string)) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		user, group := fields(entry)
		switch {
		case group == nil && accountID.MatchString(common.Value(user)):
			out = append(out, *user)
		case user == nil && common.Value(group) == permissionAll:
			out = append(out, permissionAll)
		default:
			return nil, invalidParameter("Each permission must specify either an account ID or the group all")
		}
	}

	return out, nil
}

func (c permissionChange) apply(current []string) []string {
	current = slices.DeleteFunc(current, func(p string) bool { return slices.Contains(c.remove, p) })
	current = append(current, c.add...)
	slices.Sort(current)
	return slices.Compact(current)
}
