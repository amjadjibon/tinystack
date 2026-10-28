package elbv2

import (
	"cmp"
	"context"
	"net/netip"
	"regexp"
	"slices"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	fieldHost        = "host-header"
	fieldPath        = "path-pattern"
	fieldHeader      = "http-header"
	fieldMethod      = "http-request-method"
	fieldQuery       = "query-string"
	fieldSourceIP    = "source-ip"
	priorityDefault  = "default"
	maxPriority      = 50000
	maxRules         = 100
	maxConditionSize = 128
	maxConditionVals = 5
)

var methodPattern = regexp.MustCompile(`^[A-Z_-]+$`)

// rule is a listener rule's record. A listener's default rule has none: it
// is the listener's default actions.
type rule struct {
	Tags     map[string]string `json:",omitempty"`
	R        api.Rule
	ID       string
	Listener string
	Priority int64
}

func ruleNotFound() error {
	return awserror.New("RuleNotFoundException", "One or more rules not found")
}

// defaultRule is the rule a listener's default actions form.
func defaultRule(scope request.Scope, l *listener) api.Rule {
	return api.Rule{
		RuleArn: new(l.ruleARN(scope, l.RuleID)), Priority: new(priorityDefault), Conditions: []api.RuleCondition{},
		Actions: l.L.DefaultActions, IsDefault: new(true),
	}
}

// findRule returns the rule an ARN names and its listener; the rule is nil
// for the listener's default rule.
func findRule(tx storage.ReadTx, scope request.Scope, arnValue string) (*listener, *rule, error) {
	parts, ok := arnParts(arnValue, "listener-rule", scope)
	if !ok || len(parts) != 5 {
		return nil, nil, validation("'%s' is not a valid rule ARN", arnValue)
	}

	l, found, err := storage.Read[listener](tx, scope.Key(service, kindListener, parts[1]+"/"+parts[3]))
	switch {
	case err != nil:
		return nil, nil, err
	case !found || l.ruleARN(scope, parts[4]) != arnValue:
		return nil, nil, ruleNotFound()
	case parts[4] == l.RuleID:
		return l, nil, nil
	}

	r, found, err := storage.Read[rule](tx, scope.Key(service, kindRule, l.ID+"/"+parts[4]))
	switch {
	case err != nil:
		return nil, nil, err
	case !found:
		return nil, nil, ruleNotFound()
	}

	return l, r, nil
}

// listenerRules returns a listener's rules in priority order, its default
// rule last.
func listenerRules(tx storage.ReadTx, scope request.Scope, l *listener) ([]api.Rule, error) {
	rules, err := storage.Records[rule](tx, scope.Key(service, kindRule, l.ID+"/"))
	if err != nil {
		return nil, err
	}

	slices.SortFunc(rules, func(a, b *rule) int { return cmp.Compare(a.Priority, b.Priority) })
	out := make([]api.Rule, 0, len(rules)+1)
	for _, r := range rules {
		out = append(out, r.R)
	}

	return append(out, defaultRule(scope, l)), nil
}

