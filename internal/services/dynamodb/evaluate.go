package dynamodb

import (
	"bytes"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
)

type item = map[string]api.AttributeValue

const (
	typeBool = "BOOL"
	typeNull = "NULL"
)

var attributeTypes = map[string]bool{
	"S": true, "N": true, "B": true, typeBool: true, typeNull: true,
	"SS": true, "NS": true, "BS": true, "L": true, "M": true,
}

// attributeType names v's type, or returns "" for a missing value.
//
//nolint:gocyclo // One case per type.
func attributeType(v *api.AttributeValue) string {
	switch {
	case v == nil:
		return ""
	case v.S != nil:
		return "S"
	case v.N != nil:
		return "N"
	case v.B != nil:
		return "B"
	case v.BOOL != nil:
		return typeBool
	case v.NULL != nil:
		return typeNull
	case v.SS != nil:
		return "SS"
	case v.NS != nil:
		return "NS"
	case v.BS != nil:
		return "BS"
	case v.L != nil:
		return "L"
	case v.M != nil:
		return "M"
	default:
		return ""
	}
}

// rat parses a number that was validated when it was stored or received.
func rat(s string) *big.Rat {
	r, err := number(s)
	if err != nil {
		return new(big.Rat)
	}

	return r
}

// order compares two numbers, strings, or binaries; ok is false for any other
// pair, including values of different types.
func order(a, b *api.AttributeValue) (n int, ok bool) {
	switch {
	case a == nil || b == nil:
		return 0, false
	case a.N != nil && b.N != nil:
		return rat(*a.N).Cmp(rat(*b.N)), true
	case a.S != nil && b.S != nil:
		return strings.Compare(*a.S, *b.S), true
	case a.B != nil && b.B != nil:
		return bytes.Compare(*a.B, *b.B), true
	default:
		return 0, false
	}
}

// members returns a set's members in a comparable form.
func members(v *api.AttributeValue) map[string]bool {
	out := map[string]bool{}
	switch {
	case v.SS != nil:
		for _, s := range *v.SS {
			out[s] = true
		}
	case v.NS != nil:
		for _, n := range *v.NS {
			out[rat(n).RatString()] = true
		}
	case v.BS != nil:
		for _, b := range *v.BS {
			out[string(b)] = true
		}
	}

	return out
}

func equal(a, b *api.AttributeValue) bool {
	kind := attributeType(a)
	if kind == "" || kind != attributeType(b) {
		return false
	}

	switch kind {
	case "S", "N", "B":
		n, _ := order(a, b)
		return n == 0
	case typeBool:
		return *a.BOOL == *b.BOOL
	case typeNull:
		return true
	case "L":
		return slices.EqualFunc(*a.L, *b.L, func(x, y api.AttributeValue) bool { return equal(&x, &y) })
	case "M":
		if len(*a.M) != len(*b.M) {
			return false
		}

		for k, x := range *a.M {
			y, ok := (*b.M)[k]
			if !ok || !equal(&x, &y) {
				return false
			}
		}

		return true
	default:
		x, y := members(a), members(b)
		if len(x) != len(y) {
			return false
		}

		for m := range x {
			if !y[m] {
				return false
			}
		}

		return true
	}
}

// get returns the value at p, or nil when the path does not exist.
func get(it item, p path) *api.AttributeValue {
	v, ok := it[p[0].name]
	if !ok {
		return nil
	}

	for _, s := range p[1:] {
		switch {
		case s.list && v.L != nil && s.index < len(*v.L):
			v = (*v.L)[s.index]
		case !s.list && v.M != nil:
			child, found := (*v.M)[s.name]
			if !found {
				return nil
			}

			v = child
		default:
			return nil
		}
	}

	return &v
}

// resolve evaluates a condition operand. size is missing for types it does not
// measure, so comparisons with it are false.
func (o *operand) resolve(it item) *api.AttributeValue {
	switch {
	case o.value != nil:
		return o.value
	case o.fn == fnSize:
		v := get(it, o.args[0].path)
		n := -1
		switch attributeType(v) {
		case "S":
			n = len(*v.S)
		case "B":
			n = len(*v.B)
		case "SS", "NS", "BS":
			n = len(members(v))
		case "L":
			n = len(*v.L)
		case "M":
			n = len(*v.M)
		}

		if n < 0 {
			return nil
		}

		return &api.AttributeValue{N: new(strconv.Itoa(n))}
	default:
		return get(it, o.path)
	}
}

