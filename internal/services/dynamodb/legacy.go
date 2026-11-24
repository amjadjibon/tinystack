package dynamodb

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/common"
)

type legacyParam struct {
	name string
	set  bool
}

func activeParams(params []legacyParam) []string {
	var active []string
	for _, p := range params {
		if p.set {
			active = append(active, p.name)
		}
	}

	return active
}

func checkLegacy(legacy, modern []legacyParam, placeholders bool) error {
	old := activeParams(legacy)
	if len(old) == 0 {
		return nil
	}

	expressions := activeParams(modern)
	if len(expressions) == 0 {
		if placeholders {
			return awserror.Validation("ExpressionAttributeNames or ExpressionAttributeValues requires an expression")
		}

		return nil
	}

	return awserror.Validation("Can not use both expression and non-expression parameters in the same request: "+
		"Non-expression parameters: {%s} Expression parameters: {%s}",
		strings.Join(old, ", "), strings.Join(expressions, ", "))
}

// legacyExpressions produces ordinary DynamoDB expressions. Each original
// attribute name is an alias, so a name such as "a.b" stays a single attribute.
type legacyExpressions struct {
	names  map[string]string
	values item
}

func (b *legacyExpressions) name(name string) string {
	if b.names == nil {
		b.names = map[string]string{}
	}

	key := fmt.Sprintf("#legacy%d", len(b.names))
	b.names[key] = name
	return key
}

func (b *legacyExpressions) value(v *api.AttributeValue) string {
	if b.values == nil {
		b.values = item{}
	}

	key := fmt.Sprintf(":legacy%d", len(b.values))
	b.values[key] = *v
	return key
}

func (b *legacyExpressions) projection(attrs []string) (*string, error) {
	if len(attrs) == 0 {
		return nil, awserror.Validation("AttributesToGet must contain at least one attribute")
	}

	names := make([]string, len(attrs))
	for i, attr := range attrs {
		names[i] = b.name(attr)
	}

	return new(strings.Join(names, ", ")), nil
}

func readProjection(attrs []string, names map[string]string, expression *string) (projection, error) {
	if err := checkLegacy([]legacyParam{{"AttributesToGet", attrs != nil}},
		[]legacyParam{{"ProjectionExpression", expression != nil}}, names != nil); err != nil {
		return nil, err
	}

	if attrs != nil {
		var b legacyExpressions
		var err error
		expression, err = b.projection(attrs)
		if err != nil {
			return nil, err
		}

		names = b.names
	}

	ex, err := newExpressions(names, nil)
	if err != nil {
		return nil, err
	}

	pr, err := ex.projection(expression)
	if err != nil {
		return nil, err
	}

	return pr, ex.finish()
}

var legacyComparison = map[string]string{"EQ": "=", "NE": "<>", "LT": "<", "LE": "<=", "GT": ">", "GE": ">="}

// compare translates a condition and validates the arity of the legacy operator.
func (b *legacyExpressions) compare(name, op string, values []api.AttributeValue) (string, error) {
	var count int
	comparison := legacyComparison[op]
	switch {
	case comparison != "", op == "BEGINS_WITH", op == "CONTAINS", op == "NOT_CONTAINS":
		count = 1
	case op == "BETWEEN":
		count = 2
	case op == "IN" && len(values) > 0:
		count = len(values)
	case op == typeNull, op == "NOT_NULL":
	default:
		return "", awserror.Validation("Invalid ComparisonOperator: %s", op)
	}

	if len(values) != count {
		return "", awserror.Validation("ComparisonOperator %s requires %d attribute values", op, count)
	}

	attr := b.name(name)
	args := make([]string, len(values))
	for i := range values {
		if values[i].M != nil || values[i].L != nil {
			return "", awserror.Validation("Legacy conditions do not support List or Map attribute values")
		}

		args[i] = b.value(&values[i])
	}

	switch {
	case comparison != "":
		return attr + " " + comparison + " " + args[0], nil
	case op == typeNull:
		return fnAttributeNotExists + "(" + attr + ")", nil
	case op == "NOT_NULL":
		return fnAttributeExists + "(" + attr + ")", nil
	case op == "BETWEEN":
		return attr + " BETWEEN " + args[0] + " AND " + args[1], nil
	case op == "IN":
		return attr + " IN (" + strings.Join(args, ", ") + ")", nil
	case op == "NOT_CONTAINS":
		return "NOT contains(" + attr + ", " + args[0] + ")", nil
	default:
		return strings.ToLower(op) + "(" + attr + ", " + args[0] + ")", nil
	}
}

func legacyJoin(operator *string, count int) (string, error) {
	if operator == nil {
		return " AND ", nil
	}

	if (*operator != "AND" && *operator != "OR") || count == 0 {
		return "", awserror.Validation("ConditionalOperator must be AND or OR and requires legacy conditions")
	}

	return " " + *operator + " ", nil
}