// checkConditions checks a rule's conditions and returns them as ELB reports
// them: host and path conditions both as values and as a config.
func checkConditions(conditions []api.RuleCondition) ([]api.RuleCondition, error) {
	if len(conditions) == 0 {
		return nil, validation("A rule must have at least one condition")
	}

	seen := map[string]bool{}
	count := 0
	out := make([]api.RuleCondition, len(conditions))
	for i, c := range conditions {
		field := common.Value(c.Field)
		configs := 0
		for _, set := range []bool{
			c.HostHeaderConfig != nil, c.PathPatternConfig != nil, c.HttpHeaderConfig != nil,
			c.HttpRequestMethodConfig != nil, c.QueryStringConfig != nil, c.SourceIpConfig != nil,
		} {
			if set {
				configs++
			}
		}

		// Values may repeat a host or path config's values, as DescribeRules
		// reports them.
		legacy := c.Values
		if h, p := c.HostHeaderConfig, c.PathPatternConfig; (h != nil && slices.Equal(h.Values, legacy)) ||
			(p != nil && slices.Equal(p.Values, legacy)) {
			legacy = nil
		}

		if configs > 1 || (configs == 1 && len(legacy) > 0) {
			return nil, validation("A condition takes either values or one config that matches its field")
		}

		if seen[field] && field != fieldHeader && field != fieldQuery {
			return nil, validation("A rule can have only one '%s' condition", field)
		}

		seen[field] = true
		next := api.RuleCondition{Field: c.Field}
		var values []string
		switch field {
		case fieldHost, fieldPath:
			config := c.HostHeaderConfig
			if field == fieldPath {
				config = (*api.HostHeaderConditionConfig)(c.PathPatternConfig)
			}

			values = c.Values
			if config != nil {
				values = config.Values
			} else if configs > 0 {
				return nil, validation("The config of a '%s' condition does not match its field", field)
			}

			for _, v := range values {
				if v == "" || len(v) > maxConditionSize {
					return nil, validation("'%s' condition values are 1 to 128 characters", field)
				}
			}

			next.Values = values
			if field == fieldHost {
				next.HostHeaderConfig = &api.HostHeaderConditionConfig{Values: values}
			} else {
				next.PathPatternConfig = &api.PathPatternConditionConfig{Values: values}
			}
		case fieldHeader:
			h := c.HttpHeaderConfig
			if h == nil || common.Value(h.HttpHeaderName) == "" || len(common.Value(h.HttpHeaderName)) > 40 {
				return nil, validation("An http-header condition needs an HttpHeaderConfig with a header name")
			}

			values, next.HttpHeaderConfig = h.Values, h
		case fieldMethod:
			if c.HttpRequestMethodConfig == nil {
				return nil, validation("An http-request-method condition needs an HttpRequestMethodConfig")
			}

			values, next.HttpRequestMethodConfig = c.HttpRequestMethodConfig.Values, c.HttpRequestMethodConfig
			for _, v := range values {
				if !methodPattern.MatchString(v) || len(v) > 40 {
					return nil, validation("'%s' is not a valid HTTP request method", v)
				}
			}
		case fieldQuery:
			if c.QueryStringConfig == nil {
				return nil, validation("A query-string condition needs a QueryStringConfig")
			}

			next.QueryStringConfig = c.QueryStringConfig
			for _, p := range c.QueryStringConfig.Values {
				if common.Value(p.Value) == "" {
					return nil, validation("Each query-string condition pair needs a value")
				}

				values = append(values, common.Value(p.Value))
			}
		case fieldSourceIP:
			if c.SourceIpConfig == nil {
				return nil, validation("A source-ip condition needs a SourceIpConfig")
			}

			values, next.SourceIpConfig = c.SourceIpConfig.Values, c.SourceIpConfig
			for _, v := range values {
				if _, err := netip.ParsePrefix(v); err != nil {
					return nil, validation("'%s' is not a valid CIDR block", v)
				}
			}
		default:
			return nil, validation("Condition field '%s' is not supported", field)
		}

		if len(values) == 0 {
			return nil, validation("A '%s' condition needs at least one value", field)
		}

		count += len(values)
		out[i] = next
	}

	if count > maxConditionVals {
		return nil, validation("A rule can have at most 5 condition values")
	}

	return out, nil
}

func checkPriority(priority int64) error {
	if priority < 1 || priority > maxPriority {
		return validation("Priority must be 1 to 50000")
	}

	return nil
}

func priorityInUse() error {
	return awserror.New("PriorityInUseException", "The specified priority is in use")
}

