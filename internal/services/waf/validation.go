package waf

import (
	"net/netip"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/waf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var resourceKinds = []string{
	kindIPSet, kindByteMatchSet, kindSizeConstraintSet, kindSqlInjectionMatchSet, kindXssMatchSet, kindGeoMatchSet,
	kindRegexPatternSet, kindRegexMatchSet, kindRule, kindRateBasedRule, kindWebACL,
}

var metricPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)

type reference struct{ kind, id string }

var predicateKinds = map[string]string{
	"IPMatch": kindIPSet, "ByteMatch": kindByteMatchSet, "SizeConstraint": kindSizeConstraintSet,
	"SqlInjectionMatch": kindSqlInjectionMatchSet, "XssMatch": kindXssMatchSet, "GeoMatch": kindGeoMatchSet,
	"RegexMatch": kindRegexMatchSet,
}

func references(r *record) []reference {
	out := []reference{}
	var predicates []api.Predicate
	if r.Rule != nil {
		predicates = r.Rule.Predicates
	}

	if r.RateBasedRule != nil {
		predicates = r.RateBasedRule.MatchPredicates
	}

	for _, p := range predicates {
		out = append(out, reference{predicateKinds[common.Value(p.Type)], common.Value(p.DataId)})
	}

	if r.RegexMatchSet != nil {
		for _, t := range r.RegexMatchSet.RegexMatchTuples {
			out = append(out, reference{kindRegexPatternSet, common.Value(t.RegexPatternSetId)})
		}
	}

	if r.WebACL != nil {
		for _, rule := range r.WebACL.Rules {
			kind := kindRule
			if common.Value(rule.Type) == "RATE_BASED" {
				kind = kindRateBasedRule
			}

			out = append(out, reference{kind, common.Value(rule.RuleId)})
		}
	}

	return out
}

func field(f *api.FieldToMatch, transformation *string) error {
	if f == nil ||
		!slices.Contains(
			[]string{"URI", "QUERY_STRING", "HEADER", "METHOD", "BODY", "SINGLE_QUERY_ARG", "ALL_QUERY_ARGS"},
			common.Value(f.Type),
		) {
		return invalid("Invalid FieldToMatch")
	}

	if common.Value(f.Type) == "HEADER" || common.Value(f.Type) == "SINGLE_QUERY_ARG" {
		if common.Value(f.Data) == "" {
			return invalid("FieldToMatch.Data is required")
		}
	} else if f.Data != nil {
		return invalid("FieldToMatch.Data is unsupported for this type")
	}

	if !slices.Contains(
		[]string{"NONE", "COMPRESS_WHITE_SPACE", "HTML_ENTITY_DECODE", "LOWERCASE", "CMD_LINE", "URL_DECODE"},
		common.Value(transformation),
	) {
		return invalid("Invalid TextTransformation")
	}

	return nil
}

func action(a *api.WafAction, defaultAction bool) error {
	if a == nil || !slices.Contains([]string{"ALLOW", "BLOCK", "COUNT"}, common.Value(a.Type)) ||
		(defaultAction && common.Value(a.Type) == "COUNT") {
		return invalid("Invalid WAF action")
	}

	return nil
}

