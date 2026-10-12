package xray

import (
	"context"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func defaultRuleRecord(scope request.Scope) samplingRuleRecord {
	// The built-in rule predates every custom rule; AWS reports epoch timestamps until it is updated.
	ruleArn := samplingRuleARN(scope.Region, scope.AccountID, defaultRuleName)
	return samplingRuleRecord{
		Record: &api.SamplingRuleRecord{
			SamplingRule: &api.SamplingRule{
				RuleName:      new(defaultRuleName),
				RuleARN:       new(ruleArn),
				Priority:      new(defaultRulePriority),
				FixedRate:     new(defaultRuleFixedRate),
				ReservoirSize: new(defaultRuleReservoir),
				ServiceName:   new("*"),
				ServiceType:   new("*"),
				Host:          new("*"),
				HTTPMethod:    new("*"),
				URLPath:       new("*"),
				Version:       new(defaultRuleVersion),
				ResourceARN:   new("*"),
			},
			CreatedAt:  new(float64(0)),
			ModifiedAt: new(float64(0)),
		},
		Tags: map[string]string{},
	}
}

func (s *Service) CreateSamplingRule(
	ctx context.Context,
	in *api.CreateSamplingRuleRequest,
) (*api.CreateSamplingRuleResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.SamplingRule == nil {
		return nil, invalid("SamplingRule is required")
	}

	rule := in.SamplingRule
	if rule.RuleName != nil && *rule.RuleName == defaultRuleName {
		return nil, invalid("Cannot create a rule with name Default")
	}

	if checkErr := checkSamplingRule(rule, false); checkErr != nil {
		return nil, checkErr
	}

	tags, tagErr := tagMap(in.Tags)
	if tagErr != nil {
		return nil, tagErr
	}

	if checkErr := tagging.CheckSet(tags); checkErr != nil {
		return nil, checkTagLimit(checkErr)
	}

	ruleArn := samplingRuleARN(scope.Region, scope.AccountID, *rule.RuleName)
	now := common.Unix(time.Now())

	ruleCopy := *rule
	ruleCopy.RuleARN = new(ruleArn)

	record := &api.SamplingRuleRecord{
		SamplingRule: &ruleCopy,
		CreatedAt:    new(now),
		ModifiedAt:   new(now),
	}

	rec := samplingRuleRecord{
		Record: record,
		Tags:   tags,
	}

	key := scope.Key(serviceName, samplingRuleKind, *rule.RuleName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		prefix := scope.Prefix(serviceName, samplingRuleKind)
		customCount := 0
		for _, k := range tx.Keys(prefix) {
			if k != scope.Key(serviceName, samplingRuleKind, defaultRuleName) {
				customCount++
			}
		}

		if customCount >= maxCustomRules {
			return ruleLimitExceeded("Maximum number of custom sampling rules reached")
		}

		_, found, readErr := storage.Read[samplingRuleRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if found {
			return invalid("SamplingRule already exists with name: %s", *rule.RuleName)
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateSamplingRuleResult{
		SamplingRuleRecord: record,
	}, nil
}

func (s *Service) GetSamplingRules(
	ctx context.Context,
	in *api.GetSamplingRulesRequest,
) (*api.GetSamplingRulesResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := scope.Prefix(serviceName, samplingRuleKind)
	var list []samplingRuleRecord
	defaultFound := false

	if viewErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[samplingRuleRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, r := range records {
			if r != nil {
				if r.Record != nil && r.Record.SamplingRule != nil &&
					r.Record.SamplingRule.RuleName != nil && *r.Record.SamplingRule.RuleName == defaultRuleName {
					defaultFound = true
				}

				list = append(list, *r)
			}
		}

		return nil
	}); viewErr != nil {
		return nil, viewErr
	}

	if !defaultFound {
		list = append(list, defaultRuleRecord(scope))
	}

	// Rules are returned in evaluation order; the cursor key sorts the same way so Page resumes after deletions.
	cursor := func(r samplingRuleRecord) string {
		return fmt.Sprintf("%05d/%s", common.Value(r.Record.SamplingRule.Priority), *r.Record.SamplingRule.RuleName)
	}
	sort.Slice(list, func(i, j int) bool { return cursor(list[i]) < cursor(list[j]) })

	var token *string
	if in != nil {
		token = in.NextToken
	}

	page, nextToken, err := common.Page(list, cursor, token, 50, invalid("Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	var results []api.SamplingRuleRecord
	for _, item := range page {
		results = append(results, *item.Record)
	}

	return &api.GetSamplingRulesResult{
		SamplingRuleRecords: results,
		NextToken:           nextToken,
	}, nil
}

func (s *Service) UpdateSamplingRule(
	ctx context.Context,
	in *api.UpdateSamplingRuleRequest,
) (*api.UpdateSamplingRuleResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.SamplingRuleUpdate == nil {
		return nil, invalid("SamplingRuleUpdate is required")
	}

	u := in.SamplingRuleUpdate
	if (u.RuleName == nil || *u.RuleName == "") && (u.RuleARN == nil || *u.RuleARN == "") {
		return nil, invalid("RuleName or RuleARN is required")
	}

	var updatedRecord *api.SamplingRuleRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, key, isDefault, findErr := findSamplingRule(tx, scope, u.RuleName, u.RuleARN)
		if findErr != nil {
			return findErr
		}

		rule := rec.Record.SamplingRule
		if isDefault && (u.Priority != nil || u.Host != nil || u.ServiceName != nil || u.ServiceType != nil ||
			u.HTTPMethod != nil || u.URLPath != nil || u.ResourceARN != nil || u.Attributes != nil) {
			return invalid("Only FixedRate and ReservoirSize can be changed on the Default rule")
		}

		candidate := *rule
		if u.Priority != nil {
			candidate.Priority = u.Priority
		}

		if u.FixedRate != nil {
			candidate.FixedRate = u.FixedRate
		}

		if u.ReservoirSize != nil {
			candidate.ReservoirSize = u.ReservoirSize
		}

		for _, field := range []struct{ from, to **string }{
			{&u.Host, &candidate.Host},
			{&u.ServiceName, &candidate.ServiceName},
			{&u.ServiceType, &candidate.ServiceType},
			{&u.HTTPMethod, &candidate.HTTPMethod},
			{&u.URLPath, &candidate.URLPath},
			{&u.ResourceARN, &candidate.ResourceARN},
		} {
			if *field.from != nil {
				*field.to = *field.from
			}
		}

		if u.Attributes != nil {
			candidate.Attributes = u.Attributes
		}

		if checkErr := checkSamplingRule(&candidate, isDefault); checkErr != nil {
			return checkErr
		}

		*rule = candidate

		now := common.Unix(time.Now())
		rec.Record.ModifiedAt = new(now)
		updatedRecord = rec.Record

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateSamplingRuleResult{
		SamplingRuleRecord: updatedRecord,
	}, nil
}

func (s *Service) DeleteSamplingRule(
	ctx context.Context,
	in *api.DeleteSamplingRuleRequest,
) (*api.DeleteSamplingRuleResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || ((in.RuleName == nil || *in.RuleName == "") && (in.RuleARN == nil || *in.RuleARN == "")) {
		return nil, invalid("RuleName or RuleARN is required")
	}

	var deletedRecord *api.SamplingRuleRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, key, isDefault, findErr := findSamplingRule(tx, scope, in.RuleName, in.RuleARN)
		if findErr != nil {
			return findErr
		}

		if isDefault {
			return invalid("Cannot delete the Default rule")
		}

		deletedRecord = rec.Record
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSamplingRuleResult{
		SamplingRuleRecord: deletedRecord,
	}, nil
}

func findSamplingRule(
	tx storage.ReadTx,
	scope request.Scope,
	name *string,
	arn *string,
) (record *samplingRuleRecord, key string, isDefault bool, err error) {
	if name != nil && *name != "" {
		if *name == defaultRuleName {
			key := scope.Key(serviceName, samplingRuleKind, defaultRuleName)
			rec, found, readErr := storage.Read[samplingRuleRecord](tx, key)
			if readErr != nil {
				return nil, "", true, readErr
			}

			if !found || rec == nil {
				def := defaultRuleRecord(scope)
				return &def, key, true, nil
			}

			return rec, key, true, nil
		}

		key := scope.Key(serviceName, samplingRuleKind, *name)
		rec, found, readErr := storage.Read[samplingRuleRecord](tx, key)
		if readErr != nil {
			return nil, "", false, readErr
		}

		if !found || rec == nil {
			return nil, "", false, notFound("SamplingRule with name %s not found", *name)
		}

		return rec, key, false, nil
	}

	if arn != nil && *arn != "" {
		defaultArn := samplingRuleARN(scope.Region, scope.AccountID, defaultRuleName)
		if *arn == defaultArn {
			key := scope.Key(serviceName, samplingRuleKind, defaultRuleName)
			rec, found, readErr := storage.Read[samplingRuleRecord](tx, key)
			if readErr != nil {
				return nil, "", true, readErr
			}

			if !found || rec == nil {
				def := defaultRuleRecord(scope)
				return &def, key, true, nil
			}

			return rec, key, true, nil
		}

		prefix := scope.Prefix(serviceName, samplingRuleKind)
		for _, key := range tx.Keys(prefix) {
			rec, found, readErr := storage.Read[samplingRuleRecord](tx, key)
			if readErr != nil {
				return nil, "", false, readErr
			}

			if found && rec != nil && rec.Record != nil && rec.Record.SamplingRule != nil &&
				rec.Record.SamplingRule.RuleARN != nil && *rec.Record.SamplingRule.RuleARN == *arn {
				return rec, key, false, nil
			}
		}

		return nil, "", false, notFound("SamplingRule with ARN %s not found", *arn)
	}

	return nil, "", false, invalid("RuleName or RuleARN must be specified")
}

func checkSamplingRule(rule *api.SamplingRule, isDefault bool) error {
	if rule.RuleName == nil || *rule.RuleName == "" {
		return invalid("RuleName cannot be empty")
	}

	if utf8.RuneCountInString(*rule.RuleName) > 32 {
		return invalid("RuleName length must not exceed 32 characters")
	}

	if rule.Priority == nil {
		return invalid("Priority is required")
	}

	if isDefault {
		if *rule.Priority != defaultRulePriority {
			return invalid("Priority for Default rule must be 10000")
		}
	} else {
		if *rule.Priority < 1 || *rule.Priority > 9999 {
			return invalid("Priority must be between 1 and 9999 for custom rules")
		}
	}

	if rule.FixedRate == nil || *rule.FixedRate < 0.0 || *rule.FixedRate > 1.0 {
		return invalid("FixedRate must be between 0.0 and 1.0")
	}

	if rule.ReservoirSize == nil || *rule.ReservoirSize < 0 {
		return invalid("ReservoirSize must be non-negative")
	}

	for _, field := range []struct {
		value *string
		name  string
		max   int
	}{
		{rule.ServiceName, "ServiceName", 64},
		{rule.ServiceType, "ServiceType", 64},
		{rule.Host, "Host", 64},
		{rule.HTTPMethod, "HTTPMethod", 10},
		{rule.URLPath, "URLPath", 128},
		{rule.ResourceARN, "ResourceARN", 500},
	} {
		if field.value == nil || *field.value == "" || utf8.RuneCountInString(*field.value) > field.max {
			return invalid("%s must contain 1-%d characters", field.name, field.max)
		}
	}

	if len(rule.Attributes) > 5 {
		return invalid("Attributes cannot contain more than 5 entries")
	}

	for k, v := range rule.Attributes {
		if k == "" || v == "" || utf8.RuneCountInString(k) > 32 || utf8.RuneCountInString(v) > 32 {
			return invalid("Attribute keys and values must contain 1-32 characters")
		}
	}

	if rule.Version == nil || *rule.Version != 1 {
		return invalid("Version must be 1")
	}

	return nil
}

func samplingRuleARN(region, account, name string) string {
	return fmt.Sprintf("arn:aws:xray:%s:%s:sampling-rule/%s", region, account, name)
}
