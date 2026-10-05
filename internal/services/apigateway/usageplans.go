package apigateway

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// API keys are "…/apigateway/apikey/<ID>", found by value through
// "…/apigateway/apikey-value/<value>", and usage plans, with the IDs of their
// keys, are "…/apigateway/usageplan/<ID>". A method with apiKeyRequired
// answers 403 unless the request's key (the x-api-key header, or the
// authorizer's usageIdentifierKey) is enabled and in a usage plan of the
// API's stage. Throttles and quotas are stored, never enforced.

const (
	kindAPIKey      = "apikey"
	kindAPIKeyValue = "apikey-value"
	kindUsagePlan   = "usageplan"
	keyTypeAPIKey   = "API_KEY"
	minKeyValue     = 20
)

// usagePlan is a usage plan's record.
type usagePlan struct {
	Plan api.UsagePlan
	Keys []string
}

func apiKeyNotFound() error { return notFound("Invalid API Key identifier specified") }

func usagePlanNotFound() error { return notFound("Invalid Usage Plan ID specified") }

func loadAPIKey(tx storage.ReadTx, scope request.Scope, id string) (*api.ApiKey, error) {
	k, found, err := read[api.ApiKey](tx, prefix(scope, kindAPIKey)+id)
	if err == nil && !found {
		err = apiKeyNotFound()
	}

	return k, err
}

func loadUsagePlan(tx storage.ReadTx, scope request.Scope, id string) (*usagePlan, error) {
	p, found, err := read[usagePlan](tx, prefix(scope, kindUsagePlan)+id)
	if err == nil && !found {
		err = usagePlanNotFound()
	}

	return p, err
}

// shown returns an API key as reported, with its value only when asked.
func shown(k *api.ApiKey, withValue bool) *api.ApiKey {
	out := *k
	if !withValue {
		out.Value = nil
	}

	return &out
}

func (s *Service) CreateApiKey(ctx context.Context, in *api.CreateApiKeyRequest) (*api.ApiKey, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	keyValue := value(in.Value)
	switch {
	case in.Value == nil:
		keyValue = newID(40)
	case len(keyValue) < minKeyValue || len(keyValue) > 128:
		return nil, badRequest("API Key value should be at least 20 characters")
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	now := seconds(s.now())
	k := &api.ApiKey{
		Id: new(newID(10)), Value: &keyValue, Name: in.Name, CustomerId: in.CustomerId, Description: in.Description,
		Enabled: new(value(in.Enabled)), CreatedDate: now, LastUpdatedDate: now, Tags: cloneTags(in.Tags),
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		valueKey := prefix(m.Scope, kindAPIKeyValue) + keyValue
		if _, exists := tx.Get(valueKey); exists {
			return conflict("API Key already exists")
		}

		tx.Put(valueKey, []byte(*k.Id))
		return write(tx, prefix(m.Scope, kindAPIKey)+*k.Id, k)
	})
	if err != nil {
		return nil, err
	}

	return k, nil
}

