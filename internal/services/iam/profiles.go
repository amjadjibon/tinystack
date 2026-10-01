package iam

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindProfile = "instance-profile"

// profile is an instance profile: a container for at most one role, which EC2
// instances are launched with.
type profile struct {
	Tags      map[string]string `json:",omitempty"`
	Name      string
	ID        string
	Path      string
	Role      string `json:",omitempty"`
	CreatedAt int64
}

func profileARN(account string, p *profile) string { return arn(account, kindProfile+p.Path+p.Name) }

func loadProfile(tx storage.ReadTx, account string, name *string) (*profile, string, error) {
	if err := checkName("instanceProfileName", name, roleName); err != nil {
		return nil, "", err
	}

	pk := key(account, kindProfile, *name)
	p, ok, err := read[profile](tx, pk)
	switch {
	case err != nil:
		return nil, "", err
	case !ok:
		return nil, "", noSuchEntity("Instance Profile %s cannot be found.", *name)
	}

	return p, pk, nil
}

// updateProfile runs fn on the named instance profile and stores the result,
// all in one transaction.
func (s *Service) updateProfile(
	ctx context.Context,
	name *string,
	fn func(tx storage.WriteTx, account string, p *profile) error,
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, pk, loadErr := loadProfile(tx, m.AccountID, name)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(tx, m.AccountID, p); fnErr != nil {
			return fnErr
		}

		return write(tx, pk, p)
	})
}

// profileOutput describes an instance profile with its role. Tags are only in
// single-profile responses, as in AWS.
func profileOutput(tx storage.ReadTx, account string, p *profile, tags bool) (*api.InstanceProfile, error) {
	out := &api.InstanceProfile{
		Path: new(p.Path), InstanceProfileName: new(p.Name), InstanceProfileId: new(p.ID),
		Arn: new(profileARN(account, p)), CreateDate: millis(p.CreatedAt), Roles: []api.Role{},
	}
	if p.Role != "" {
		r, ok, err := read[role](tx, key(account, kindRole, p.Role))
		if err != nil {
			return nil, err
		}

		if ok {
			out.Roles = append(out.Roles, *roleOutput(account, r, false))
		}
	}

	if tags && len(p.Tags) > 0 {
		out.Tags = tagList(p.Tags)
	}

	return out, nil
}

// profilesWithRole lists the instance profiles that hold a role.
func profilesWithRole(tx storage.ReadTx, account, roleName string) ([]*profile, error) {
	all, err := list[profile](tx, account, kindProfile)
	if err != nil {
		return nil, err
	}

	var out []*profile
	for _, p := range all {
		if strings.EqualFold(p.Role, roleName) {
			out = append(out, p)
		}
	}

	return out, nil
}

