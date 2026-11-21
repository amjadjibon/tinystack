package apigateway

import (
	"context"
	"maps"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type stageCacheKey struct {
	scope        request.Scope
	apiID, stage string
	created      float64
}

func (s *Service) stageAuthorizerCache(c *call) *proxy.AuthorizerCache {
	key := stageCacheKey{c.scope, *c.api.API.Id, *c.stage.StageName, common.Value(c.stage.CreatedDate)}
	cache := s.authorizerCaches[key]
	if cache == nil {
		cache = new(proxy.AuthorizerCache)
		s.authorizerCaches[key] = cache
	}

	return cache
}

func (s *Service) FlushStageAuthorizersCache(
	ctx context.Context, in *api.FlushStageAuthorizersCacheRequest,
) (*api.FlushStageAuthorizersCacheOutput, error) {
	var key stageCacheKey
	err := s.viewAPI(ctx, in.RestApiId, func(tx storage.ReadTx, scope request.Scope, a *restAPI) error {
		st, err := loadStage(tx, scope, *a.API.Id, in.StageName)
		if err != nil {
			return err
		}

		key = stageCacheKey{scope, *a.API.Id, *st.StageName, common.Value(st.CreatedDate)}
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.runtimeMu.Lock()
	delete(s.authorizerCaches, key)
	s.runtimeMu.Unlock()
	return &api.FlushStageAuthorizersCacheOutput{}, nil
}

func (s *Service) discardStageRuntime(scope request.Scope, apiID string, stage *string) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	maps.DeleteFunc(s.authorizerCaches, func(key stageCacheKey, _ *proxy.AuthorizerCache) bool {
		return key.scope == scope && key.apiID == apiID && (stage == nil || *stage == key.stage)
	})
	maps.DeleteFunc(s.buckets, func(key throttleKey, _ proxy.Bucket) bool {
		return key.scope == scope && key.apiID == apiID && (stage == nil || *stage == key.stage)
	})
}

// Reset clears transient throttles and cached authorizer responses.
func (s *Service) Reset(context.Context) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	clear(s.buckets)
	clear(s.authorizerCaches)
}