// match evaluates the condition against it; a nil item has no attributes.
//
//nolint:gocyclo // One case per operator and function.
func (c *condition) match(it item) bool {
	switch c.op {
	case condTrue:
		return true
	case condAnd:
		return c.sub[0].match(it) && c.sub[1].match(it)
	case condOr:
		return c.sub[0].match(it) || c.sub[1].match(it)
	case condNot:
		return !c.sub[0].match(it)
	case condCompare:
		a, b := c.args[0].resolve(it), c.args[1].resolve(it)
		switch c.cmp {
		case "=":
			return equal(a, b)
		case "<>":
			return (a != nil || b != nil) && !equal(a, b)
		}

		n, ok := order(a, b)
		switch c.cmp {
		case "<":
			return ok && n < 0
		case "<=":
			return ok && n <= 0
		case ">":
			return ok && n > 0
		default:
			return ok && n >= 0
		}
	case condBetween:
		v := c.args[0].resolve(it)
		lower, okLower := order(v, c.args[1].resolve(it))
		upper, okUpper := order(v, c.args[2].resolve(it))
		return okLower && okUpper && lower >= 0 && upper <= 0
	case condIn:
		v := c.args[0].resolve(it)
		return slices.ContainsFunc(c.args[1:], func(o *operand) bool { return equal(v, o.resolve(it)) })
	default:
		return c.function(it)
	}
}

func (c *condition) function(it item) bool {
	v := get(it, c.args[0].path)
	switch c.fn {
	case fnAttributeExists:
		return v != nil
	case fnAttributeNotExists:
		return v == nil
	case fnAttributeType:
		return attributeType(v) == *c.args[1].resolve(it).S
	case fnBeginsWith:
		prefix := c.args[1].resolve(it)
		switch {
		case v == nil || prefix == nil:
			return false
		case v.S != nil && prefix.S != nil:
			return strings.HasPrefix(*v.S, *prefix.S)
		case v.B != nil && prefix.B != nil:
			return bytes.HasPrefix(*v.B, *prefix.B)
		default:
			return false
		}
	default: // contains
		x := c.args[1].resolve(it)
		switch kind := attributeType(v); {
		case v == nil || x == nil:
			return false
		case kind == "S" && x.S != nil:
			return strings.Contains(*v.S, *x.S)
		case kind == "B" && x.B != nil:
			return bytes.Contains(*v.B, *x.B)
		case kind == "SS" && x.S != nil, kind == "BS" && x.B != nil:
			return members(v)[*attributeKey(x)]
		case kind == "NS" && x.N != nil:
			return members(v)[rat(*x.N).RatString()]
		case kind == "L":
			return slices.ContainsFunc(*v.L, func(e api.AttributeValue) bool { return equal(&e, x) })
		default:
			return false
		}
	}
}

// attributeKey returns a string or binary value in the form members uses.
func attributeKey(v *api.AttributeValue) *string {
	if v.S != nil {
		return v.S
	}

	return new(string(*v.B))
}

// apply returns the attributes of it that the projection selects.
func (pr projection) apply(it item) item {
	if len(pr) == 0 || it == nil {
		return it
	}

	root := &projNode{}
	for _, p := range pr {
		n := root
		for _, s := range p {
			n = n.child(s)
		}

		n.leaf = true
	}

	out := item{}
	for name, child := range root.names {
		if v, ok := it[name]; ok {
			if picked, found := child.extract(v); found {
				out[name] = picked
			}
		}
	}

	return out
}

type projNode struct {
	names   map[string]*projNode
	indexes map[int]*projNode
	leaf    bool
}

func (n *projNode) child(s step) *projNode {
	if s.list {
		if n.indexes == nil {
			n.indexes = map[int]*projNode{}
		}

		if n.indexes[s.index] == nil {
			n.indexes[s.index] = &projNode{}
		}

		return n.indexes[s.index]
	}

	if n.names == nil {
		n.names = map[string]*projNode{}
	}

	if n.names[s.name] == nil {
		n.names[s.name] = &projNode{}
	}

	return n.names[s.name]
}