func (s *Service) CreateRule(ctx context.Context, in *api.CreateRuleInput) (*api.CreateRuleOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	priority := common.Value(in.Priority)
	if checkErr := checkPriority(priority); checkErr != nil {
		return nil, checkErr
	}

	conditions, err := checkConditions(in.Conditions)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	var r *rule
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		l, findErr := findListener(tx, m.Scope, common.Value(in.ListenerArn))
		if findErr != nil {
			return findErr
		}

		b, found, readErr := storage.Read[balancer](tx, m.Key(service, kindLB, l.Balancer))
		switch {
		case readErr != nil:
			return readErr
		case !found:
			return listenerNotFound()
		case b.kind() != typeApplication:
			return validation("Rules are only supported by application load balancer listeners")
		}

		actions, actionsErr := checkActions(tx, m.Scope, b, common.Value(l.L.Protocol), in.Actions)
		if actionsErr != nil {
			return actionsErr
		}

		existing, listErr := storage.Records[rule](tx, m.Key(service, kindRule, l.ID+"/"))
		switch {
		case listErr != nil:
			return listErr
		case slices.ContainsFunc(existing, func(o *rule) bool { return o.Priority == priority }):
			return priorityInUse()
		case len(existing) >= maxRules:
			return awserror.New("TooManyRulesException", "A listener can have at most 100 rules")
		}

		id := resourceID()
		r = &rule{Tags: tags, ID: id, Listener: l.ID, Priority: priority, R: api.Rule{
			RuleArn: new(l.ruleARN(m.Scope, id)), Priority: new(strconv.FormatInt(priority, 10)),
			Conditions: conditions, Actions: actions, IsDefault: new(false),
		}}
		return storage.Write(tx, m.Key(service, kindRule, l.ID+"/"+id), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateRuleOutput{Rules: []api.Rule{r.R}}, nil
}

func (s *Service) DescribeRules(ctx context.Context, in *api.DescribeRulesInput) (*api.DescribeRulesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if (in.ListenerArn == nil) == (len(in.RuleArns) == 0) {
		return nil, validation("You must specify either rule ARNs or a listener ARN")
	}

	var selected []api.Rule
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.ListenerArn != nil {
			l, findErr := findListener(tx, m.Scope, *in.ListenerArn)
			if findErr != nil {
				return findErr
			}

			var listErr error
			selected, listErr = listenerRules(tx, m.Scope, l)
			return listErr
		}

		for _, a := range in.RuleArns {
			l, r, findErr := findRule(tx, m.Scope, a)
			switch {
			case findErr != nil:
				return findErr
			case r == nil:
				selected = append(selected, defaultRule(m.Scope, l))
			default:
				selected = append(selected, r.R)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(
		selected,
		func(r api.Rule) string { return common.Value(r.RuleArn) },
		in.Marker,
		in.PageSize,
	)
	if err != nil {
		return nil, err
	}

	return &api.DescribeRulesOutput{Rules: append([]api.Rule{}, selected...), NextMarker: next}, nil
}

func defaultRuleChange() error {
	return awserror.New("OperationNotPermittedException", "The default rule cannot be changed this way; "+
		"modify the listener's default actions instead")
}

func (s *Service) ModifyRule(ctx context.Context, in *api.ModifyRuleInput) (*api.ModifyRuleOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var conditions []api.RuleCondition
	if in.Conditions != nil {
		if conditions, err = checkConditions(in.Conditions); err != nil {
			return nil, err
		}
	}

	var r *rule
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		l, found, findErr := findRule(tx, m.Scope, common.Value(in.RuleArn))
		switch {
		case findErr != nil:
			return findErr
		case found == nil:
			return defaultRuleChange()
		}

		r = found
		if conditions != nil {
			r.R.Conditions = conditions
		}

		if in.Actions != nil {
			b, ok, readErr := storage.Read[balancer](tx, m.Key(service, kindLB, l.Balancer))
			switch {
			case readErr != nil:
				return readErr
			case !ok:
				return ruleNotFound()
			}

			actions, actionsErr := checkActions(tx, m.Scope, b, common.Value(l.L.Protocol), in.Actions)
			if actionsErr != nil {
				return actionsErr
			}

			r.R.Actions = actions
		}

		return storage.Write(tx, m.Key(service, kindRule, r.Listener+"/"+r.ID), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyRuleOutput{Rules: []api.Rule{r.R}}, nil
}

func (s *Service) DeleteRule(ctx context.Context, in *api.DeleteRuleInput) (*api.DeleteRuleOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	return &api.DeleteRuleOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, r, findErr := findRule(tx, m.Scope, common.Value(in.RuleArn))
		switch {
		case findErr != nil:
			return findErr
		case r == nil:
			return defaultRuleChange()
		}

		tx.Delete(m.Key(service, kindRule, r.Listener+"/"+r.ID))
		return nil
	})
}

func (s *Service) SetRulePriorities(
	ctx context.Context, in *api.SetRulePrioritiesInput,
) (*api.SetRulePrioritiesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.RulePriorities) == 0 {
		return nil, validation("At least one rule priority must be specified")
	}

	out := &api.SetRulePrioritiesOutput{Rules: []api.Rule{}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		changed := map[string]*rule{}
		var order []string
		for _, p := range in.RulePriorities {
			if checkErr := checkPriority(common.Value(p.Priority)); checkErr != nil {
				return checkErr
			}

			_, r, findErr := findRule(tx, m.Scope, common.Value(p.RuleArn))
			switch {
			case findErr != nil:
				return findErr
			case r == nil:
				return defaultRuleChange()
			}

			if previous, ok := changed[r.ID]; ok {
				r = previous
			} else {
				order = append(order, r.ID)
			}

			r.Priority = common.Value(p.Priority)
			r.R.Priority = new(strconv.FormatInt(r.Priority, 10))
			changed[r.ID] = r
			if writeErr := storage.Write(tx, m.Key(service, kindRule, r.Listener+"/"+r.ID), r); writeErr != nil {
				return writeErr
			}
		}

		for _, r := range changed {
			rules, listErr := storage.Records[rule](tx, m.Key(service, kindRule, r.Listener+"/"))
			if listErr != nil {
				return listErr
			}

			for _, o := range rules {
				if o.ID != r.ID && o.Priority == r.Priority {
					return priorityInUse()
				}
			}
		}

		for _, id := range order {
			out.Rules = append(out.Rules, changed[id].R)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
