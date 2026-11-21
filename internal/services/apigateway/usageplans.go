package apigateway

import (
	"context"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// API keys are "…/apigateway/apikey/<ID>", found by value through
// "…/apigateway/apikey-value/<value>", and usage plans, with the IDs of their
// keys, are "…/apigateway/usageplan/<ID>". A method with apiKeyRequired
// answers 403 unless the request's key (the x-api-key header, or the
// authorizer's usageIdentifierKey) is enabled and in a usage plan of the
// API's stage.

const (
	kindAPIKey      = "apikey"
	kindAPIKeyValue = "apikey-value"
	kindUsagePlan   = "usageplan"
	keyTypeAPIKey   = "API_KEY"
	minKeyValue     = 20
)

// usagePlan is a usage plan's record.
type usagePlan struct {
	Joined map[string]time.Time
	Plan   api.UsagePlan
	Keys   []string
}

func apiKeyNotFound() error { return notFound("Invalid API Key identifier specified") }

func usagePlanNotFound() error { return notFound("Invalid Usage Plan ID specified") }

func loadAPIKey(tx storage.ReadTx, scope request.Scope, id string) (*api.ApiKey, error) {
	k, found, err := storage.Read[api.ApiKey](tx, scope.Prefix(service, kindAPIKey)+id)
	if err == nil && !found {
		err = apiKeyNotFound()
	}

	return k, err
}

func loadUsagePlan(tx storage.ReadTx, scope request.Scope, id string) (*usagePlan, error) {
	p, found, err := storage.Read[usagePlan](tx, scope.Prefix(service, kindUsagePlan)+id)
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	keyValue := common.Value(in.Value)
	switch {
	case in.Value == nil:
		keyValue = common.RandomID(40)
	case len(keyValue) < minKeyValue || len(keyValue) > 128:
		return nil, badRequest("API Key value should be at least 20 characters")
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	now := common.Seconds(s.now())
	k := &api.ApiKey{
		Id: new(
			common.RandomID(10),
		),
		Value:           &keyValue,
		Name:            in.Name,
		CustomerId:      in.CustomerId,
		Description:     in.Description,
		Enabled:         new(common.Value(in.Enabled)),
		CreatedDate:     now,
		LastUpdatedDate: now,
		Tags:            common.CloneTags(in.Tags),
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		valueKey := m.Prefix(service, kindAPIKeyValue) + keyValue
		if _, exists := tx.Get(valueKey); exists {
			return conflict("API Key already exists")
		}

		tx.Put(valueKey, []byte(*k.Id))
		return storage.Write(tx, m.Prefix(service, kindAPIKey)+*k.Id, k)
	})
	if err != nil {
		return nil, err
	}

	return k, nil
}

func (s *Service) GetApiKey(ctx context.Context, in *api.GetApiKeyRequest) (*api.ApiKey, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.ApiKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		k, loadErr := loadAPIKey(tx, m.Scope, common.Value(in.ApiKey))
		if loadErr == nil {
			out = shown(k, common.Value(in.IncludeValue))
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var keys []*api.ApiKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := storage.Records[api.ApiKey](tx, m.Prefix(service, kindAPIKey))
		for _, k := range all {
			if strings.HasPrefix(common.Value(k.Name), common.Value(in.NameQuery)) &&
				(in.CustomerId == nil || common.Value(k.CustomerId) == *in.CustomerId) {
				keys = append(keys, shown(k, common.Value(in.IncludeValues)))
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.ApiKey
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, loadErr := loadAPIKey(tx, m.Scope, common.Value(in.ApiKey))
		if loadErr != nil {
			return loadErr
		}

		if patchErr := patch(k, in.PatchOperations, "name", "description", "enabled", "customerId"); patchErr != nil {
			return patchErr
		}

		k.LastUpdatedDate = common.Seconds(s.now())
		out = shown(k, false)
		return storage.Write(tx, m.Prefix(service, kindAPIKey)+*k.Id, k)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteApiKey deletes an API key and takes it out of its usage plans.
func (s *Service) DeleteApiKey(ctx context.Context, in *api.DeleteApiKeyRequest) (*api.DeleteApiKeyOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, loadErr := loadAPIKey(tx, m.Scope, common.Value(in.ApiKey))
		if loadErr != nil {
			return loadErr
		}

		for _, pk := range tx.Keys(m.Prefix(service, kindUsagePlan)) {
			p, _, readErr := storage.Read[usagePlan](tx, pk)
			if readErr != nil {
				return readErr
			}

			if slices.Contains(p.Keys, *k.Id) || !p.Joined[*k.Id].IsZero() {
				p.Keys = slices.DeleteFunc(p.Keys, func(id string) bool { return id == *k.Id })
				delete(p.Joined, *k.Id)
				for _, key := range tx.Keys(usagePrefix(m.Scope, *p.Plan.Id, *k.Id)) {
					tx.Delete(key)
				}
				if writeErr := storage.Write(tx, pk, p); writeErr != nil {
					return writeErr
				}
			}
		}

		tx.Delete(m.Prefix(service, kindAPIKeyValue) + common.Value(k.Value))
		tx.Delete(m.Prefix(service, kindAPIKey) + *k.Id)
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
		if err := checkPlanStageThrottles(st); err != nil {
			return err
		}

		if _, found := tx.Get(owned(scope, kindStage, common.Value(st.ApiId), common.Value(st.Stage))); !found {
			return badRequest(
				"Invalid stage identifier specified: %s:%s",
				common.Value(st.ApiId),
				common.Value(st.Stage),
			)
		}
	}

	return nil
}

func checkLimits(throttle *api.ThrottleSettings, quota *api.QuotaSettings) error {
	if throttle != nil {
		if err := checkThrottle(throttle.RateLimit, throttle.BurstLimit); err != nil {
			return err
		}
	}

	if quota != nil {
		switch common.Value(quota.Period) {
		case "DAY", "WEEK", "MONTH":
		default:
			return badRequest("Quota period must be DAY, WEEK, or MONTH.")
		}

		if quota.Limit == nil || *quota.Limit < 0 || common.Value(quota.Offset) < 0 {
			return badRequest("Quota limit is required. The limit and offset must not be negative.")
		}
	}

	return nil
}

func (s *Service) CreateUsagePlan(ctx context.Context, in *api.CreateUsagePlanRequest) (*api.UsagePlan, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.Name) == "" {
		return nil, badRequest("Usage plan name is required.")
	}

	if limitErr := checkLimits(in.Throttle, in.Quota); limitErr != nil {
		return nil, limitErr
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	p := &usagePlan{Plan: api.UsagePlan{
		Id: new(common.RandomID(6)), Name: in.Name, Description: in.Description, ApiStages: in.ApiStages,
		Throttle: in.Throttle, Quota: in.Quota, Tags: common.CloneTags(in.Tags),
	}, Keys: []string{}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if stageErr := checkStages(tx, m.Scope, p.Plan.ApiStages); stageErr != nil {
			return stageErr
		}

		return storage.Write(tx, m.Prefix(service, kindUsagePlan)+*p.Plan.Id, p)
	})
	if err != nil {
		return nil, err
	}

	return &p.Plan, nil
}

func (s *Service) GetUsagePlan(ctx context.Context, in *api.GetUsagePlanRequest) (*api.UsagePlan, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.UsagePlan
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, common.Value(in.UsagePlanId))
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var plans []*usagePlan
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := storage.Records[usagePlan](tx, m.Prefix(service, kindUsagePlan))
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

// checkUsagePlanMembership enforces one plan per API key and stage.
func checkUsagePlanMembership(tx storage.ReadTx, scope request.Scope, plan *usagePlan) error {
	if len(plan.Keys) == 0 || len(plan.Plan.ApiStages) == 0 {
		return nil
	}

	plans, err := storage.Records[usagePlan](tx, scope.Prefix(service, kindUsagePlan))
	if err != nil {
		return err
	}

	for _, other := range plans {
		if *other.Plan.Id == *plan.Plan.Id || !slices.ContainsFunc(other.Keys, func(id string) bool {
			return slices.Contains(plan.Keys, id)
		}) {
			continue
		}

		for _, stage := range plan.Plan.ApiStages {
			if findPlanStage(other, common.Value(stage.ApiId), common.Value(stage.Stage)) >= 0 {
				return conflict("An API key can belong to only one usage plan for an API stage.")
			}
		}
	}

	return nil
}

func findPlanStage(p *usagePlan, apiID, stage string) int {
	return slices.IndexFunc(p.Plan.ApiStages, func(st api.ApiStage) bool {
		return common.Value(st.ApiId) == apiID && common.Value(st.Stage) == stage
	})
}

func patchUsagePlan(tx storage.ReadTx, scope request.Scope, p *usagePlan, op api.PatchOperation) error {
	parts := pathSegments(common.Value(op.Path))
	if len(parts) == 0 || parts[0] != "apiStages" {
		return patch(&p.Plan, []api.PatchOperation{op}, "name", "description", "throttle", "quota")
	}

	if len(parts) == 1 {
		apiID, stage, ok := strings.Cut(common.Value(op.Value), ":")
		if !ok {
			return badRequest("API stages are given as <API ID>:<stage>.")
		}

		index := findPlanStage(p, apiID, stage)
		switch common.Value(op.Op) {
		case opAdd:
			if index < 0 {
				st := api.ApiStage{ApiId: new(apiID), Stage: new(stage)}
				if err := checkStages(tx, scope, []api.ApiStage{st}); err != nil {
					return err
				}

				p.Plan.ApiStages = append(p.Plan.ApiStages, st)
			}
		case opRemove:
			if index >= 0 {
				p.Plan.ApiStages = slices.Delete(p.Plan.ApiStages, index, index+1)
			}
		default:
			return badRequest("API stages can only be added or removed.")
		}

		return nil
	}

	apiID, stage, _ := strings.Cut(parts[1], ":")
	index := findPlanStage(p, apiID, stage)
	if len(parts) < 4 || parts[2] != "throttle" || index < 0 {
		return badRequest("Invalid usage plan stage throttle path: %s", common.Value(op.Path))
	}

	return patchPlanStageThrottle(&p.Plan.ApiStages[index], parts[3:], op)
}

func patchPlanStageThrottle(st *api.ApiStage, parts []string, op api.PatchOperation) error {
	field := parts[len(parts)-1]
	hasField := field == "rateLimit" || field == "burstLimit"
	if hasField {
		parts = parts[:len(parts)-1]
	}

	method := strings.Join(parts, "/")
	if !strings.HasPrefix(method, "/") || strings.Count(method, "/") == 1 {
		method = "/" + method
	}

	if !validThrottleMethod(method) {
		return badRequest("Method throttle keys must use <resource path>/<HTTP method>.")
	}

	if !hasField {
		if common.Value(op.Op) != opRemove {
			return badRequest("Invalid method throttle path: %s", common.Value(op.Path))
		}

		delete(st.Throttle, method)
		return nil
	}

	config := st.Throttle[method]
	op.Path = new("/" + field)
	if err := patch(&config, []api.PatchOperation{op}, field); err != nil {
		return err
	}

	if st.Throttle == nil {
		st.Throttle = make(map[string]api.ThrottleSettings)
	}

	if config.RateLimit == nil && config.BurstLimit == nil {
		delete(st.Throttle, method)
	} else {
		st.Throttle[method] = config
	}

	return nil
}

func validThrottleMethod(key string) bool {
	i := strings.LastIndexByte(key, '/')
	return i > 0 && key[0] == '/' && methods[key[i+1:]]
}

func checkPlanStageThrottles(st api.ApiStage) error {
	for path, config := range st.Throttle {
		if !validThrottleMethod(path) {
			return badRequest("Method throttle keys must use <resource path>/<HTTP method>.")
		}

		if err := checkLimits(&config, nil); err != nil {
			return err
		}

		if config.BurstLimit == nil && config.RateLimit == nil {
			return badRequest("A method throttle needs a rate or burst limit.")
		}
	}

	return nil
}

// UpdateUsagePlan patches a usage plan: its name, description, throttle, and
// quota, and "/apiStages", which adds or removes "<API ID>:<stage>".
func (s *Service) UpdateUsagePlan(ctx context.Context, in *api.UpdateUsagePlanRequest) (*api.UsagePlan, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.UsagePlan
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, common.Value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		for _, op := range in.PatchOperations {
			if patchErr := patchUsagePlan(tx, m.Scope, p, op); patchErr != nil {
				return patchErr
			}
		}

		for _, stage := range p.Plan.ApiStages {
			if throttleErr := checkPlanStageThrottles(stage); throttleErr != nil {
				return throttleErr
			}
		}

		if membershipErr := checkUsagePlanMembership(tx, m.Scope, p); membershipErr != nil {
			return membershipErr
		}

		if limitErr := checkLimits(p.Plan.Throttle, p.Plan.Quota); limitErr != nil {
			return limitErr
		}

		out = &p.Plan
		return storage.Write(tx, m.Prefix(service, kindUsagePlan)+*p.Plan.Id, p)
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadUsagePlan(tx, m.Scope, common.Value(in.UsagePlanId)); loadErr != nil {
			return loadErr
		}

		id := common.Value(in.UsagePlanId)
		for _, key := range tx.Keys(m.Prefix(service, kindUsage) + id + "/") {
			tx.Delete(key)
		}

		tx.Delete(m.Prefix(service, kindUsagePlan) + id)
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.KeyType) != keyTypeAPIKey {
		return nil, badRequest("keyType must be API_KEY.")
	}

	var out *api.UsagePlanKey
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, common.Value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		k, loadErr := loadAPIKey(tx, m.Scope, common.Value(in.KeyId))
		if loadErr != nil {
			return loadErr
		}

		if slices.Contains(p.Keys, *k.Id) {
			return conflict("Usage Plan %s cannot be added because API Key %s is already in it", *p.Plan.Id, *k.Id)
		}

		p.Keys = append(p.Keys, *k.Id)
		if membershipErr := checkUsagePlanMembership(tx, m.Scope, p); membershipErr != nil {
			return membershipErr
		}

		if p.Joined == nil {
			p.Joined = make(map[string]time.Time)
		}

		if p.Joined[*k.Id].IsZero() {
			p.Joined[*k.Id] = s.now()
		}
		out = planKey(k)
		return storage.Write(tx, m.Prefix(service, kindUsagePlan)+*p.Plan.Id, p)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetUsagePlanKey(ctx context.Context, in *api.GetUsagePlanKeyRequest) (*api.UsagePlanKey, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.UsagePlanKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, common.Value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		if !slices.Contains(p.Keys, common.Value(in.KeyId)) {
			return notFound("Invalid Usage Plan Key ID specified")
		}

		k, loadErr := loadAPIKey(tx, m.Scope, common.Value(in.KeyId))
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var keys []*api.UsagePlanKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, common.Value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		for _, id := range slices.Sorted(slices.Values(p.Keys)) {
			k, keyErr := loadAPIKey(tx, m.Scope, id)
			if keyErr != nil {
				return keyErr
			}

			if strings.HasPrefix(common.Value(k.Name), common.Value(in.NameQuery)) {
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadUsagePlan(tx, m.Scope, common.Value(in.UsagePlanId))
		if loadErr != nil {
			return loadErr
		}

		if !slices.Contains(p.Keys, common.Value(in.KeyId)) {
			return notFound("Invalid Usage Plan Key ID specified")
		}

		p.Keys = slices.DeleteFunc(p.Keys, func(id string) bool { return id == common.Value(in.KeyId) })
		return storage.Write(tx, m.Prefix(service, kindUsagePlan)+*p.Plan.Id, p)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUsagePlanKeyOutput{}, nil
}