// extract returns the parts of v under n; list elements keep their order and
// close up, as DynamoDB returns them.
//
//nolint:gocritic // Values are small unions of pointers.
func (n *projNode) extract(v api.AttributeValue) (api.AttributeValue, bool) {
	switch {
	case n.leaf:
		return v, true
	case v.M != nil && len(n.names) > 0:
		out := map[string]api.AttributeValue{}
		for name, child := range n.names {
			if cv, ok := (*v.M)[name]; ok {
				if picked, found := child.extract(cv); found {
					out[name] = picked
				}
			}
		}

		return api.AttributeValue{M: &out}, len(out) > 0
	case v.L != nil && len(n.indexes) > 0:
		var out []api.AttributeValue
		indexes := make([]int, 0, len(n.indexes))
		for i := range n.indexes {
			indexes = append(indexes, i)
		}

		sort.Ints(indexes)
		for _, i := range indexes {
			if i < len(*v.L) {
				if picked, found := n.indexes[i].extract((*v.L)[i]); found {
					out = append(out, picked)
				}
			}
		}

		return api.AttributeValue{L: &out}, len(out) > 0
	default:
		return api.AttributeValue{}, false
	}
}

//nolint:gocritic // Values are small unions of pointers.
func clone(v api.AttributeValue) api.AttributeValue {
	if v.L != nil {
		l := make([]api.AttributeValue, len(*v.L))
		for i, e := range *v.L {
			l[i] = clone(e)
		}

		v.L = &l
	}

	if v.M != nil {
		m := cloneItem(*v.M)
		v.M = &m
	}

	return v
}

func cloneItem(it item) item {
	out := make(item, len(it))
	for k, v := range it {
		out[k] = clone(v)
	}

	return out
}

func incorrectType() error {
	return awserror.Validation("An operand in the update expression has an incorrect data type")
}

// evaluate computes a SET value from the item before the update. It returns
// nil when an operand refers to a missing attribute.
func (o *operand) evaluate(it item) (*api.AttributeValue, error) {
	switch {
	case o.value != nil:
		return o.value, nil
	case o.path != nil:
		return get(it, o.path), nil
	case o.fn == fnIfNotExists:
		if v := get(it, o.args[0].path); v != nil {
			return v, nil
		}

		return o.args[1].evaluate(it)
	}

	a, err := o.args[0].evaluate(it)
	if err != nil || a == nil {
		return nil, err
	}

	b, err := o.args[1].evaluate(it)
	if err != nil || b == nil {
		return nil, err
	}

	if o.fn == fnListAppend {
		if a.L == nil || b.L == nil {
			return nil, incorrectType()
		}

		l := append(slices.Clone(*a.L), *b.L...)
		return &api.AttributeValue{L: &l}, nil
	}

	if a.N == nil || b.N == nil {
		return nil, incorrectType()
	}

	sum := rat(*b.N)
	if o.arith == "-" {
		sum.Neg(sum)
	}

	n, err := formatNumber(sum.Add(sum, rat(*a.N)))
	if err != nil {
		return nil, err
	}

	return &api.AttributeValue{N: &n}, nil
}

// formatNumber writes r exactly; DynamoDB numbers are decimals, so the
// denominator has no factors but 2 and 5.
func formatNumber(r *big.Rat) (string, error) {
	s := r.Num().String()
	if !r.IsInt() {
		d := new(big.Int).Set(r.Denom())
		digits := 0
		for _, f := range []int64{2, 5} {
			n := 0
			for m := new(big.Int); m.Mod(d, big.NewInt(f)).Sign() == 0; n++ {
				d.Quo(d, big.NewInt(f))
			}

			digits = max(digits, n)
		}

		s = strings.TrimRight(strings.TrimRight(r.FloatString(digits), "0"), ".")
	}

	if _, err := number(s); err != nil {
		return "", err
	}

	return s, nil
}

// change sets the value at a path, or removes it when value is nil.
type change struct {
	value *api.AttributeValue
	path  path
}

