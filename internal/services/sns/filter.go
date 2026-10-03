package sns

import (
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
)

const (
	attrFilterPolicy      = "FilterPolicy"
	attrFilterPolicyScope = "FilterPolicyScope"
	scopeAttributes       = "MessageAttributes"
	scopeBody             = "MessageBody"
	typeString            = "String"
	typeNumber            = "Number"
)

// Filter policy limits, as SNS enforces them.
const (
	maxPolicySize         = 256 * 1024
	maxPolicyFields       = 5
	maxPolicyCombinations = 150
	maxPolicyNumeric      = 1e9
)

// set validates and applies one writable subscription attribute. check then
// validates the subscription as a whole.
func (sub *subscription) set(name, v string) error {
	switch name {
	case attrRaw:
		if v != valueTrue && v != valueFalse {
			return invalid("RawMessageDelivery must be true or false")
		}

		sub.Raw = v == valueTrue
	case attrFilterPolicy:
		// An empty policy removes the filter.
		if v == "{}" {
			v = ""
		}

		sub.FilterPolicy = v
	case attrFilterPolicyScope:
		if v != scopeAttributes && v != scopeBody {
			return invalid("FilterPolicyScope: Invalid filter policy scope, must be one of %s or %s",
				scopeAttributes, scopeBody)
		}

		sub.FilterPolicyScope = v
		if v == scopeAttributes {
			sub.FilterPolicyScope = ""
		}
	default:
		return invalid("Attribute %s is not supported", name)
	}

	return nil
}

// check validates the combination of a subscription's attributes.
func (sub *subscription) check() error {
	if sub.Raw && sub.Protocol != protocolSQS && sub.Protocol != protocolHTTP && sub.Protocol != protocolHTTPS {
		return invalid("Delivery protocol [%s] does not support raw message delivery.", sub.Protocol)
	}

	if sub.FilterPolicy == "" {
		return nil
	}

	_, err := sub.filter()
	return err
}

// filter compiles the subscription's filter policy, which must be set.
func (sub *subscription) filter() (*pattern.Pattern, error) {
	if len(sub.FilterPolicy) > maxPolicySize {
		return nil, invalid("FilterPolicy: Filter policy is too long")
	}

	p, err := pattern.Compile(sub.FilterPolicy, pattern.Dialect{
		MaxNumeric: maxPolicyNumeric,
		Flat:       sub.FilterPolicyScope != scopeBody,
	})
	if err != nil {
		return nil, invalid("FilterPolicy: %s", err.Error())
	}

	if p.Fields() > maxPolicyFields {
		return nil, invalid("FilterPolicy: Filter policy can not have more than %d keys", maxPolicyFields)
	}

	if p.Combinations() > maxPolicyCombinations {
		return nil, invalid("FilterPolicy: Filter policy is too complex")
	}

	return p, nil
}

// accepts reports whether the subscription's filter policy lets a message
// through. body is the text the subscription receives.
func (sub *subscription) accepts(attributes map[string]api.MessageAttributeValue, body string) (bool, error) {
	if sub.FilterPolicy == "" {
		return true, nil
	}

	p, err := sub.filter()
	if err != nil {
		return false, err
	}

	if sub.FilterPolicyScope == scopeBody {
		// A body that is not a JSON object matches no policy.
		event, ok := pattern.Flatten([]byte(body))
		return ok && p.Matches(event), nil
	}

	return p.Matches(attributeEvent(attributes)), nil
}

// attributeEvent returns the values filter policies see in message
// attributes: a String is a string, a Number a number, and a String.Array its
// elements. Binary attributes are not matched.
func attributeEvent(attributes map[string]api.MessageAttributeValue) pattern.Event {
	event := pattern.Event{}
	for name, a := range attributes {
		v := value(a.StringValue)
		switch base, custom, _ := strings.Cut(value(a.DataType), "."); {
		case base == typeString && custom == "Array":
			if values, ok := pattern.Values([]byte(v)); ok {
				event[name] = values
			}
		case base == typeString:
			event[name] = []any{v}
		case base == typeNumber:
			n, _ := strconv.ParseFloat(v, 64)
			event[name] = []any{n}
		}
	}

	return event
}