func (b *legacyExpressions) conditions(conditions map[string]api.Condition, operator *string) (*string, error) {
	join, err := legacyJoin(operator, len(conditions))
	if err != nil || len(conditions) == 0 {
		return nil, err
	}

	parts := make([]string, 0, len(conditions))
	for _, name := range slices.Sorted(maps.Keys(conditions)) {
		c := conditions[name]
		part, err := b.compare(name, common.Value(c.ComparisonOperator), c.AttributeValueList)
		if err != nil {
			return nil, err
		}

		parts = append(parts, part)
	}

	return new(strings.Join(parts, join)), nil
}

func (b *legacyExpressions) expected(
	conditions map[string]api.ExpectedAttributeValue,
	operator *string,
) (*string, error) {
	join, err := legacyJoin(operator, len(conditions))
	if err != nil || len(conditions) == 0 {
		return nil, err
	}

	parts := make([]string, 0, len(conditions))
	for _, name := range slices.Sorted(maps.Keys(conditions)) {
		c := conditions[name]
		op, vals := common.Value(c.ComparisonOperator), c.AttributeValueList
		switch {
		case c.ComparisonOperator != nil:
			if c.Exists != nil || c.Value != nil {
				return nil, awserror.Validation("Expected cannot combine ComparisonOperator with Exists or Value")
			}
		case vals != nil:
			return nil, awserror.Validation("AttributeValueList requires a ComparisonOperator")
		case c.Value != nil && common.Value(common.OrElse(c.Exists, new(true))):
			op, vals = "EQ", []api.AttributeValue{*c.Value}
		case c.Value == nil && c.Exists != nil && !*c.Exists:
			op = typeNull
		default:
			return nil, awserror.Validation("Expected requires Value with Exists true or no Value with Exists false")
		}

		part, err := b.compare(name, op, vals)
		if err != nil {
			return nil, err
		}

		parts = append(parts, part)
	}

	return new(strings.Join(parts, join)), nil
}

func (b *legacyExpressions) updates(updates map[string]api.AttributeValueUpdate) (*string, error) {
	sections := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(updates)) {
		u := updates[name]
		attr := b.name(name)
		action := *common.OrElse(u.Action, new("PUT"))
		switch {
		case action == "PUT" && u.Value != nil:
			sections[sectionSet] = append(sections[sectionSet], attr+" = "+b.value(u.Value))
		case action == sectionAdd && u.Value != nil:
			sections[sectionAdd] = append(sections[sectionAdd], attr+" "+b.value(u.Value))
		case action == stmtDelete && u.Value != nil:
			sections[stmtDelete] = append(sections[stmtDelete], attr+" "+b.value(u.Value))
		case action == stmtDelete:
			sections[sectionRemove] = append(sections[sectionRemove], attr)
		default:
			return nil, awserror.Validation("AttributeUpdates requires PUT with a value, ADD with a value, or DELETE")
		}
	}

	var parts []string
	for _, section := range []string{sectionSet, sectionAdd, sectionRemove, stmtDelete} {
		if actions := sections[section]; len(actions) > 0 {
			parts = append(parts, section+" "+strings.Join(actions, ", "))
		}
	}

	return new(strings.Join(parts, " ")), nil
}

func prepareLegacyWrite(
	expected map[string]api.ExpectedAttributeValue, updates map[string]api.AttributeValueUpdate,
	operator, condition, update *string, placeholders bool,
) (b legacyExpressions, cond, upd *string, err error) {
	err = checkLegacy(
		[]legacyParam{
			{"AttributeUpdates", updates != nil},
			{"Expected", expected != nil},
			{"ConditionalOperator", operator != nil},
		},
		[]legacyParam{{"ConditionExpression", condition != nil}, {"UpdateExpression", update != nil}},
		placeholders,
	)
	if err != nil {
		return b, nil, nil, err
	}

	cond, err = b.expected(expected, operator)
	if err != nil {
		return b, nil, nil, err
	}

	if len(updates) > 0 {
		upd, err = b.updates(updates)
	}

	return b, cond, upd, err
}

func prepareLegacyRead(
	attrs []string, keys, filters map[string]api.Condition, filterName string,
	operator, project, key, filter *string, placeholders bool,
) (b legacyExpressions, pr, kc, f *string, err error) {
	err = checkLegacy(
		[]legacyParam{
			{"AttributesToGet", attrs != nil},
			{"KeyConditions", keys != nil},
			{filterName, filters != nil},
			{"ConditionalOperator", operator != nil},
		},
		[]legacyParam{
			{"ProjectionExpression", project != nil},
			{"KeyConditionExpression", key != nil},
			{"FilterExpression", filter != nil},
		}, placeholders,
	)
	if err != nil {
		return b, nil, nil, nil, err
	}

	kc, err = b.conditions(keys, nil)
	if err != nil {
		return b, nil, nil, nil, err
	}

	f, err = b.conditions(filters, operator)
	if err == nil && attrs != nil {
		pr, err = b.projection(attrs)
	}

	return b, pr, kc, f, err
}
