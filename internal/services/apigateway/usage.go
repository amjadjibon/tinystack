package apigateway

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindUsage = "usage"

func usagePrefix(scope request.Scope, planID, keyID string) string {
	return owned(scope, kindUsage, planID, keyID) + "/"
}

func quotaStart(day time.Time, quota *api.QuotaSettings) time.Time {
	if quota != nil {
		switch common.Value(quota.Period) {
		case "WEEK":
			return day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
		case "MONTH":
			return day.AddDate(0, 0, 1-day.Day())
		}
	}

	return day
}

func quotaLimit(p *usagePlan, key string, window time.Time) int64 {
	q := p.Plan.Quota
	if q == nil {
		return 0
	}

	limit := common.Value(q.Limit)
	if joined := p.Joined[key]; !joined.IsZero() && window.Equal(quotaStart(joined.UTC().Truncate(24*time.Hour), q)) {
		limit = max(0, limit-common.Value(q.Offset))
	}

	return limit
}

func dailyUsage(tx storage.ReadTx, prefix string, day time.Time) (int64, error) {
	used, _, err := storage.Read[int64](tx, prefix+day.Format(time.DateOnly))
	return common.Value(used), err
}

func usedBefore(tx storage.ReadTx, prefix string, start, end time.Time) (int64, error) {
	var used int64
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		count, err := dailyUsage(tx, prefix, day)
		if err != nil {
			return 0, err
		}

		used += count
	}

	return used, nil
}

func (s *Service) GetUsage(ctx context.Context, in *api.GetUsageRequest) (*api.Usage, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	start, startErr := time.Parse(time.DateOnly, common.Value(in.StartDate))
	end, endErr := time.Parse(time.DateOnly, common.Value(in.EndDate))
	if startErr != nil || endErr != nil || end.Before(start) || end.Sub(start) >= 366*24*time.Hour {
		return nil, badRequest("Start and end dates must be YYYY-MM-DD and span at most 366 days.")
	}

	out := &api.Usage{
		UsagePlanId: in.UsagePlanId, StartDate: in.StartDate, EndDate: in.EndDate, Items: map[string][][]int64{},
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, common.Value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		keys := slices.Sorted(slices.Values(p.Keys))
		if in.KeyId != nil {
			if !slices.Contains(keys, *in.KeyId) {
				return notFound("Invalid Usage Plan Key ID specified")
			}

			keys = []string{*in.KeyId}
		}

		selected, next, pageErr := page(keys, func(id string) string { return id }, in.Position, in.Limit)
		if pageErr != nil {
			return pageErr
		}

		out.Position = next
		for _, key := range selected {
			log, logErr := usageLog(tx, m.Scope, p, key, start, end)
			if logErr != nil {
				return logErr
			}

			out.Items[key] = log
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func usageLog(
	tx storage.ReadTx,
	scope request.Scope,
	p *usagePlan,
	key string,
	start, end time.Time,
) ([][]int64, error) {
	prefix := usagePrefix(scope, *p.Plan.Id, key)
	window := quotaStart(start, p.Plan.Quota)
	total, err := usedBefore(tx, prefix, window, start)
	if err != nil {
		return nil, err
	}

	var log [][]int64
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		if next := quotaStart(day, p.Plan.Quota); !next.Equal(window) {
			window, total = next, 0
		}

		used, err := dailyUsage(tx, prefix, day)
		if err != nil {
			return nil, err
		}

		total += used
		log = append(log, []int64{used, max(0, quotaLimit(p, key, window)-total)})
	}

	return log, nil
}

// checkAPIKey first finds an enabled key for the stage, then admits the request
// against its plan and stage buckets and records one use in the same transaction.
func (s *Service) checkAPIKey(ctx context.Context, c *call) *execute.Response {
	if !common.Value(c.method.ApiKeyRequired) {
		return s.checkThrottle(c)
	}

	value := c.req.Header.Get("X-Api-Key")
	if common.Value(c.api.API.ApiKeySource) == "AUTHORIZER" {
		value = c.usageKey
	}

	forbidden := execute.Message(http.StatusForbidden, "Forbidden")
	if value == "" {
		return forbidden
	}

	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()

	now := s.now()
	limits := c.throttleLimits()
	var buckets []proxy.Bucket
	denied := forbidden
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		id, found := tx.Get(c.scope.Prefix(service, kindAPIKeyValue) + value)
		if !found {
			return nil
		}

		key, err := loadAPIKey(tx, c.scope, string(id))
		if err != nil || !common.Value(key.Enabled) {
			return err
		}

		plans, err := storage.Records[usagePlan](tx, c.scope.Prefix(service, kindUsagePlan))
		if err != nil {
			return err
		}

		for _, plan := range plans {
			if !slices.Contains(plan.Keys, *key.Id) {
				continue
			}

			if index := findPlanStage(plan, *c.api.API.Id, *c.stage.StageName); index >= 0 {
				limits = append(planLimits(c, plan, plan.Plan.ApiStages[index], *key.Id), limits...)
				buckets, denied, err = s.admitKey(tx, c.scope, plan, *key.Id, limits, now)
				return err
			}
		}

		return nil
	})
	if err != nil {
		return execute.Message(http.StatusInternalServerError, "Internal Server Error")
	}

	for i, bucket := range buckets {
		s.buckets[limits[i].key] = bucket
	}

	return denied
}

func planLimits(c *call, plan *usagePlan, stage api.ApiStage, keyID string) []throttleLimit {
	var limits []throttleLimit
	key := throttleKey{scope: c.scope, planID: *plan.Plan.Id, apiKeyID: keyID}
	method := common.Value(c.resource.Path) + "/" + common.Value(c.method.HttpMethod)
	if config, ok := stage.Throttle[method]; ok {
		methodKey := key
		methodKey.apiID, methodKey.stage, methodKey.method = *c.api.API.Id, *c.stage.StageName, method
		methodKey.stageCreated = common.Value(c.stage.CreatedDate)
		limits = append(limits, throttleLimit{key: methodKey, rate: config.RateLimit, burst: config.BurstLimit})
	}

	if config := plan.Plan.Throttle; config != nil && (config.RateLimit != nil || config.BurstLimit != nil) {
		limits = append(limits, throttleLimit{key: key, rate: config.RateLimit, burst: config.BurstLimit})
	}

	return limits
}

func (s *Service) admitKey(
	tx storage.WriteTx, scope request.Scope, p *usagePlan, key string, limits []throttleLimit, now time.Time,
) ([]proxy.Bucket, *execute.Response, error) {
	day := now.UTC().Truncate(24 * time.Hour)
	prefix := usagePrefix(scope, *p.Plan.Id, key)
	count, err := dailyUsage(tx, prefix, day)
	if err != nil {
		return nil, nil, err
	}

	if p.Plan.Quota != nil {
		start := quotaStart(day, p.Plan.Quota)
		previous, err := usedBefore(tx, prefix, start, day)
		if err != nil {
			return nil, nil, err
		}

		if previous+count >= quotaLimit(p, key, start) {
			return nil, execute.Message(http.StatusTooManyRequests, "Limit Exceeded"), nil
		}
	}

	buckets, allowed := s.takeThrottle(now, limits)
	if !allowed {
		return nil, throttled(), nil
	}

	if err := storage.Write(tx, prefix+day.Format(time.DateOnly), new(count+1)); err != nil {
		return nil, nil, err
	}

	return buckets, nil, nil
}
