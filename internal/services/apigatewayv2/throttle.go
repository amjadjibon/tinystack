package apigatewayv2

import (
	"math"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
)

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

	rate, burst := proxy.DefaultThrottleRate, float64(proxy.DefaultThrottleBurst)
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
		routes = map[string]proxy.Bucket{}
		s.buckets[key] = routes
	}

	route := *c.route.RouteKey
	b, allowed := routes[route].Take(now, rate, burst)
	routes[route] = b
	return allowed
}
