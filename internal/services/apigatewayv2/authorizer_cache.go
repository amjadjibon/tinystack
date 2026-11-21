package apigatewayv2

import (
	"context"
	"crypto/sha256"
	"maps"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

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

// cachedAuthorizer keeps deployment and identity tuple boundaries separate.
func (s *Service) cachedAuthorizer(c *call, authorizerID string, sources []string,
) (cache *proxy.AuthorizerCache, cacheKey [sha256.Size]byte, payload []byte) {
	cacheKey = proxy.AuthorizerCacheKey(*c.stage.DeploymentId, authorizerID, sources)

	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	key := c.runtimeKey()
	cache = s.authorizerCaches[key]
	if cache == nil {
		cache = new(proxy.AuthorizerCache)
		s.authorizerCaches[key] = cache
	}

	payload = cache.Get(cacheKey, s.now())
	return cache, cacheKey, payload
}

func (s *Service) cacheAuthorizer(
	cache *proxy.AuthorizerCache,
	key [sha256.Size]byte,
	payload []byte,
	ttl time.Duration,
) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	cache.Put(key, payload, s.now(), ttl)
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
	maps.DeleteFunc(
		s.authorizerCaches,
		func(key stageRuntimeKey, _ *proxy.AuthorizerCache) bool { return matches(key) },
	)
	maps.DeleteFunc(s.buckets, func(key stageRuntimeKey, _ map[string]proxy.Bucket) bool { return matches(key) })
}