func (s *Service) CreateInstanceProfile(
	ctx context.Context,
	in *api.CreateInstanceProfileRequest,
) (*api.CreateInstanceProfileResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName("instanceProfileName", in.InstanceProfileName, roleName); guardErr != nil {
		return nil, guardErr
	}

	path, err := checkPath(in.Path, rolePath)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if guardErr := tagError(tagging.Check(tags)); guardErr != nil {
		return nil, guardErr
	}

	out := &api.CreateInstanceProfileResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		pk := key(m.AccountID, kindProfile, *in.InstanceProfileName)
		if _, exists := tx.Get(pk); exists {
			return alreadyExists("Instance Profile %s already exists.", *in.InstanceProfileName)
		}

		p := &profile{
			Tags: tags, Name: *in.InstanceProfileName, ID: newID("AIPA"), Path: path,
			CreatedAt: s.now().UnixMilli(),
		}

		var outErr error
		if out.InstanceProfile, outErr = profileOutput(tx, m.AccountID, p, true); outErr != nil {
			return outErr
		}

		return write(tx, pk, p)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetInstanceProfile(
	ctx context.Context,
	in *api.GetInstanceProfileRequest,
) (*api.GetInstanceProfileResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetInstanceProfileResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, _, loadErr := loadProfile(tx, m.AccountID, in.InstanceProfileName)
		if loadErr != nil {
			return loadErr
		}

		out.InstanceProfile, loadErr = profileOutput(tx, m.AccountID, p, true)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteInstanceProfile deletes an instance profile that holds no role.
func (s *Service) DeleteInstanceProfile(
	ctx context.Context,
	in *api.DeleteInstanceProfileRequest,
) (*api.DeleteInstanceProfileOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, pk, loadErr := loadProfile(tx, m.AccountID, in.InstanceProfileName)
		switch {
		case loadErr != nil:
			return loadErr
		case p.Role != "":
			return deleteConflict("Cannot delete entity, must remove roles from instance profile first.")
		}

		tx.Delete(pk)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteInstanceProfileOutput{}, nil
}

// listProfiles pages through the profiles that keep selects, in name order.
func listProfiles(tx storage.ReadTx, account string, keep func(*profile) bool, marker *string, maxItems *int64) (
	out []api.InstanceProfile, truncated *bool, next *string, err error,
) {
	all, err := list[profile](tx, account, kindProfile)
	if err != nil {
		return nil, nil, nil, err
	}

	var matched []*profile
	for _, p := range all {
		if keep(p) {
			matched = append(matched, p)
		}
	}

	selected, truncated, next, err := page(matched, func(p *profile) string { return strings.ToLower(p.Name) },
		marker, maxItems)
	if err != nil {
		return nil, nil, nil, err
	}

	out = []api.InstanceProfile{}
	for _, p := range selected {
		described, describeErr := profileOutput(tx, account, p, false)
		if describeErr != nil {
			return nil, nil, nil, describeErr
		}

		out = append(out, *described)
	}

	return out, truncated, next, nil
}

func (s *Service) ListInstanceProfiles(
	ctx context.Context,
	in *api.ListInstanceProfilesRequest,
) (*api.ListInstanceProfilesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.PathPrefix != nil && !pathPrefix.MatchString(*in.PathPrefix) {
		return nil, validation("The specified value for pathPrefix is invalid.")
	}

	out := &api.ListInstanceProfilesResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var listErr error
		out.InstanceProfiles, out.IsTruncated, out.Marker, listErr = listProfiles(tx, m.AccountID,
			func(p *profile) bool { return strings.HasPrefix(p.Path, value(in.PathPrefix)) }, in.Marker, in.MaxItems)
		return listErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ListInstanceProfilesForRole lists the instance profiles that hold a role.
func (s *Service) ListInstanceProfilesForRole(
	ctx context.Context,
	in *api.ListInstanceProfilesForRoleRequest,
) (*api.ListInstanceProfilesForRoleResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListInstanceProfilesForRoleResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, loadErr := loadRole(tx, m.AccountID, in.RoleName)
		if loadErr != nil {
			return loadErr
		}

		out.InstanceProfiles, out.IsTruncated, out.Marker, loadErr = listProfiles(tx, m.AccountID,
			func(p *profile) bool { return strings.EqualFold(p.Role, r.Name) }, in.Marker, in.MaxItems)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// AddRoleToInstanceProfile puts a role in an instance profile, which holds at
// most one.
func (s *Service) AddRoleToInstanceProfile(
	ctx context.Context,
	in *api.AddRoleToInstanceProfileRequest,
) (*api.AddRoleToInstanceProfileOutput, error) {
	err := s.updateProfile(ctx, in.InstanceProfileName, func(tx storage.WriteTx, account string, p *profile) error {
		r, _, err := loadRole(tx, account, in.RoleName)
		switch {
		case err != nil:
			return err
		case p.Role != "":
			return limitExceeded("Cannot exceed quota for InstanceSessionsPerInstanceProfile: 1")
		}

		p.Role = r.Name
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddRoleToInstanceProfileOutput{}, nil
}

func (s *Service) RemoveRoleFromInstanceProfile(
	ctx context.Context,
	in *api.RemoveRoleFromInstanceProfileRequest,
) (*api.RemoveRoleFromInstanceProfileOutput, error) {
	err := s.updateProfile(ctx, in.InstanceProfileName, func(_ storage.WriteTx, _ string, p *profile) error {
		if p.Role == "" || !strings.EqualFold(p.Role, value(in.RoleName)) {
			return noSuchEntity("The role with name %s cannot be found.", value(in.RoleName))
		}

		p.Role = ""
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (s *Service) TagInstanceProfile(
	ctx context.Context,
	in *api.TagInstanceProfileRequest,
) (*api.TagInstanceProfileOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.updateProfile(ctx, in.InstanceProfileName, func(_ storage.WriteTx, _ string, p *profile) error {
		merged, mergeErr := tagging.Merge(p.Tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		p.Tags = merged
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagInstanceProfileOutput{}, nil
}

func (s *Service) UntagInstanceProfile(
	ctx context.Context,
	in *api.UntagInstanceProfileRequest,
) (*api.UntagInstanceProfileOutput, error) {
	err := s.updateProfile(ctx, in.InstanceProfileName, func(_ storage.WriteTx, _ string, p *profile) error {
		p.Tags = tagging.Remove(p.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagInstanceProfileOutput{}, nil
}

func (s *Service) ListInstanceProfileTags(
	ctx context.Context,
	in *api.ListInstanceProfileTagsRequest,
) (*api.ListInstanceProfileTagsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListInstanceProfileTagsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, _, loadErr := loadProfile(tx, m.AccountID, in.InstanceProfileName)
		if loadErr != nil {
			return loadErr
		}

		out.Tags, out.IsTruncated, out.Marker, loadErr = page(
			tagList(p.Tags), func(t api.Tag) string { return *t.Key }, in.Marker, in.MaxItems)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// InstanceProfile is what EC2 needs to know about an instance profile.
type InstanceProfile struct {
	ARN string
	ID  string
}

// InstanceProfileOf returns the instance profile in account that ref names: its
// name, or an ARN whose path matches the profile's. It reports false when there
// is none.
func (s *Service) InstanceProfileOf(ctx context.Context, account, ref string) (InstanceProfile, bool, error) {
	name := ref
	if rest, isARN := strings.CutPrefix(ref, "arn:aws:iam::"+account+":"+kindProfile+"/"); isARN {
		name = rest[strings.LastIndex(rest, "/")+1:]
	} else if strings.HasPrefix(ref, "arn:") {
		return InstanceProfile{}, false, nil
	}

	var out InstanceProfile
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		p, ok, err := read[profile](tx, key(account, kindProfile, name))
		if err != nil || !ok {
			return err
		}

		out = InstanceProfile{ARN: profileARN(account, p), ID: p.ID}
		found = !strings.HasPrefix(ref, "arn:") || out.ARN == ref
		return nil
	})

	return out, found, err
}
