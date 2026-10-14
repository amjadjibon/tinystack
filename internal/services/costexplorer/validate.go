package costexplorer

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/common"
)

func genericName(value *string) error {
	if value == nil || *value == "" || utf8.RuneCountInString(*value) > 1024 {
		return awserror.Validation("Name is required and must contain at most 1024 characters.")
	}

	return nil
}

const (
	dimensionLinkedAccount = "LINKED_ACCOUNT"
	dimensionTag           = "TAG"
)

var categoryPattern = regexp.MustCompile(`^[\p{L}\p{N}\p{Z}_-]+$`)

func categoryText(value *string) error {
	v := common.Value(value)
	if utf8.RuneCountInString(v) < 1 || utf8.RuneCountInString(v) > 50 || strings.HasPrefix(v, " ") ||
		strings.HasSuffix(v, " ") ||
		!categoryPattern.MatchString(v) {
		return awserror.Validation(
			"Cost category names and values require 1–50 valid characters without leading or trailing spaces.",
		)
	}

	return nil
}

func validateMonitor(m *api.AnomalyMonitor) error {
	if err := genericName(m.MonitorName); err != nil {
		return err
	}

	dimension := common.Value(m.MonitorDimension)
	switch common.Value(m.MonitorType) {
	case "DIMENSIONAL":
		switch dimension {
		case "SERVICE", dimensionLinkedAccount:
			if m.MonitorSpecification != nil {
				return awserror.Validation(
					"SERVICE and LINKED_ACCOUNT dimensional monitors cannot have a specification.",
				)
			}
		case dimensionTag:
			e := m.MonitorSpecification
			if e == nil || e.Tags == nil || e.Tags.Key == nil || *e.Tags.Key == "" || len(e.Tags.Values) != 0 ||
				len(e.Tags.MatchOptions) != 0 {
				return awserror.Validation("A TAG dimensional monitor requires only a tag key.")
			}

			if err := validateExpression(e); err != nil {
				return err
			}
		case "COST_CATEGORY":
			e := m.MonitorSpecification
			if e == nil || e.CostCategories == nil || e.CostCategories.Key == nil || *e.CostCategories.Key == "" ||
				len(e.CostCategories.Values) != 0 ||
				len(e.CostCategories.MatchOptions) != 0 {
				return awserror.Validation("A COST_CATEGORY dimensional monitor requires only a cost category key.")
			}

			if err := validateExpression(e); err != nil {
				return err
			}
		default:
			return awserror.Validation("Invalid MonitorDimension.")
		}
	case "CUSTOM":
		e := m.MonitorSpecification
		if m.MonitorDimension != nil || e == nil {
			return awserror.Validation("CUSTOM monitors require a specification and cannot specify MonitorDimension.")
		}

		if err := validateExpression(e); err != nil {
			return err
		}

		switch {
		case e.Dimensions != nil:
			if common.Value(e.Dimensions.Key) != dimensionLinkedAccount || len(e.Dimensions.Values) < 1 ||
				len(e.Dimensions.Values) > 10 ||
				len(e.Dimensions.MatchOptions) != 0 {
				return awserror.Validation("CUSTOM account monitors require 1–10 LINKED_ACCOUNT values.")
			}
		case e.Tags != nil:
			if common.Value(e.Tags.Key) == "" || len(e.Tags.Values) < 1 || len(e.Tags.Values) > 10 ||
				len(e.Tags.MatchOptions) != 0 {
				return awserror.Validation("CUSTOM tag monitors require a key and 1–10 values.")
			}
		case e.CostCategories != nil:
			if common.Value(e.CostCategories.Key) == "" || len(e.CostCategories.Values) != 1 ||
				len(e.CostCategories.MatchOptions) != 0 {
				return awserror.Validation("CUSTOM cost category monitors require a key and one value.")
			}
		default:
			return awserror.Validation("Unsupported CUSTOM monitor specification.")
		}
	default:
		return awserror.Validation("MonitorType must be DIMENSIONAL or CUSTOM.")
	}

	return nil
}

