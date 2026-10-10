package appconfig

import (
	"context"
	"fmt"
	"strconv"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appconfig"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func versionID(n int64) string { return fmt.Sprintf("%020d", n) }
func hostedOutput(h *hosted) *api.HostedConfigurationVersion {
	return &api.HostedConfigurationVersion{
		ApplicationId:          new(h.ApplicationID),
		ConfigurationProfileId: new(h.ProfileID),
		VersionNumber: new(
			h.Number,
		),
		Description:  new(h.Description),
		Content:      h.Content,
		ContentType:  new(h.ContentType),
		VersionLabel: new(h.Label),
	}
}

func (s *Service) CreateHostedConfigurationVersion(
	ctx context.Context,
	in *api.CreateHostedConfigurationVersionRequest,
) (
	*api.HostedConfigurationVersion, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.ContentType) == "" {
		return nil, bad("ContentType is required")
	}

	var out *api.HostedConfigurationVersion
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		app, profile := common.Value(in.ApplicationId), common.Value(in.ConfigurationProfileId)
		p, e := read(tx, scope, kindProfile, app, "", profile)
		if e != nil {
			return e
		}

		if in.LatestVersionNumber != nil && *in.LatestVersionNumber != p.Counter {
			return conflict("LatestVersionNumber mismatch")
		}

		for _, k := range tx.Keys(scope.Key(serviceName, kindVersion, app+"/"+profile+"/")) {
			r, _, readErr := storage.Read[record](tx, k)
			if readErr != nil {
				return readErr
			}

			if in.VersionLabel != nil && r.Hosted.Label == *in.VersionLabel {
				return conflict("VersionLabel already exists")
			}
		}

		p.Counter++
		h := &hosted{
			ApplicationID: app, ProfileID: profile, Number: p.Counter, Description: common.Value(in.Description),
			ContentType: *in.ContentType, Label: common.Value(in.VersionLabel), Content: in.Content,
		}

		if saveErr := save(tx, scope, p); saveErr != nil {
			return saveErr
		}

		r := &record{
			ID:            versionID(h.Number),
			Name:          strconv.FormatInt(h.Number, 10),
			Kind:          kindVersion,
			ApplicationID: app,
			Parent:        profile,
			Hosted:        h,
		}
		out = hostedOutput(h)
		return save(tx, scope, r)
	})
	return out, err
}

func (s *Service) GetHostedConfigurationVersion(ctx context.Context, in *api.GetHostedConfigurationVersionRequest) (
	*api.HostedConfigurationVersion, error,
) {
	r, err := s.get(ctx, kindVersion, common.Value(in.ApplicationId), common.Value(in.ConfigurationProfileId),
		versionID(common.Value(in.VersionNumber)))
	if err != nil {
		return nil, err
	}

	return hostedOutput(r.Hosted), nil
}

func (s *Service) DeleteHostedConfigurationVersion(
	ctx context.Context,
	in *api.DeleteHostedConfigurationVersionRequest,
) (
	*api.DeleteHostedConfigurationVersionOutput, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	app, profile, id := common.Value(in.ApplicationId), common.Value(in.ConfigurationProfileId),
		versionID(common.Value(in.VersionNumber))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := read(tx, scope, kindVersion, app, profile, id); e != nil {
			return e
		}

		for _, k := range tx.Keys(scope.Key(serviceName, kindEnvironment, app+"//")) {
			r, _, readErr := storage.Read[record](tx, k)
			if readErr != nil {
				return readErr
			}

			if r.Active[profile] == id {
				return conflict("Version is deployed")
			}
		}

		tx.Delete(key(scope, kindVersion, app, profile, id))
		return nil
	})
	return &api.DeleteHostedConfigurationVersionOutput{}, err
}
