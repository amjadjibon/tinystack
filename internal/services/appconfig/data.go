package appconfig

import (
	"context"
	"time"

	data "github.com/amjadjibon/tinystack/internal/aws/generated/appconfigdata"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// DataService exposes polling without duplicating control-plane inventory.
type DataService struct{ store storage.Store }

func (s *Service) Data() *DataService { return &DataService{store: s.store} }

var _ data.API = (*DataService)(nil)

type session struct {
	Expires                                 time.Time
	Application, Environment, Profile, Seen string
	Interval                                int64
}

func resolve(tx storage.ReadTx, scope request.Scope, kind, app, identifier string) (*record, error) {
	for _, k := range tx.Keys(scope.Key(serviceName, kind, app+"//")) {
		r, _, err := storage.Read[record](tx, k)
		if err != nil {
			return nil, err
		}

		if r.ID == identifier || r.Name == identifier {
			return r, nil
		}
	}

	return nil, missing()
}

func issue(tx storage.WriteTx, scope request.Scope, t *session) (*string, error) {
	token := common.RandomID(64)
	t.Expires = time.Now().Add(24 * time.Hour)
	err := storage.Write(tx, scope.Key(serviceName, "token", token), t)
	return new(token), err
}

func (s *DataService) StartConfigurationSession(ctx context.Context, in *data.StartConfigurationSessionRequest) (
	*data.StartConfigurationSessionResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	interval := common.Value(common.OrElse(in.RequiredMinimumPollIntervalInSeconds, new(int64(60))))
	if interval < 15 || interval > 86400 {
		return nil, bad("Poll interval must be between 15 and 86400 seconds")
	}

	var token *string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		app, e := resolve(tx, scope, kindApplication, "", common.Value(in.ApplicationIdentifier))
		if e != nil {
			return e
		}

		env, e := resolve(tx, scope, kindEnvironment, app.ID, common.Value(in.EnvironmentIdentifier))
		if e != nil {
			return e
		}

		profile, e := resolve(tx, scope, kindProfile, app.ID, common.Value(in.ConfigurationProfileIdentifier))
		if e != nil {
			return e
		}

		// Opportunistic expiry keeps abandoned sessions from accumulating forever.
		for _, k := range tx.Keys(scope.Prefix(serviceName, "token")) {
			t, _, readErr := storage.Read[session](tx, k)
			if readErr != nil {
				return readErr
			}

			if time.Now().After(t.Expires) {
				tx.Delete(k)
			}
		}

		token, e = issue(
			tx,
			scope,
			&session{Application: app.ID, Environment: env.ID, Profile: profile.ID, Interval: interval},
		)
		return e
	})
	return &data.StartConfigurationSessionResponse{InitialConfigurationToken: token}, err
}

func (s *DataService) GetLatestConfiguration(ctx context.Context, in *data.GetLatestConfigurationRequest) (
	*data.GetLatestConfigurationResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &data.GetLatestConfigurationResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := scope.Key(serviceName, "token", common.Value(in.ConfigurationToken))
		t, found, e := storage.Read[session](tx, k)
		if e != nil {
			return e
		}

		if !found || time.Now().After(t.Expires) {
			return bad("Configuration token is invalid or expired")
		}

		env, e := read(tx, scope, kindEnvironment, t.Application, "", t.Environment)
		if e != nil {
			return e
		}

		current := env.Active[t.Profile]
		if current != "" && current != t.Seen {
			v, versionErr := read(tx, scope, kindVersion, t.Application, t.Profile, current)
			if versionErr != nil {
				return versionErr
			}

			out.Configuration = v.Hosted.Content
			out.ContentType = new(v.Hosted.ContentType)
			out.VersionLabel = new(v.Hosted.Label)
		}

		t.Seen = current
		tx.Delete(k)
		out.NextPollIntervalInSeconds = new(t.Interval)
		out.NextPollConfigurationToken, e = issue(tx, scope, t)
		return e
	})
	return out, err
}