var (
	dimensions = strings.Fields(
		"AZ INSTANCE_TYPE LINKED_ACCOUNT PAYER_ACCOUNT LINKED_ACCOUNT_NAME OPERATION PURCHASE_TYPE " +
			"REGION SERVICE SERVICE_CODE USAGE_TYPE USAGE_TYPE_GROUP RECORD_TYPE OPERATING_SYSTEM TENANCY " +
			"SCOPE PLATFORM SUBSCRIPTION_ID LEGAL_ENTITY_NAME DEPLOYMENT_OPTION DATABASE_ENGINE CACHE_ENGINE " +
			"INSTANCE_TYPE_FAMILY BILLING_ENTITY RESERVATION_ID RESOURCE_ID RIGHTSIZING_TYPE " +
			"SAVINGS_PLANS_TYPE SAVINGS_PLAN_ARN PAYMENT_OPTION AGREEMENT_END_DATE_TIME_AFTER " +
			"AGREEMENT_END_DATE_TIME_BEFORE INVOICING_ENTITY ANOMALY_TOTAL_IMPACT_ABSOLUTE " +
			"ANOMALY_TOTAL_IMPACT_PERCENTAGE PRODUCT_ATTRIBUTE",
	)
	matchOptions = strings.Fields(
		"EQUALS ABSENT STARTS_WITH ENDS_WITH CONTAINS CASE_SENSITIVE CASE_INSENSITIVE GREATER_THAN_OR_EQUAL",
	)
)

func validateValues(key *string, values, options []string) error {
	if key == nil || utf8.RuneCountInString(*key) > 1024 {
		return awserror.Validation("Expression key is required and must be at most 1024 characters.")
	}

	for _, v := range values {
		if utf8.RuneCountInString(v) > 1024 {
			return awserror.Validation("Expression values must be at most 1024 characters.")
		}
	}

	for _, option := range options {
		if !slices.Contains(matchOptions, option) {
			return awserror.Validation("Invalid MatchOption %q.", option)
		}
	}

	return nil
}

func validateExpression(e *api.Expression) error {
	roots := 0
	if len(e.And) != 0 {
		roots++
	}

	if len(e.Or) != 0 {
		roots++
	}

	if e.Not != nil {
		roots++
	}

	if e.Dimensions != nil {
		roots++
	}

	if e.Tags != nil {
		roots++
	}

	if e.CostCategories != nil {
		roots++
	}

	if roots != 1 {
		return awserror.Validation("Each expression must have exactly one root operator.")
	}

	for _, child := range e.And {
		if err := validateExpression(&child); err != nil {
			return err
		}
	}

	for _, child := range e.Or {
		if err := validateExpression(&child); err != nil {
			return err
		}
	}

	if e.Not != nil {
		return validateExpression(e.Not)
	}

	if d := e.Dimensions; d != nil {
		if !slices.Contains(dimensions, common.Value(d.Key)) {
			return awserror.Validation("Invalid dimension key.")
		}

		return validateValues(d.Key, d.Values, d.MatchOptions)
	}

	if t := e.Tags; t != nil {
		return validateValues(t.Key, t.Values, t.MatchOptions)
	}

	if c := e.CostCategories; c != nil {
		return validateValues(c.Key, c.Values, c.MatchOptions)
	}

	return nil
}

func validateThreshold(e *api.Expression) error {
	if err := validateExpression(e); err != nil {
		return err
	}

	if e.Not != nil || e.Tags != nil || e.CostCategories != nil {
		return awserror.Validation("ThresholdExpression supports And, Or and impact Dimensions only.")
	}

	for _, child := range e.And {
		if err := validateThreshold(&child); err != nil {
			return err
		}
	}

	for _, child := range e.Or {
		if err := validateThreshold(&child); err != nil {
			return err
		}
	}

	if d := e.Dimensions; d != nil {
		if !slices.Contains(
			[]string{"ANOMALY_TOTAL_IMPACT_ABSOLUTE", "ANOMALY_TOTAL_IMPACT_PERCENTAGE"},
			common.Value(d.Key),
		) ||
			len(d.Values) != 1 ||
			len(d.MatchOptions) != 1 ||
			d.MatchOptions[0] != "GREATER_THAN_OR_EQUAL" {
			return awserror.Validation(
				"Threshold dimensions require an absolute/percentage impact, one value and GREATER_THAN_OR_EQUAL.",
			)
		}

		v, err := strconv.ParseFloat(d.Values[0], 64)
		if err != nil || v < 0 || v != v || strings.Contains(d.Values[0], "Inf") {
			return awserror.Validation("Threshold expression value must be finite and nonnegative.")
		}
	}

	return nil
}

var subscriberPattern = regexp.MustCompile(
	`^([a-zA-Z0-9.!#$%&'*+=?^_‘{|}~-]+@[a-zA-Z0-9_-]+(\.[a-zA-Z0-9_-]+)+|` +
		`arn:aws[a-zA-Z-]*:sns:[a-zA-Z0-9-]+:[0-9]{12}:[a-zA-Z0-9_-]+(\.fifo)?)$`,
)