func (s *Service) GetApiKey(ctx context.Context, in *api.GetApiKeyRequest) (*api.ApiKey, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.ApiKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		k, loadErr := loadAPIKey(tx, m.Scope, value(in.ApiKey))
		if loadErr == nil {
			out = shown(k, value(in.IncludeValue))
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// GetApiKeys lists the caller's API keys in ID order, optionally those whose
// name starts with nameQuery or that belong to a customer.
func (s *Service) GetApiKeys(ctx context.Context, in *api.GetApiKeysRequest) (*api.ApiKeys, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var keys []*api.ApiKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := records[api.ApiKey](tx, prefix(m.Scope, kindAPIKey))
		for _, k := range all {
			if strings.HasPrefix(value(k.Name), value(in.NameQuery)) &&
				(in.CustomerId == nil || value(k.CustomerId) == *in.CustomerId) {
				keys = append(keys, shown(k, value(in.IncludeValues)))
			}
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(keys, func(k *api.ApiKey) string { return *k.Id }, in.Position, in.Limit)
	if err != nil {
		return nil, err
	}

	out := &api.ApiKeys{Items: []api.ApiKey{}, Position: next}
	for _, k := range selected {
		out.Items = append(out.Items, *k)
	}

	return out, nil
}

func (s *Service) UpdateApiKey(ctx context.Context, in *api.UpdateApiKeyRequest) (*api.ApiKey, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.ApiKey
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, loadErr := loadAPIKey(tx, m.Scope, value(in.ApiKey))
		if loadErr != nil {
			return loadErr
		}

		if patchErr := patch(k, in.PatchOperations, "name", "description", "enabled", "customerId"); patchErr != nil {
			return patchErr
		}

		k.LastUpdatedDate = seconds(s.now())
		out = shown(k, false)
		return write(tx, prefix(m.Scope, kindAPIKey)+*k.Id, k)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteApiKey deletes an API key and takes it out of its usage plans.
func (s *Service) DeleteApiKey(ctx context.Context, in *api.DeleteApiKeyRequest) (*api.DeleteApiKeyOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, loadErr := loadAPIKey(tx, m.Scope, value(in.ApiKey))
		if loadErr != nil {
			return loadErr
		}

		for _, pk := range tx.Keys(prefix(m.Scope, kindUsagePlan)) {
			p, _, readErr := read[usagePlan](tx, pk)
			if readErr != nil {
				return readErr
			}

			if slices.Contains(p.Keys, *k.Id) {
				p.Keys = slices.DeleteFunc(p.Keys, func(id string) bool { return id == *k.Id })
				if writeErr := write(tx, pk, p); writeErr != nil {
					return writeErr
				}
			}
		}

		tx.Delete(prefix(m.Scope, kindAPIKeyValue) + value(k.Value))
		tx.Delete(prefix(m.Scope, kindAPIKey) + *k.Id)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteApiKeyOutput{}, nil
}

// checkStages checks that a usage plan's API stages exist.
func checkStages(tx storage.ReadTx, scope request.Scope, stages []api.ApiStage) error {
	for _, st := range stages {
		if _, found := tx.Get(owned(scope, kindStage, value(st.ApiId), value(st.Stage))); !found {
			return badRequest("Invalid stage identifier specified: %s:%s", value(st.ApiId), value(st.Stage))
		}
	}

	return nil
}

func checkLimits(throttle *api.ThrottleSettings, quota *api.QuotaSettings) error {
	if throttle != nil && (value(throttle.BurstLimit) < 0 || value(throttle.RateLimit) < 0) {
		return badRequest("Throttle limits must not be negative.")
	}

	if quota != nil {
		switch value(quota.Period) {
		case "DAY", "WEEK", "MONTH":
		default:
			return badRequest("Quota period must be DAY, WEEK, or MONTH.")
		}

		if value(quota.Limit) < 0 || value(quota.Offset) < 0 {
			return badRequest("Quota limit and offset must not be negative.")
		}
	}

	return nil
}

func (s *Service) CreateUsagePlan(ctx context.Context, in *api.CreateUsagePlanRequest) (*api.UsagePlan, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if value(in.Name) == "" {
		return nil, badRequest("Usage plan name is required.")
	}

	if limitErr := checkLimits(in.Throttle, in.Quota); limitErr != nil {
		return nil, limitErr
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	p := &usagePlan{Plan: api.UsagePlan{
		Id: new(newID(6)), Name: in.Name, Description: in.Description, ApiStages: in.ApiStages,
		Throttle: in.Throttle, Quota: in.Quota, Tags: cloneTags(in.Tags),
	}, Keys: []string{}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if stageErr := checkStages(tx, m.Scope, p.Plan.ApiStages); stageErr != nil {
			return stageErr
		}

		return write(tx, prefix(m.Scope, kindUsagePlan)+*p.Plan.Id, p)
	})
	if err != nil {
		return nil, err
	}

	return &p.Plan, nil
}

func (s *Service) GetUsagePlan(ctx context.Context, in *api.GetUsagePlanRequest) (*api.UsagePlan, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.UsagePlan
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, value(in.UsagePlanId))
		if loadErr == nil {
			out = &p.Plan
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// GetUsagePlans lists the caller's usage plans in ID order, optionally those
// with an API key.
func (s *Service) GetUsagePlans(ctx context.Context, in *api.GetUsagePlansRequest) (*api.UsagePlans, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var plans []*usagePlan
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := records[usagePlan](tx, prefix(m.Scope, kindUsagePlan))
		for _, p := range all {
			if in.KeyId == nil || slices.Contains(p.Keys, *in.KeyId) {
				plans = append(plans, p)
			}
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(plans, func(p *usagePlan) string { return *p.Plan.Id }, in.Position, in.Limit)
	if err != nil {
		return nil, err
	}

	out := &api.UsagePlans{Items: []api.UsagePlan{}, Position: next}
	for _, p := range selected {
		out.Items = append(out.Items, p.Plan)
	}

	return out, nil
}

// UpdateUsagePlan patches a usage plan: its name, description, throttle, and
// quota, and "/apiStages", which adds or removes "<API ID>:<stage>".
func (s *Service) UpdateUsagePlan(ctx context.Context, in *api.UpdateUsagePlanRequest) (*api.UsagePlan, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var stageOps, otherOps []api.PatchOperation
	for _, op := range in.PatchOperations {
		if value(op.Path) == "/apiStages" {
			stageOps = append(stageOps, op)
		} else {
			otherOps = append(otherOps, op)
		}
	}

	var out *api.UsagePlan
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		if patchErr := patch(&p.Plan, otherOps, "name", "description", "throttle", "quota"); patchErr != nil {
			return patchErr
		}

		for _, op := range stageOps {
			apiID, stage, ok := strings.Cut(value(op.Value), ":")
			if !ok {
				return badRequest("API stages are given as <API ID>:<stage>.")
			}

			st := api.ApiStage{ApiId: &apiID, Stage: &stage}
			p.Plan.ApiStages = slices.DeleteFunc(p.Plan.ApiStages, func(a api.ApiStage) bool {
				return value(a.ApiId) == apiID && value(a.Stage) == stage
			})
			switch value(op.Op) {
			case opAdd:
				if stageErr := checkStages(tx, m.Scope, []api.ApiStage{st}); stageErr != nil {
					return stageErr
				}

				p.Plan.ApiStages = append(p.Plan.ApiStages, st)
			case opRemove:
			default:
				return badRequest("API stages can only be added or removed.")
			}
		}

		if limitErr := checkLimits(p.Plan.Throttle, p.Plan.Quota); limitErr != nil {
			return limitErr
		}

		out = &p.Plan
		return write(tx, prefix(m.Scope, kindUsagePlan)+*p.Plan.Id, p)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteUsagePlan(
	ctx context.Context,
	in *api.DeleteUsagePlanRequest,
) (*api.DeleteUsagePlanOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadUsagePlan(tx, m.Scope, value(in.UsagePlanId)); loadErr != nil {
			return loadErr
		}

		tx.Delete(prefix(m.Scope, kindUsagePlan) + value(in.UsagePlanId))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUsagePlanOutput{}, nil
}

func planKey(k *api.ApiKey) *api.UsagePlanKey {
	return &api.UsagePlanKey{Id: k.Id, Type: new(keyTypeAPIKey), Value: k.Value, Name: k.Name}
}

func (s *Service) CreateUsagePlanKey(
	ctx context.Context,
	in *api.CreateUsagePlanKeyRequest,
) (*api.UsagePlanKey, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if value(in.KeyType) != keyTypeAPIKey {
		return nil, badRequest("keyType must be API_KEY.")
	}

	var out *api.UsagePlanKey
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		k, loadErr := loadAPIKey(tx, m.Scope, value(in.KeyId))
		if loadErr != nil {
			return loadErr
		}

		if slices.Contains(p.Keys, *k.Id) {
			return conflict("Usage Plan %s cannot be added because API Key %s is already in it", *p.Plan.Id, *k.Id)
		}

		p.Keys = append(p.Keys, *k.Id)
		out = planKey(k)
		return write(tx, prefix(m.Scope, kindUsagePlan)+*p.Plan.Id, p)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetUsagePlanKey(ctx context.Context, in *api.GetUsagePlanKeyRequest) (*api.UsagePlanKey, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.UsagePlanKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		if !slices.Contains(p.Keys, value(in.KeyId)) {
			return notFound("Invalid Usage Plan Key ID specified")
		}

		k, loadErr := loadAPIKey(tx, m.Scope, value(in.KeyId))
		if loadErr == nil {
			out = planKey(k)
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// GetUsagePlanKeys lists a usage plan's keys in ID order, optionally those
// whose name starts with nameQuery.
func (s *Service) GetUsagePlanKeys(
	ctx context.Context,
	in *api.GetUsagePlanKeysRequest,
) (*api.UsagePlanKeys, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var keys []*api.UsagePlanKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		for _, id := range slices.Sorted(slices.Values(p.Keys)) {
			k, keyErr := loadAPIKey(tx, m.Scope, id)
			if keyErr != nil {
				return keyErr
			}

			if strings.HasPrefix(value(k.Name), value(in.NameQuery)) {
				keys = append(keys, planKey(k))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(keys, func(k *api.UsagePlanKey) string { return *k.Id }, in.Position, in.Limit)
	if err != nil {
		return nil, err
	}

	out := &api.UsagePlanKeys{Items: []api.UsagePlanKey{}, Position: next}
	for _, k := range selected {
		out.Items = append(out.Items, *k)
	}

	return out, nil
}

func (s *Service) DeleteUsagePlanKey(
	ctx context.Context,
	in *api.DeleteUsagePlanKeyRequest,
) (*api.DeleteUsagePlanKeyOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		if !slices.Contains(p.Keys, value(in.KeyId)) {
			return notFound("Invalid Usage Plan Key ID specified")
		}

		p.Keys = slices.DeleteFunc(p.Keys, func(id string) bool { return id == value(in.KeyId) })
		return write(tx, prefix(m.Scope, kindUsagePlan)+*p.Plan.Id, p)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUsagePlanKeyOutput{}, nil
}

// checkAPIKey answers 403 unless a call that needs an API key has an enabled
// one in a usage plan of its API's stage: the x-api-key header's, or, for an
// API whose key source is AUTHORIZER, the authorizer's usageIdentifierKey.
func (s *Service) checkAPIKey(ctx context.Context, c *call, authorizer map[string]any) *execute.Response {
	if !value(c.method.ApiKeyRequired) {
		return nil
	}

	keyValue := c.req.Header.Get("X-Api-Key")
	if value(c.api.API.ApiKeySource) == "AUTHORIZER" {
		keyValue, _ = authorizer["usageIdentifierKey"].(string)
	}

	allowed := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		id, found := tx.Get(prefix(c.scope, kindAPIKeyValue) + keyValue)
		if keyValue == "" || !found {
			return nil
		}

		k, loadErr := loadAPIKey(tx, c.scope, string(id))
		if loadErr != nil || !value(k.Enabled) {
			return loadErr
		}

		plans, readErr := records[usagePlan](tx, prefix(c.scope, kindUsagePlan))
		for _, p := range plans {
			if slices.Contains(p.Keys, *k.Id) && slices.ContainsFunc(p.Plan.ApiStages, func(st api.ApiStage) bool {
				return value(st.ApiId) == *c.api.API.Id && value(st.Stage) == *c.stage.StageName
			}) {
				allowed = true
			}
		}

		return readErr
	})
	if err != nil || !allowed {
		return execute.Message(http.StatusForbidden, "Forbidden")
	}

	return nil
}
