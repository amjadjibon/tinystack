package resourcegroups

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroups"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// nameOf returns the group a tag operation's ARN names.
func (s *Service) nameOf(ctx context.Context, arn *string) (request.Metadata, string, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return m, "", err
	}

	if arn == nil {
		return m, "", bad("Arn is required.")
	}

	if !strings.HasPrefix(*arn, "arn:") {
		return m, "", bad("%q is not a valid group ARN.", *arn)
	}

	name, err := groupName(m.Scope, arn, nil)

	return m, name, err
}

func (s *Service) GetTags(ctx context.Context, in *api.GetTagsInput) (*api.GetTagsOutput, error) {
	m, name, err := s.nameOf(ctx, in.Arn)
	if err != nil {
		return nil, err
	}

	var g *group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		g, err = load(tx, m.Scope, name)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetTagsOutput{Arn: in.Arn, Tags: g.Tags}, nil
}

func (s *Service) Tag(ctx context.Context, in *api.TagInput) (*api.TagOutput, error) {
	if len(in.Tags) == 0 {
		return nil, bad("Tags must hold at least one tag.")
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, bad("%s", err.Error())
	}

	m, name, err := s.nameOf(ctx, in.Arn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, loadErr := load(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		if g.Tags, loadErr = tagging.Merge(g.Tags, in.Tags); loadErr != nil {
			return bad("%s", loadErr.Error())
		}

		return storage.Write(tx, m.Key(service, kindGroup, name), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagOutput{Arn: in.Arn, Tags: common.CloneTags(in.Tags)}, nil
}

func (s *Service) Untag(ctx context.Context, in *api.UntagInput) (*api.UntagOutput, error) {
	if len(in.Keys) == 0 {
		return nil, bad("Keys must hold at least one tag key.")
	}

	m, name, err := s.nameOf(ctx, in.Arn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, loadErr := load(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		g.Tags = tagging.Remove(g.Tags, in.Keys)

		return storage.Write(tx, m.Key(service, kindGroup, name), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagOutput{Arn: in.Arn, Keys: in.Keys}, nil
}

// accountKey is where an account's settings are kept in a Region.
func accountKey(scope request.Scope) string { return scope.Key(service, kindAccount, nameSetting) }

func (s *Service) settings(ctx context.Context, update *string) (*api.AccountSettings, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var current *settings
	if update == nil {
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			current, _, err = storage.Read[settings](tx, accountKey(m.Scope))
			return err
		})
	} else {
		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			current = &settings{Status: *update}
			return storage.Write(tx, accountKey(m.Scope), current)
		})
	}

	if err != nil {
		return nil, err
	}

	status := statusInactive
	if current != nil && current.Status != "" {
		status = current.Status
	}

	return &api.AccountSettings{
		GroupLifecycleEventsDesiredStatus: new(status), GroupLifecycleEventsStatus: new(status),
	}, nil
}

func (s *Service) GetAccountSettings(
	ctx context.Context, _ *api.GetAccountSettingsInput,
) (*api.GetAccountSettingsOutput, error) {
	out, err := s.settings(ctx, nil)
	if err != nil {
		return nil, err
	}

	return &api.GetAccountSettingsOutput{AccountSettings: out}, nil
}

func (s *Service) UpdateAccountSettings(
	ctx context.Context, in *api.UpdateAccountSettingsInput,
) (*api.UpdateAccountSettingsOutput, error) {
	if v := in.GroupLifecycleEventsDesiredStatus; v != nil && *v != statusActive && *v != statusInactive {
		return nil, bad("GroupLifecycleEventsDesiredStatus must be %s or %s.", statusActive, statusInactive)
	}

	out, err := s.settings(ctx, in.GroupLifecycleEventsDesiredStatus)
	if err != nil {
		return nil, err
	}

	return &api.UpdateAccountSettingsOutput{AccountSettings: out}, nil
}