func validateSubscriber(sub api.Subscriber, frequency string) error {
	address := common.Value(sub.Address)
	if utf8.RuneCountInString(address) < 6 || utf8.RuneCountInString(address) > 302 ||
		!subscriberPattern.MatchString(address) {
		return awserror.Validation("Invalid subscriber address.")
	}

	expected := "EMAIL"
	if frequency == "IMMEDIATE" {
		expected = "SNS"
	}

	if common.Value(sub.Type) != expected || (expected == "SNS") != strings.HasPrefix(address, "arn:") {
		return awserror.Validation("IMMEDIATE subscriptions use SNS; DAILY and WEEKLY subscriptions use EMAIL.")
	}

	if sub.Status != nil {
		return awserror.Validation("Subscriber Status is an output field.")
	}

	return nil
}

func validateCategory(c *api.CostCategory) error {
	if err := categoryText(c.Name); err != nil {
		return err
	}

	if common.Value(c.RuleVersion) != "CostCategoryExpression.v1" {
		return awserror.Validation("Invalid RuleVersion.")
	}

	if len(c.Rules) < 1 || len(c.Rules) > 500 {
		return awserror.Validation("Rules must contain 1–500 entries.")
	}

	if c.DefaultValue != nil {
		if err := categoryText(c.DefaultValue); err != nil {
			return err
		}
	}

	for _, rule := range c.Rules {
		switch common.Value(common.OrElse(rule.Type, new("REGULAR"))) {
		case "REGULAR":
			if rule.Rule == nil || rule.InheritedValue != nil {
				return awserror.Validation("REGULAR rules require Rule and Value, without InheritedValue.")
			}

			if err := categoryText(rule.Value); err != nil {
				return err
			}

			if err := validateCategoryExpression(rule.Rule); err != nil {
				return err
			}
		case "INHERITED_VALUE":
			if rule.InheritedValue == nil || rule.Rule != nil || rule.Value != nil {
				return awserror.Validation("INHERITED_VALUE rules require only InheritedValue.")
			}

			d := rule.InheritedValue
			if d.DimensionKey != nil && utf8.RuneCountInString(*d.DimensionKey) > 1024 {
				return awserror.Validation("Inherited DimensionKey must be at most 1024 characters.")
			}

			if !slices.Contains([]string{dimensionTag, "LINKED_ACCOUNT_NAME"}, common.Value(d.DimensionName)) {
				return awserror.Validation("Invalid inherited dimension name.")
			}

			if common.Value(d.DimensionName) == dimensionTag && common.Value(d.DimensionKey) == "" {
				return awserror.Validation("Inherited TAG rules require DimensionKey.")
			}
		default:
			return awserror.Validation("Invalid cost category rule Type.")
		}
	}

	if c.SplitChargeRules != nil && (len(c.SplitChargeRules) < 1 || len(c.SplitChargeRules) > 10) {
		return awserror.Validation("SplitChargeRules must contain 1–10 entries.")
	}

	for _, rule := range c.SplitChargeRules {
		if rule.Source == nil || len(*rule.Source) > 1024 || len(rule.Targets) < 1 || len(rule.Targets) > 500 {
			return awserror.Validation("Split charge rules require Source and 1–500 Targets.")
		}

		if !slices.Contains([]string{"FIXED", "PROPORTIONAL", "EVEN"}, common.Value(rule.Method)) {
			return awserror.Validation("Invalid split charge Method.")
		}

		if rule.Parameters != nil && (len(rule.Parameters) < 1 || len(rule.Parameters) > 10) {
			return awserror.Validation("Split charge Parameters must contain 1–10 entries.")
		}

		for _, target := range rule.Targets {
			if len(target) > 1024 {
				return awserror.Validation("Split charge targets must be at most 1024 characters.")
			}
		}

		for _, p := range rule.Parameters {
			if common.Value(p.Type) != "ALLOCATION_PERCENTAGES" || len(p.Values) < 1 || len(p.Values) > 500 {
				return awserror.Validation("Invalid split charge parameter.")
			}

			for _, v := range p.Values {
				if len(v) > 1024 {
					return awserror.Validation("Split charge parameter values must be at most 1024 characters.")
				}
			}
		}
	}

	return nil
}

func validateCategoryExpression(e *api.Expression) error {
	if err := validateExpression(e); err != nil {
		return err
	}

	if e.Dimensions != nil &&
		!slices.Contains(
			[]string{
				dimensionLinkedAccount,
				"SERVICE_CODE",
				"RECORD_TYPE",
				"LINKED_ACCOUNT_NAME",
				"REGION",
				"USAGE_TYPE",
			},
			common.Value(e.Dimensions.Key),
		) {
		return awserror.Validation("Unsupported cost category dimension.")
	}

	for _, c := range e.And {
		if err := validateCategoryExpression(&c); err != nil {
			return err
		}
	}

	for _, c := range e.Or {
		if err := validateCategoryExpression(&c); err != nil {
			return err
		}
	}

	if e.Not != nil {
		return validateCategoryExpression(e.Not)
	}

	return nil
}
