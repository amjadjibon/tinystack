package appsync

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	day          = 24 * time.Hour
	keyLifetime  = 7 * day
	keyRetention = 60 * day
)

// expiry is when a key expires: the time expires names (by default a week
// from now), rounded down to the hour, which must be a day to a year away.
func (s *Service) expiry(expires *int64) (time.Time, error) {
	now := s.now()
	at := now.Add(keyLifetime)
	if common.Value(expires) != 0 {
		at = time.Unix(*expires, 0)
	}

	at = at.Truncate(time.Hour)
	if at.Before(now.Add(day)) || at.After(now.Add(365*day)) {
		return time.Time{}, awserror.New("ApiKeyValidityOutOfBoundsException",
			"API key must be valid for a minimum of 1 day and a maximum of 365 days.")
	}

	return at, nil
}

func setExpiry(k *api.ApiKey, at time.Time) {
	k.Expires, k.Deletes = new(at.Unix()), new(at.Add(keyRetention).Unix())
}

func loadAPIKey(tx storage.ReadTx, scope request.Scope, apiID, id *string) (*api.ApiKey, string, error) {
	key := owned(scope, kindAPIKey, common.Value(apiID), common.Value(id))
	k, ok, err := storage.Read[api.ApiKey](tx, key)
	if err != nil || !ok {
		return nil, "", common.ErrorsOr(err, notFound("API key %s not found.", common.Value(id)))
	}

	return k, key, nil
}

func (s *Service) CreateApiKey(ctx context.Context, in *api.CreateApiKeyRequest) (*api.CreateApiKeyResponse, error) {
	at, err := s.expiry(in.Expires)
	if err != nil {
		return nil, err
	}

	k := &api.ApiKey{Id: new("da2-" + common.RandomID(26)), Description: in.Description}
	setExpiry(k, at)
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if loadErr := keyAPI(tx, scope, in.ApiId); loadErr != nil {
			return loadErr
		}

		return storage.Write(tx, owned(scope, kindAPIKey, *in.ApiId, *k.Id), k)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateApiKeyResponse{ApiKey: k}, nil
}

func (s *Service) ListApiKeys(ctx context.Context, in *api.ListApiKeysRequest) (*api.ListApiKeysResponse, error) {
	out := &api.ListApiKeysResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if loadErr := keyAPI(tx, scope, in.ApiId); loadErr != nil {
			return loadErr
		}

		all, readErr := records[api.ApiKey](tx, owned(scope, kindAPIKey, *in.ApiId, ""))
		if readErr != nil {
			return readErr
		}

		// Keys past their deletion date are gone.
		live := all[:0]
		for _, k := range all {
			if s.now().Unix() < common.Value(k.Deletes) {
				live = append(live, k)
			}
		}

		out.ApiKeys, out.NextToken, readErr = page(live, func(k api.ApiKey) string { return *k.Id }, in.NextToken,
			in.MaxResults)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateApiKey(ctx context.Context, in *api.UpdateApiKeyRequest) (*api.UpdateApiKeyResponse, error) {
	out := &api.UpdateApiKeyResponse{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		k, key, loadErr := loadAPIKey(tx, scope, in.ApiId, in.Id)
		if loadErr != nil {
			return loadErr
		}

		if in.Expires != nil {
			at, expiryErr := s.expiry(in.Expires)
			if expiryErr != nil {
				return expiryErr
			}

			setExpiry(k, at)
		}

		k.Description, out.ApiKey = common.OrElse(in.Description, k.Description), k
		return storage.Write(tx, key, k)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteApiKey(ctx context.Context, in *api.DeleteApiKeyRequest) (*api.DeleteApiKeyResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		_, key, loadErr := loadAPIKey(tx, scope, in.ApiId, in.Id)
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteApiKeyResponse{}, nil
}
