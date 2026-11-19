package apigatewayv2

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"maps"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const authorizerCacheSize = 1024

type stageRuntimeKey struct {
	created          time.Time
	apiID, stageName string
}

func (c *call) runtimeKey() stageRuntimeKey {
	return stageRuntimeKey{
		created:   common.Value(c.stage.CreatedDate),
		apiID:     *c.api.ApiId,
		stageName: *c.stage.StageName,
	}
}

type authorizerCache struct {
	entries map[[sha256.Size]byte]*list.Element
	recency list.List
}

type cachedAuthorization struct {
	expires time.Time
	payload []byte
	key     [sha256.Size]byte
}

// cachedAuthorizer keeps deployment and identity tuple boundaries separate.
func (s *Service) cachedAuthorizer(c *call, authorizerID string, sources []string,
) (cache *authorizerCache, cacheKey [sha256.Size]byte, payload []byte) {
	input, _ := json.Marshal(struct {
		Deployment, Authorizer string
		Sources                []string
	}{*c.stage.DeploymentId, authorizerID, sources})
	cacheKey = sha256.Sum256(input)

	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	key := c.runtimeKey()
	cache = s.authorizerCaches[key]
	if cache == nil {
		cache = &authorizerCache{entries: map[[sha256.Size]byte]*list.Element{}}
		s.authorizerCaches[key] = cache
	}

	if element := cache.entries[cacheKey]; element != nil {
		entry := element.Value.(cachedAuthorization)
		if s.now().Before(entry.expires) {
			cache.recency.MoveToFront(element)
			return cache, cacheKey, entry.payload
		}

		cache.recency.Remove(element)
		delete(cache.entries, cacheKey)
	}

	return cache, cacheKey, nil
}

func (s *Service) cacheAuthorizer(cache *authorizerCache, key [sha256.Size]byte, payload []byte, ttl time.Duration) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	// Keep the first result if concurrent requests missed the same entry.
	if cache.entries[key] != nil {
		return
	}

	if len(cache.entries) == authorizerCacheSize {
		removed := cache.recency.Remove(cache.recency.Back()).(cachedAuthorization)
		delete(cache.entries, removed.key)
	}

	cache.entries[key] = cache.recency.PushFront(cachedAuthorization{
		key: key, payload: payload, expires: s.now().Add(ttl),
	})
}

func (s *Service) ResetAuthorizersCache(
	ctx context.Context, in *api.ResetAuthorizersCacheRequest,
) (*api.ResetAuthorizersCacheOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var key stageRuntimeKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		if common.Value(a.ProtocolType) != protocolHTTP {
			return badRequest("Authorizer caching is only supported for HTTP APIs.")
		}

		stage, loadErr := loadStage(tx, m.Scope, *a.ApiId, in.StageName)
		if loadErr != nil {
			return loadErr
		}

		key = stageRuntimeKey{created: common.Value(stage.CreatedDate), apiID: *a.ApiId, stageName: *stage.StageName}
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.runtimeMu.Lock()
	delete(s.authorizerCaches, key)
	s.runtimeMu.Unlock()
	return &api.ResetAuthorizersCacheOutput{}, nil
}

// discardStageRuntime removes state after deleting an API or a stage.
func (s *Service) discardStageRuntime(apiID string, stageName *string) {
	matches := func(key stageRuntimeKey) bool {
		return key.apiID == apiID && (stageName == nil || key.stageName == *stageName)
	}

	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	maps.DeleteFunc(s.authorizerCaches, func(key stageRuntimeKey, _ *authorizerCache) bool { return matches(key) })
	maps.DeleteFunc(s.buckets, func(key stageRuntimeKey, _ map[string]throttleBucket) bool { return matches(key) })
}
