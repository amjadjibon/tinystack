package apigatewayv2

import (
	"math"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	defaultThrottleBurst = 5000
	defaultThrottleRate  = 10000.0
)

type throttleBucket struct {
	at                  time.Time
	rate, burst, tokens float64
}

func checkThrottling(settings *api.RouteSettings) error {
	if settings == nil {
		return nil
	}

	if common.Value(settings.ThrottlingBurstLimit) < 0 {
		return badRequest("ThrottlingBurstLimit must be greater than or equal to 0.")
	}

	rate := common.Value(settings.ThrottlingRateLimit)
	if rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return badRequest("ThrottlingRateLimit must be a finite number greater than or equal to 0.")
	}

	return nil
}

// routeSettings inherits defaults independently for each setting.
func (c *call) routeSettings() api.RouteSettings {
	settings := c.stage.RouteSettings[*c.route.RouteKey]
	defaults := common.Value(c.stage.DefaultRouteSettings)
	settings.ThrottlingBurstLimit = common.OrElse(settings.ThrottlingBurstLimit, defaults.ThrottlingBurstLimit)
	settings.ThrottlingRateLimit = common.OrElse(settings.ThrottlingRateLimit, defaults.ThrottlingRateLimit)
	settings.DetailedMetricsEnabled = common.OrElse(settings.DetailedMetricsEnabled, defaults.DetailedMetricsEnabled)
	return settings
}

func (s *Service) allowRequest(c *call) bool {
	settings := c.routeSettings()
	if settings.ThrottlingBurstLimit == nil && settings.ThrottlingRateLimit == nil {
		return true
	}

	rate, burst := defaultThrottleRate, float64(defaultThrottleBurst)
	if settings.ThrottlingRateLimit != nil {
		rate = *settings.ThrottlingRateLimit
	}

	if settings.ThrottlingBurstLimit != nil {
		burst = float64(*settings.ThrottlingBurstLimit)
	}

	if burst == 0 {
		return false
	}

	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	now := s.now()
	key := c.runtimeKey()
	routes := s.buckets[key]
	if routes == nil {
		routes = map[string]throttleBucket{}
		s.buckets[key] = routes
	}

	route := *c.route.RouteKey
	b, ok := routes[route]
	if !ok || b.burst != burst || b.rate != rate {
		b = throttleBucket{at: now, rate: rate, burst: burst, tokens: burst}
	}

	if now.After(b.at) {
		b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
		b.at = now
	}

	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}

	routes[route] = b
	return allowed
}