// apply returns the item the update produces from old, which it leaves as is.
// Every operand reads old, as DynamoDB evaluates them before changing anything.
func (u *update) apply(old item) (item, error) {
	var changes []change
	for _, a := range u.set {
		v, err := a.value.evaluate(old)
		if err != nil {
			return nil, err
		}

		if v == nil {
			return nil, awserror.Validation(
				"The provided expression refers to an attribute that does not exist in the item")
		}

		changes = append(changes, change{path: a.path, value: v})
	}

	for _, a := range u.add {
		v, err := add(get(old, a.path), a.value.value)
		if err != nil {
			return nil, err
		}

		changes = append(changes, change{path: a.path, value: v})
	}

	for _, a := range u.del {
		current := get(old, a.path)
		if current == nil {
			continue
		}

		if attributeType(current) != attributeType(a.value.value) {
			return nil, incorrectType()
		}

		v := without(current, members(a.value.value))
		changes = append(changes, change{path: a.path, value: v})
	}

	for _, p := range u.remove {
		changes = append(changes, change{path: p})
	}

	// Assign in path order, then remove from the highest list index down so
	// earlier removals do not shift later ones.
	slices.SortFunc(changes, func(a, b change) int {
		switch {
		case (a.value == nil) != (b.value == nil) && a.value == nil:
			return 1
		case (a.value == nil) != (b.value == nil):
			return -1
		case a.value == nil:
			return b.path.compare(a.path)
		default:
			return a.path.compare(b.path)
		}
	})
	out := cloneItem(old)
	for _, c := range changes {
		if err := c.write(out); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// add implements ADD: numbers are summed and sets are joined; a missing
// attribute takes the value.
func add(current, v *api.AttributeValue) (*api.AttributeValue, error) {
	switch kind := attributeType(current); {
	case current == nil:
		return v, nil
	case kind != attributeType(v):
		return nil, incorrectType()
	case kind == "N":
		n, err := formatNumber(new(big.Rat).Add(rat(*current.N), rat(*v.N)))
		if err != nil {
			return nil, err
		}

		return &api.AttributeValue{N: &n}, nil
	default:
		joined := clone(*current)
		seen := members(current)
		switch kind {
		case "SS":
			l := slices.Clone(*current.SS)
			for _, s := range *v.SS {
				if !seen[s] {
					l = append(l, s)
					seen[s] = true
				}
			}

			joined.SS = &l
		case "NS":
			l := slices.Clone(*current.NS)
			for _, s := range *v.NS {
				if key := rat(s).RatString(); !seen[key] {
					l = append(l, s)
					seen[key] = true
				}
			}

			joined.NS = &l
		default:
			l := slices.Clone(*current.BS)
			for _, b := range *v.BS {
				if !seen[string(b)] {
					l = append(l, b)
					seen[string(b)] = true
				}
			}

			joined.BS = &l
		}

		return &joined, nil
	}
}

// without removes members from a set; an emptied set is removed (nil).
func without(set *api.AttributeValue, remove map[string]bool) *api.AttributeValue {
	out := api.AttributeValue{}
	n := 0
	switch {
	case set.SS != nil:
		l := slices.DeleteFunc(slices.Clone(*set.SS), func(s string) bool { return remove[s] })
		out.SS, n = &l, len(l)
	case set.NS != nil:
		l := slices.DeleteFunc(slices.Clone(*set.NS), func(s string) bool { return remove[rat(s).RatString()] })
		out.NS, n = &l, len(l)
	default:
		l := slices.DeleteFunc(slices.Clone(*set.BS), func(b []byte) bool { return remove[string(b)] })
		out.BS, n = &l, len(l)
	}

	if n == 0 {
		return nil
	}

	return &out
}

func invalidPath() error {
	return awserror.Validation("The document path provided in the update expression is invalid for update")
}

func (c change) write(it item) error {
	last := c.path[len(c.path)-1]
	if len(c.path) == 1 {
		if c.value == nil {
			delete(it, last.name)
		} else {
			it[last.name] = *c.value
		}

		return nil
	}

	parent := get(it, c.path[:len(c.path)-1])
	switch {
	case parent == nil:
		if c.value == nil {
			return nil
		}

		return invalidPath()
	case last.list && parent.L != nil:
		l := parent.L
		switch {
		case c.value == nil && last.index < len(*l):
			*l = slices.Delete(*l, last.index, last.index+1)
		case c.value == nil:
		case last.index < len(*l):
			(*l)[last.index] = *c.value
		default:
			*l = append(*l, *c.value)
		}
	case !last.list && parent.M != nil:
		if c.value == nil {
			delete(*parent.M, last.name)
		} else {
			(*parent.M)[last.name] = *c.value
		}
	default:
		return invalidPath()
	}

	return nil
}
