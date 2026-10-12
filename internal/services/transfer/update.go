package transfer

import (
	"context"
	"maps"
	"reflect"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func applyServer(s *api.DescribedServer, in *api.UpdateServerRequest) {
	if in.EndpointType != nil {
		s.EndpointType = in.EndpointType
	}
	if in.IdentityProviderType != nil {
		s.IdentityProviderType = in.IdentityProviderType
	}
	if in.Protocols != nil {
		s.Protocols = in.Protocols
	}
	if in.SecurityPolicyName != nil {
		s.SecurityPolicyName = in.SecurityPolicyName
	}
	if in.PreAuthenticationLoginBanner != nil {
		s.PreAuthenticationLoginBanner = in.PreAuthenticationLoginBanner
	}
}

func applyUser(u *api.DescribedUser, in *api.UpdateUserRequest) {
	if in.Role != nil {
		u.Role = in.Role
	}
	if in.HomeDirectory != nil {
		u.HomeDirectory = in.HomeDirectory
	}
	if in.HomeDirectoryType != nil {
		u.HomeDirectoryType = in.HomeDirectoryType
	}
}

func ownedTags(r *Snapshot, owned, desired map[string]string) {
	if desired == nil {
		return
	}
	tags := tagMap(tagsOf(r))
	for k := range owned {
		if _, ok := desired[k]; !ok {
			delete(tags, k)
		}
	}
	maps.Copy(tags, desired)
	setTags(r, tagList(tags))
}

func (s *Service) updateServer(
	ctx context.Context,
	in *api.UpdateServerRequest,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, nil, e
	}
	if in == nil {
		return nil, nil, invalid("UpdateServer input is required")
	}
	if in.Protocols != nil && len(in.Protocols) == 0 {
		return nil, nil, invalid("Protocols must contain SFTP")
	}
	in = common.Clone(in)
	owned = maps.Clone(owned)
	desired = maps.Clone(desired)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readServer(tx, scope, common.Value(in.ServerId))
		if readErr != nil {
			return readErr
		}
		before = common.Clone(r)
		applyServer(r.Server, in)
		ownedTags(r, owned, desired)
		if err := ValidateServerConfiguration(r.Server); err != nil {
			return err
		}
		if !reflect.DeepEqual(before, r) {
			r.Revision = revision()
			if err := save(tx, scope, r); err != nil {
				return err
			}
		}
		after = common.Clone(r)
		return nil
	})
	if e != nil {
		return nil, nil, e
	}
	return before, after, nil
}

func (s *Service) updateUser(
	ctx context.Context,
	in *api.UpdateUserRequest,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, nil, e
	}
	if in == nil {
		return nil, nil, invalid("UpdateUser input is required")
	}
	in = common.Clone(in)
	owned = maps.Clone(owned)
	desired = maps.Clone(desired)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readUser(tx, scope, common.Value(in.ServerId), common.Value(in.UserName))
		if readErr != nil {
			return readErr
		}
		before = common.Clone(r)
		applyUser(r.User, in)
		ownedTags(r, owned, desired)
		if err := ValidateUserConfiguration(r.User, scope); err != nil {
			return err
		}
		if !reflect.DeepEqual(before, r) {
			r.Revision = revision()
			if err := save(tx, scope, r); err != nil {
				return err
			}
		}
		after = common.Clone(r)
		return nil
	})
	if e != nil {
		return nil, nil, e
	}
	return before, after, nil
}

func (s *Service) UpdateServer(ctx context.Context, in *api.UpdateServerRequest) (*api.UpdateServerResponse, error) {
	_, r, e := s.updateServer(ctx, in, nil, nil)
	if e != nil {
		return nil, e
	}
	return &api.UpdateServerResponse{ServerId: new(r.ServerID)}, nil
}

func (s *Service) UpdateUser(ctx context.Context, in *api.UpdateUserRequest) (*api.UpdateUserResponse, error) {
	_, r, e := s.updateUser(ctx, in, nil, nil)
	if e != nil {
		return nil, e
	}
	return &api.UpdateUserResponse{ServerId: new(r.ServerID), UserName: common.Clone(r.User.UserName)}, nil
}

// CloudFormationUpdateServer commits configuration and final owned/external tags in one transaction.
func (s *Service) CloudFormationUpdateServer(
	ctx context.Context,
	in *api.UpdateServerRequest,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	return s.updateServer(ctx, in, owned, desired)
}

// CloudFormationUpdateUser also retains independently imported public-key IDs and timestamps.
func (s *Service) CloudFormationUpdateUser(
	ctx context.Context,
	in *api.UpdateUserRequest,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	return s.updateUser(ctx, in, owned, desired)
}

// CloudFormationRestore restores actual live metadata only at the captured incarnation and committed revision.
func (s *Service) CloudFormationRestore(ctx context.Context, b *Snapshot, expected string) (*Snapshot, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if b == nil {
		return nil, invalid("Snapshot is required")
	}
	b = common.Clone(b)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var r *Snapshot
		var err error
		if b.User != nil {
			r, err = readUser(tx, scope, b.ServerID, common.Value(b.User.UserName))
		} else {
			r, err = readServer(tx, scope, b.ServerID)
		}
		if err != nil {
			return err
		}
		if expected == "" || r.Revision != expected || r.Incarnation != b.Incarnation {
			return invalid("Compensation revision or incarnation changed")
		}
		b.Revision = revision()
		return save(tx, scope, b)
	})
	if e != nil {
		return nil, e
	}
	return b, nil
}
