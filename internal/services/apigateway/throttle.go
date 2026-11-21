package apigateway

import (
	"context"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
)

type throttleKey struct {
	scope                request.Scope
	apiID, stage, method string
	planID, apiKeyID     string
	stageCreated         float64
}

type throttleLimit struct {
	rate  *float64
	burst *int64
	key   throttleKey
}

func checkThrottle(rate *float64, burst *int64) error {
	r := common.Value(rate)
	if r < 0 || math.IsNaN(r) || math.IsInf(r, 0) || common.Value(burst) < 0 {
		return badRequest("Throttle limits must be finite and nonnegative.")
	}

	return nil
}

func (s *Service) GetAccount(ctx context.Context, _ *api.GetAccountRequest) (*api.Account, error) {
	if _, err := request.Regional(ctx); err != nil {
		return nil, err
	}

	return &api.Account{ThrottleSettings: &api.ThrottleSettings{
		RateLimit: new(proxy.DefaultThrottleRate), BurstLimit: new(int64(proxy.DefaultThrottleBurst)),
	}}, nil
}

func (c *call) throttleLimits() []throttleLimit {
	resource := common.Value(c.resource.Path)
	method := common.Value(c.method.HttpMethod)
	settings := c.stage.MethodSettings[strings.TrimPrefix(resource, "/")+"/"+method]
	defaults := c.stage.MethodSettings["*/*"]
	rate := common.OrElse(settings.ThrottlingRateLimit, defaults.ThrottlingRateLimit)
	burst := common.OrElse(settings.ThrottlingBurstLimit, defaults.ThrottlingBurstLimit)

	var limits []throttleLimit
	if rate != nil || burst != nil {
		limits = append(limits, throttleLimit{
			key: throttleKey{
				scope: c.scope, apiID: *c.api.API.Id, stage: *c.stage.StageName,
				method: resource + "/" + method, stageCreated: common.Value(c.stage.CreatedDate),
			},
			rate: rate, burst: burst,
		})
	}

	return append(limits, throttleLimit{key: throttleKey{scope: c.scope}})
}

// takeThrottle updates all buckets together only when all of them allow the request.
// The caller holds runtimeMu and commits the returned buckets after any store write.
func (s *Service) takeThrottle(now time.Time, limits []throttleLimit) ([]proxy.Bucket, bool) {
	buckets := make([]proxy.Bucket, len(limits))
	for i := range limits {
		limit := &limits[i]
		rate, burst := proxy.DefaultThrottleRate, float64(proxy.DefaultThrottleBurst)
		if limit.rate != nil {
			rate = *limit.rate
		}

		if limit.burst != nil {
			burst = float64(*limit.burst)
		}

		var allowed bool
		buckets[i], allowed = s.buckets[limit.key].Take(now, rate, burst)
		if !allowed {
			return nil, false
		}
	}

	return buckets, true
}

func throttled() *execute.Response {
	return execute.Message(http.StatusTooManyRequests, "Too Many Requests")
}

func (s *Service) checkThrottle(c *call) *execute.Response {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	limits := c.throttleLimits()
	buckets, allowed := s.takeThrottle(s.now(), limits)
	if !allowed {
		return throttled()
	}

	for i := range limits {
		s.buckets[limits[i].key] = buckets[i]
	}

	return nil
}