func validate(tx storage.ReadTx, scope request.Scope, r *record) error {
	for _, ref := range references(r) {
		if ref.kind == "" {
			return invalid("Invalid predicate type")
		}

		if _, err := read(tx, scope, ref.kind, new(ref.id)); err != nil {
			return err
		}
	}

	var metric *string
	var predicates []api.Predicate
	if r.Rule != nil {
		metric = r.Rule.MetricName
		predicates = r.Rule.Predicates
	}

	if r.RateBasedRule != nil {
		v := r.RateBasedRule
		metric = v.MetricName
		predicates = v.MatchPredicates
		if common.Value(v.RateKey) != "IP" || common.Value(v.RateLimit) < 100 ||
			common.Value(v.RateLimit) > 2000000000 {
			return invalid("RateKey must be IP and RateLimit must be 100-2000000000")
		}
	}

	if r.WebACL != nil {
		metric = r.WebACL.MetricName
		if err := action(r.WebACL.DefaultAction, true); err != nil {
			return err
		}

		priorities := map[int64]bool{}
		ids := map[string]bool{}
		for _, v := range r.WebACL.Rules {
			if v.Priority == nil || *v.Priority < 0 || *v.Priority > 2147483647 ||
				priorities[common.Value(v.Priority)] ||
				ids[common.Value(v.RuleId)] {
				return invalid("Activated rules require unique IDs and nonnegative unique priorities")
			}

			if v.Type != nil && *v.Type != "REGULAR" && *v.Type != "RATE_BASED" {
				return invalid("Only REGULAR and RATE_BASED rules are supported")
			}

			if err := action(v.Action, false); err != nil {
				return err
			}

			priorities[*v.Priority] = true
			ids[common.Value(v.RuleId)] = true
		}
	}

	if r.Rule != nil || r.RateBasedRule != nil || r.WebACL != nil {
		if !metricPattern.MatchString(common.Value(metric)) || *metric == "All" || *metric == "DefaultAction" {
			return invalid("MetricName must contain 1-128 alphanumeric characters and must not be reserved")
		}
	}

	for _, p := range predicates {
		if p.Negated == nil {
			return invalid("Predicate.Negated is required")
		}
	}

	if r.IPSet != nil {
		for _, d := range r.IPSet.IPSetDescriptors {
			prefix, err := netip.ParsePrefix(common.Value(d.Value))
			if err != nil || (common.Value(d.Type) != "IPV4" && common.Value(d.Type) != "IPV6") ||
				(prefix.Addr().Is4() != (common.Value(d.Type) == "IPV4")) {
				return invalid("Invalid IP descriptor")
			}

			bits := prefix.Bits()
			if prefix != prefix.Masked() || (prefix.Addr().Is4() && bits != 8 && (bits < 16 || bits > 32)) ||
				(!prefix.Addr().Is4() && !slices.Contains([]int{24, 32, 48, 56, 64, 128}, bits)) {
				return invalid("Unsupported CIDR range")
			}
		}
	}

	if r.ByteMatchSet != nil {
		for _, v := range r.ByteMatchSet.ByteMatchTuples {
			if err := field(v.FieldToMatch, v.TextTransformation); err != nil {
				return err
			}

			if len(v.TargetString) < 1 || len(v.TargetString) > 50 ||
				!slices.Contains(
					[]string{"EXACTLY", "STARTS_WITH", "ENDS_WITH", "CONTAINS", "CONTAINS_WORD"},
					common.Value(v.PositionalConstraint),
				) {
				return invalid("Invalid byte match tuple")
			}
		}
	}

	if r.SizeConstraintSet != nil {
		for _, v := range r.SizeConstraintSet.SizeConstraints {
			if err := field(v.FieldToMatch, v.TextTransformation); err != nil {
				return err
			}

			if v.Size == nil || *v.Size < 0 || *v.Size > 21474836480 ||
				!slices.Contains([]string{"EQ", "NE", "LE", "LT", "GE", "GT"}, common.Value(v.ComparisonOperator)) {
				return invalid("Invalid size constraint")
			}
		}
	}

	if r.SqlInjectionMatchSet != nil {
		for _, v := range r.SqlInjectionMatchSet.SqlInjectionMatchTuples {
			if err := field(v.FieldToMatch, v.TextTransformation); err != nil {
				return err
			}
		}
	}

	if r.XssMatchSet != nil {
		for _, v := range r.XssMatchSet.XssMatchTuples {
			if err := field(v.FieldToMatch, v.TextTransformation); err != nil {
				return err
			}
		}
	}

	if r.RegexMatchSet != nil {
		for _, v := range r.RegexMatchSet.RegexMatchTuples {
			if err := field(v.FieldToMatch, v.TextTransformation); err != nil {
				return err
			}
		}
	}

	if r.RegexPatternSet != nil {
		for _, v := range r.RegexPatternSet.RegexPatternStrings {
			if len(v) < 1 || len(v) > 128 {
				return invalid("Regex patterns require 1-128 characters")
			}

			// Patterns are stored verbatim; no regex engine is used for request filtering.
		}
	}

	if r.GeoMatchSet != nil {
		for _, v := range r.GeoMatchSet.GeoMatchConstraints {
			if common.Value(v.Type) != "Country" || !slices.Contains(strings.Fields(countries), common.Value(v.Value)) {
				return invalid("Invalid country constraint")
			}
		}
	}

	return nil
}

const countries = `
AF AX AL DZ AS AD AO AI AQ AG AR AM AW AU AT AZ BS BH BD BB BY BE BZ BJ BM BT BO BQ BA BW BV BR IO BN BG BF BI
KH CM CA CV KY CF TD CL CN CX CC CO KM CG CD CK CR CI HR CU CW CY CZ DK DJ DM DO EC EG SV GQ ER EE ET FK FO FJ
FI FR GF PF TF GA GM GE DE GH GI GR GL GD GP GU GT GG GN GW GY HT HM VA HN HK HU IS IN ID IR IQ IE IM IL IT JM
JP JE JO KZ KE KI KP KR KW KG LA LV LB LS LR LY LI LT LU MO MK MG MW MY MV ML MT MH MQ MR MU YT MX FM MD MC MN
ME MS MA MZ MM NA NR NP NL NC NZ NI NE NG NU NF MP NO OM PK PW PS PA PG PY PE PH PN PL PT PR QA RE RO RU RW BL
SH KN LC MF PM VC WS SM ST SA SN RS SC SL SG SX SK SI SB SO ZA GS SS ES LK SD SR SJ SZ SE CH SY TW TJ TZ TH TL
TG TK TO TT TN TR TM TC TV UG UA AE GB US UM UY UZ VU VE VN VG VI WF EH YE ZM ZW
`

func empty(r *record) bool {
	if r.IPSet != nil {
		return len(r.IPSet.IPSetDescriptors) == 0
	}

	if r.ByteMatchSet != nil {
		return len(r.ByteMatchSet.ByteMatchTuples) == 0
	}

	if r.SizeConstraintSet != nil {
		return len(r.SizeConstraintSet.SizeConstraints) == 0
	}

	if r.SqlInjectionMatchSet != nil {
		return len(r.SqlInjectionMatchSet.SqlInjectionMatchTuples) == 0
	}

	if r.XssMatchSet != nil {
		return len(r.XssMatchSet.XssMatchTuples) == 0
	}

	if r.GeoMatchSet != nil {
		return len(r.GeoMatchSet.GeoMatchConstraints) == 0
	}

	if r.RegexPatternSet != nil {
		return len(r.RegexPatternSet.RegexPatternStrings) == 0
	}

	if r.RegexMatchSet != nil {
		return len(r.RegexMatchSet.RegexMatchTuples) == 0
	}

	if r.Rule != nil {
		return len(r.Rule.Predicates) == 0
	}

	if r.RateBasedRule != nil {
		return len(r.RateBasedRule.MatchPredicates) == 0
	}

	if r.WebACL != nil {
		return len(r.WebACL.Rules) == 0
	}

	return true
}
