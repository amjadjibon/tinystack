package wafv2

import (
	"net/netip"
	"regexp"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	namePattern        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	descriptionPattern = regexp.MustCompile(`^[\w+=:#@/\-,.][\w+=:#@/\-,.\s]+[\w+=:#@/\-,.]$`)
	metricPattern      = regexp.MustCompile(`^[\w#:.\-/]+$`)
)

func nameOK(name *string) error {
	if name == nil || !namePattern.MatchString(*name) {
		return invalid("Name requires 1-128 letters, digits, underscores or hyphens")
	}
	return nil
}

func descriptionOK(value *string) error {
	if value != nil && (utf8.RuneCountInString(*value) > 256 || !descriptionPattern.MatchString(*value)) {
		return invalid("Description must match the WAF description pattern and contain at most 256 characters")
	}
	return nil
}

func visibilityOK(v *api.VisibilityConfig) error {
	if v == nil || v.SampledRequestsEnabled == nil || v.CloudWatchMetricsEnabled == nil || v.MetricName == nil {
		return invalid("VisibilityConfig requires both booleans and MetricName")
	}
	n := *v.MetricName
	if len(n) < 1 || len(n) > 255 || !metricPattern.MatchString(n) || n == "All" || n == "Default_Action" {
		return invalid("Invalid or reserved MetricName")
	}
	return nil
}

func choices(values ...bool) int {
	n := 0
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}

func stateOK(tx storage.ReadTx, scope request.Scope, r *record) error {
	if r.Kind == kindIP {
		return addressesOK(r.Family, r.Addresses)
	}
	if r.Kind == kindGroup && (r.Capacity < 1 || r.Capacity > 5000) {
		return invalid("Capacity must be between 1 and 5000 and cannot be changed")
	}
	if r.Kind == kindACL &&
		(r.DefaultAction == nil || choices(r.DefaultAction.Allow != nil, r.DefaultAction.Block != nil) != 1) {
		return invalid("DefaultAction requires exactly one Allow or Block")
	}
	if err := visibilityOK(r.Visibility); err != nil {
		return err
	}
	if len(r.Rules) > 50 {
		return failure("WAFLimitsExceededException", "At most 50 reference statements per web ACL or rule group")
	}
	names := map[string]bool{}
	priorities := map[int64]bool{}
	for _, rule := range r.Rules {
		if err := nameOK(rule.Name); err != nil {
			return err
		}
		if rule.Priority == nil || *rule.Priority < 0 || *rule.Priority > 2147483647 {
			return invalid("Rule Priority must be an integer between 0 and 2147483647")
		}
		if names[*rule.Name] || priorities[*rule.Priority] {
			return invalid("Rule names and priorities must be unique")
		}
		names[*rule.Name] = true
		priorities[*rule.Priority] = true
		if err := visibilityOK(rule.VisibilityConfig); err != nil {
			return err
		}
		st := rule.Statement
		if st == nil || choices(st.IPSetReferenceStatement != nil, st.RuleGroupReferenceStatement != nil) != 1 {
			return invalid("Statement requires exactly one supported reference")
		}
		targetKind := kindIP
		if st.RuleGroupReferenceStatement != nil {
			targetKind = kindGroup
			if r.Kind != kindACL {
				return invalid("RuleGroupReferenceStatement is only supported in web ACLs")
			}
			if rule.Action != nil || rule.OverrideAction == nil ||
				choices(rule.OverrideAction.None != nil, rule.OverrideAction.Count != nil) != 1 {
				return invalid("Rule group references require exactly one OverrideAction None or Count and no Action")
			}
		} else if rule.OverrideAction != nil || rule.Action == nil ||
			choices(rule.Action.Allow != nil, rule.Action.Block != nil, rule.Action.Count != nil) != 1 {
			return invalid("IP set references require exactly one Action Allow, Block or Count and no OverrideAction")
		}
		if _, err := byARN(tx, scope, reference(st), targetKind, r.Scope); err != nil {
			return err
		}
	}
	return nil
}

func addressesOK(family string, values []string) error {
	if family != "IPV4" && family != "IPV6" {
		return invalid("IPAddressVersion must be IPV4 or IPV6")
	}
	if values == nil {
		return invalid("Addresses is required (an empty array is allowed)")
	}
	if len(values) > 10000 {
		return failure("WAFLimitsExceededException", "At most 10000 CIDRs per IP set")
	}
	for _, v := range values {
		p, err := netip.ParsePrefix(v)
		if len(v) > 50 || err != nil || p.Bits() == 0 || p.Addr().Is4In6() || (family == "IPV4") != p.Addr().Is4() {
			return invalid("Addresses must contain valid CIDRs of the selected IP family, excluding /0")
		}
	}
	return nil
}
