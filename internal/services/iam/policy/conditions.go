package policy

import (
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/policydoc"
)

const (
	allValues     = "ForAllValues"
	nullOperator  = "Null"
	trueValue     = "true"
	falseValue    = "false"
	boolOperator  = "Bool"
	ipOperator    = "IpAddress"
	notIPOperator = "NotIpAddress"
)

type operator struct {
	base, set        string
	negate, ifExists bool
}

func parseOperator(name string) (operator, bool) {
	op := operator{}
	if set, base, found := strings.Cut(name, ":"); found {
		if set != "ForAnyValue" && set != allValues {
			return op, false
		}

		op.set, name = set, base
	}

	op.ifExists = strings.HasSuffix(name, "IfExists")
	op.base = strings.TrimSuffix(name, "IfExists")
	switch op.base {
	case "StringNotEquals", "StringNotEqualsIgnoreCase", "StringNotLike", "StringNotLikeIgnoreCase",
		"ArnNotEquals", "ArnNotLike", "NumericNotEquals", "DateNotEquals", notIPOperator:
		op.negate = true
	case "StringEquals", "StringEqualsIgnoreCase", "StringLike", "StringLikeIgnoreCase", "ArnEquals", "ArnLike",
		boolOperator, nullOperator, "NumericEquals", "NumericLessThan", "NumericLessThanEquals", "NumericGreaterThan",
		"NumericGreaterThanEquals", "DateEquals", "DateLessThan", "DateLessThanEquals", "DateGreaterThan",
		"DateGreaterThanEquals", ipOperator:
	default:
		return op, false
	}

	if op.base == nullOperator && (op.ifExists || op.set != "") {
		return op, false
	}

	return op, true
}

func (op operator) holds(actual, want []string) bool {
	if op.base == nullOperator {
		return slices.ContainsFunc(want, func(v string) bool {
			return (v == trueValue && len(actual) == 0) || (v == falseValue && len(actual) > 0)
		})
	}

	if len(actual) == 0 {
		if op.ifExists {
			return true
		}

		if op.set != "" {
			return op.set == allValues
		}

		return op.negate
	}

	match := func(value string) bool {
		matched := false
		for _, pattern := range want {
			yes, valid := op.compare(value, pattern)
			if !valid {
				return false
			}

			matched = matched || yes
		}

		return matched != op.negate
	}

	if op.set == allValues || (op.set == "" && op.negate) {
		return !slices.ContainsFunc(actual, func(value string) bool { return !match(value) })
	}

	return slices.ContainsFunc(actual, match)
}

func (op operator) compare(actual, want string) (matches, valid bool) {
	switch op.base {
	case "StringEquals", "StringNotEquals":
		return actual == want, true
	case "StringEqualsIgnoreCase", "StringNotEqualsIgnoreCase":
		return strings.EqualFold(actual, want), true
	case "StringLike", "StringNotLike", "ArnEquals", "ArnNotEquals", "ArnLike", "ArnNotLike":
		return policydoc.Like(want, actual, false), true
	case "StringLikeIgnoreCase", "StringNotLikeIgnoreCase":
		return policydoc.Like(want, actual, true), true
	case boolOperator:
		if (actual != trueValue && actual != falseValue) || (want != trueValue && want != falseValue) {
			return false, false
		}

		return actual == want, true
	case ipOperator, notIPOperator:
		ip, err := netip.ParseAddr(actual)
		if err != nil {
			return false, false
		}

		prefix, err := netip.ParsePrefix(want)
		if err != nil {
			address, addressErr := netip.ParseAddr(want)
			if addressErr != nil {
				return false, false
			}

			prefix = netip.PrefixFrom(address, address.BitLen())
		}

		return prefix.Contains(ip.Unmap()), true
	}

	if strings.HasPrefix(op.base, "Numeric") {
		a, errA := strconv.ParseFloat(actual, 64)
		b, errB := strconv.ParseFloat(want, 64)
		if errA != nil || errB != nil || math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
			return false, false
		}

		return ordered(strings.TrimPrefix(op.base, "Numeric"), a, b), true
	}

	if strings.HasPrefix(op.base, "Date") {
		a, okA := date(actual)
		b, okB := date(want)
		if !okA || !okB {
			return false, false
		}

		return ordered(strings.TrimPrefix(op.base, "Date"), a, b), true
	}

	return false, false
}

func ordered(comparison string, a, b float64) bool {
	switch comparison {
	case "Equals", "NotEquals":
		return a == b
	case "LessThan":
		return a < b
	case "LessThanEquals":
		return a <= b
	case "GreaterThan":
		return a > b
	case "GreaterThanEquals":
		return a >= b
	}

	return false
}

func date(value string) (float64, bool) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return float64(parsed.Unix()) + float64(parsed.Nanosecond())/1e9, true
	}

	seconds, err := strconv.ParseFloat(value, 64)
	return seconds, err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0)
}

// Validate expected values before absent-key shortcuts such as IfExists or a
// negated comparison, so malformed conditions cannot grant access.
func (op operator) validValues(want []string) bool {
	for _, value := range want {
		switch {
		case op.base == nullOperator || op.base == boolOperator:
			if value != trueValue && value != falseValue {
				return false
			}
		case strings.HasPrefix(op.base, "Numeric"), strings.HasPrefix(op.base, "Date"),
			op.base == ipOperator, op.base == notIPOperator:
			actual := value
			if op.base == ipOperator || op.base == notIPOperator {
				if prefix, err := netip.ParsePrefix(value); err == nil {
					actual = prefix.Addr().String()
				}
			}

			if _, valid := op.compare(actual, value); !valid {
				return false
			}
		}
	}

	return true
}
