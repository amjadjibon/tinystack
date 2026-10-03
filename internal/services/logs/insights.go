package logs

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// This file runs a subset of the CloudWatch Logs Insights query language:
//
//	fields a, b         the fields to show
//	display a, b        the fields to show, after other commands
//	filter <condition>  =, !=, <, <=, >, >=, like and not like (a "substring" or
//	                    a /regular expression/ with an optional i flag), in [..],
//	                    ispresent(f), combined with and, or, not, and parentheses
//	parse f "a * b * c" as x, y   a glob, or parse f /(?<x>..)/ with named groups
//	sort f [asc|desc], limit n
//	stats count(*), count(f), count_distinct(f), sum(f), avg(f), min(f), max(f)
//	      [as name], ... [by f, bin(5m), ...]
//
// Every event has @timestamp, @message, @logStream, @log, and @ingestionTime,
// and the top-level and nested fields of a JSON message (a.b for nesting).

const (
	insightsTime   = "2006-01-02 15:04:05.000"
	defaultResults = 1000
	maxResults     = 10000
)

// Command kinds, and the names more than one place uses.
const (
	cmdFields      = "fields"
	cmdFilter      = "filter"
	cmdSort        = "sort"
	cmdLimit       = "limit"
	cmdStats       = "stats"
	cmdParse       = "parse"
	fnCount        = "count"
	fieldTimestamp = "@timestamp"
)

func malformed(format string, args ...any) error {
	return awserror.New("MalformedQueryException", fmt.Sprintf(format, args...))
}

// row is one result row: field names to values.
type row map[string]string

// token is one lexical element of a query.
type token struct {
	text string
	kind byte // 'w' word, 's' string, 'r' regular expression, 'n' number, 'p' punctuation
}

func lex(query string) ([]token, error) {
	var out []token
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '"' || c == '\'' || c == '`':
			end := strings.IndexByte(query[i+1:], c)
			if end < 0 {
				return nil, malformed("Query has an unterminated string")
			}

			out = append(out, token{query[i+1 : i+1+end], 's'})
			i += end + 2
		case c == '/':
			// A regular expression ends at the first / that no \ escapes.
			end := i + 1
			for end < len(query) && query[end] != '/' {
				if query[end] == '\\' {
					end++
				}

				end++
			}

			if end >= len(query) {
				return nil, malformed("Query has an unterminated regular expression")
			}

			pattern := query[i+1 : end]
			i = end + 1
			if i < len(query) && query[i] == 'i' {
				pattern = "(?i)" + pattern
				i++
			}

			out = append(out, token{pattern, 'r'})
		case strings.ContainsRune("|(),[]*", rune(c)):
			out = append(out, token{string(c), 'p'})
			i++
		case strings.ContainsRune("=!<>", rune(c)):
			j := i + 1
			if j < len(query) && query[j] == '=' {
				j++
			}

			out = append(out, token{query[i:j], 'p'})
			i = j
		case c == '-' || c == '.' || unicode.IsDigit(rune(c)):
			j := i + 1
			for j < len(query) && (unicode.IsDigit(rune(query[j])) || query[j] == '.') {
				j++
			}

			out = append(out, token{query[i:j], 'n'})
			i = j
		default:
			j := i
			for j < len(query) && (unicode.IsLetter(rune(query[j])) || unicode.IsDigit(rune(query[j])) ||
				strings.ContainsRune("@_.$-", rune(query[j]))) {
				j++
			}

			if j == i {
				return nil, malformed("Query has an unexpected character %q", c)
			}

			out = append(out, token{query[i:j], 'w'})
			i = j
		}
	}

	return out, nil
}

// parser walks one command's tokens.
type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}

	return token{}
}

func (p *parser) next() token {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) done() bool { return p.pos >= len(p.tokens) }

// keyword reports, and consumes, a case-insensitive keyword.
func (p *parser) keyword(word string) bool {
	if t := p.peek(); t.kind == 'w' && strings.EqualFold(t.text, word) {
		p.pos++
		return true
	}

	return false
}

func (p *parser) punct(s string) bool {
	if t := p.peek(); t.kind == 'p' && t.text == s {
		p.pos++
		return true
	}

	return false
}

func (p *parser) field() (string, error) {
	t := p.next()
	if t.kind != 'w' {
		return "", malformed("Query expected a field name, not %q", t.text)
	}

	return t.text, nil
}

// fieldList parses "a, b, c".
func (p *parser) fieldList() ([]string, error) {
	var names []string
	for {
		name, err := p.field()
		if err != nil {
			return nil, err
		}

		names = append(names, name)
		if !p.punct(",") {
			return names, nil
		}
	}
}

// condition is a filter expression.
type condition func(row) bool

func (p *parser) or() (condition, error) {
	left, err := p.and()
	for err == nil && p.keyword("or") {
		var right condition
		if right, err = p.and(); err == nil {
			l := left
			left = func(r row) bool { return l(r) || right(r) }
		}
	}

	return left, err
}

func (p *parser) and() (condition, error) {
	left, err := p.not()
	for err == nil && p.keyword("and") {
		var right condition
		if right, err = p.not(); err == nil {
			l := left
			left = func(r row) bool { return l(r) && right(r) }
		}
	}

	return left, err
}

func (p *parser) not() (condition, error) {
	if p.keyword("not") {
		inner, err := p.not()
		if err != nil {
			return nil, err
		}

		return func(r row) bool { return !inner(r) }, nil
	}

	return p.comparison()
}

// operand is a literal or a field's value.
func (p *parser) operand() (func(row) (string, bool), error) {
	t := p.next()
	switch t.kind {
	case 's', 'n':
		return func(row) (string, bool) { return t.text, true }, nil
	case 'w':
		return func(r row) (string, bool) { v, ok := r[t.text]; return v, ok }, nil
	}

	return nil, malformed("Query expected a value, not %q", t.text)
}

func (p *parser) comparison() (condition, error) {
	if p.punct("(") {
		inner, err := p.or()
		if err == nil && !p.punct(")") {
			err = malformed("Query is missing a )")
		}

		return inner, err
	}

	if p.keyword("ispresent") {
		if !p.punct("(") {
			return nil, malformed("ispresent needs a field in parentheses")
		}

		name, err := p.field()
		if err != nil || !p.punct(")") {
			return nil, malformed("ispresent needs a field in parentheses")
		}

		return func(r row) bool { _, ok := r[name]; return ok }, nil
	}

	name, err := p.field()
	if err != nil {
		return nil, err
	}

	negate := p.keyword("not")
	switch {
	case p.keyword("like"):
		t := p.next()
		var match func(string) bool
		switch t.kind {
		case 'r':
			re, reErr := regexp.Compile(t.text)
			if reErr != nil {
				return nil, malformed("Query has an invalid regular expression: %v", reErr)
			}

			match = re.MatchString
		case 's':
			match = func(v string) bool { return strings.Contains(v, t.text) }
		default:
			return nil, malformed("like needs a string or a regular expression")
		}

		return func(r row) bool { v, ok := r[name]; return ok && match(v) != negate }, nil
	case p.keyword("in"):
		if !p.punct("[") {
			return nil, malformed("in needs a [list]")
		}

		var values []string
		for !p.punct("]") {
			if p.done() {
				return nil, malformed("in needs a [list]")
			}

			if t := p.next(); t.kind != 'p' {
				values = append(values, t.text)
			}
		}

		return func(r row) bool { v, ok := r[name]; return ok && slices.Contains(values, v) != negate }, nil
	case negate:
		return nil, malformed("not must come before like, in, or a condition")
	}

	op := p.next()
	if op.kind != 'p' || !slices.Contains([]string{"=", "==", "!=", "<", "<=", ">", ">="}, op.text) {
		return nil, malformed("Query expected a comparison, not %q", op.text)
	}

	right, err := p.operand()
	if err != nil {
		return nil, err
	}

	return func(r row) bool {
		left, ok := r[name]
		value, known := right(r)
		if !ok || !known {
			return op.text == "!="
		}

		c := compare(left, value)
		switch op.text {
		case "=", "==":
			return c == 0
		case "!=":
			return c != 0
		case "<":
			return c < 0
		case "<=":
			return c <= 0
		case ">":
			return c > 0
		}

		return c >= 0
	}, nil
}

// compare orders two values as numbers when both are, and else as text.
func compare(a, b string) int {
	x, errA := strconv.ParseFloat(a, 64)
	y, errB := strconv.ParseFloat(b, 64)
	if errA == nil && errB == nil {
		return cmp.Compare(x, y)
	}

	return strings.Compare(a, b)
}

// aggregate is one stats function.
type aggregate struct {
	fn, field, name string
}

// grouping is one stats "by" key: a field, or bin(duration) of @timestamp.
type grouping struct {
	field, name string
	bin         time.Duration
}

// command is one step of a query.
type command struct {
	filter  condition
	parse   *regexp.Regexp
	from    string
	sortBy  string
	kind    string
	stats   []aggregate
	by      []grouping
	fields  []string
	limit   int
	desc    bool
	display bool
}

var units = map[string]time.Duration{
	"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
	"d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
}

// binSize parses bin's duration, such as 5m or 1h.
func binSize(text string) (time.Duration, error) {
	i := strings.IndexFunc(text, func(r rune) bool { return !unicode.IsDigit(r) })
	n, err := strconv.Atoi(text[:max(i, 0)])
	unit, ok := units[strings.ToLower(text[max(i, 0):])]
	if i <= 0 || err != nil || n <= 0 || !ok {
		return 0, malformed("bin needs a duration such as 5m, not %q", text)
	}

	return time.Duration(n) * unit, nil
}

func (p *parser) stats() (*command, error) {
	c := &command{kind: cmdStats}
	for {
		fn := p.next()
		if fn.kind != 'w' || !p.punct("(") {
			return nil, malformed("stats needs functions such as count(*)")
		}

		a := aggregate{fn: strings.ToLower(fn.text)}
		if !slices.Contains([]string{fnCount, "count_distinct", "sum", "avg", "min", "max"}, a.fn) {
			return nil, malformed("TinyStack does not support the stats function %s", fn.text)
		}

		if p.punct("*") {
			if a.fn != fnCount {
				return nil, malformed("Only count takes *")
			}
		} else {
			var err error
			if a.field, err = p.field(); err != nil {
				return nil, err
			}
		}

		if !p.punct(")") {
			return nil, malformed("stats function is missing a )")
		}

		a.name = a.fn + "(" + cmp.Or(a.field, "*") + ")"
		if p.keyword("as") {
			alias, err := p.field()
			if err != nil {
				return nil, err
			}

			a.name = alias
		}

		c.stats = append(c.stats, a)
		if !p.punct(",") {
			break
		}
	}

	if !p.keyword("by") {
		return c, nil
	}

	for {
		var g grouping
		if p.keyword("bin") {
			if !p.punct("(") {
				return nil, malformed("bin needs a duration in parentheses")
			}

			// "5m" lexes as the number 5 and the word m.
			text := p.next().text + p.next().text
			size, err := binSize(text)
			if err != nil {
				return nil, err
			}

			if !p.punct(")") {
				return nil, malformed("bin is missing a )")
			}

			g = grouping{field: fieldTimestamp, name: "bin(" + text + ")", bin: size}
		} else {
			name, err := p.field()
			if err != nil {
				return nil, err
			}

			g = grouping{field: name, name: name}
		}

		if p.keyword("as") {
			alias, err := p.field()
			if err != nil {
				return nil, err
			}

			g.name = alias
		}

		c.by = append(c.by, g)
		if !p.punct(",") {
			return c, nil
		}
	}
}

// globPattern turns parse's glob, where each * captures a field, into a
// regular expression with named groups.
func globPattern(glob string, names []string) (*regexp.Regexp, error) {
	parts := strings.Split(glob, "*")
	if len(parts)-1 != len(names) {
		return nil, malformed("parse has %d * captures but %d names", len(parts)-1, len(names))
	}

	var b strings.Builder
	for i, part := range parts {
		b.WriteString(regexp.QuoteMeta(part))
		if i < len(names) {
			b.WriteString("(?P<" + names[i] + ">.*?)")
		}
	}

	return regexp.Compile(b.String() + "$")
}

func parseCommand(tokens []token) (*command, error) {
	p := &parser{tokens: tokens}
	head := strings.ToLower(p.next().text)
	var c *command
	var err error
	switch head {
	case cmdFields, "display":
		c = &command{kind: cmdFields, display: head == "display"}
		c.fields, err = p.fieldList()
	case cmdFilter:
		c = &command{kind: cmdFilter}
		c.filter, err = p.or()
	case cmdSort:
		c = &command{kind: cmdSort}
		if c.sortBy, err = p.field(); err == nil {
			c.desc = p.keyword("desc")
			if !c.desc {
				p.keyword("asc")
			}
		}
	case cmdLimit:
		c = &command{kind: cmdLimit}
		t := p.next()
		if c.limit, err = strconv.Atoi(t.text); err != nil || c.limit < 1 || c.limit > maxResults {
			err = malformed("limit needs a number from 1 to %d", maxResults)
		}
	case cmdStats:
		c, err = p.stats()
	case cmdParse:
		c = &command{kind: cmdParse}
		if c.from, err = p.field(); err != nil {
			break
		}

		pattern := p.next()
		switch {
		case pattern.kind == 'r':
			c.parse, err = regexp.Compile(strings.ReplaceAll(pattern.text, "(?<", "(?P<"))
		case pattern.kind == 's' && p.keyword("as"):
			var names []string
			if names, err = p.fieldList(); err == nil {
				c.parse, err = globPattern(pattern.text, names)
			}
		default:
			err = malformed("parse needs a \"glob\" with as names, or a /regular expression/")
		}
	default:
		return nil, malformed("TinyStack does not support the %q query command", head)
	}

	if err == nil && !p.done() {
		err = malformed("Query has unexpected %q after %s", p.peek().text, head)
	}

	return c, err
}

// compileQuery splits a query into its commands.
func compileQuery(query string) ([]*command, error) {
	tokens, err := lex(query)
	if err != nil {
		return nil, err
	}

	var commands []*command
	for start := 0; start <= len(tokens); {
		end := start
		for end < len(tokens) && (tokens[end].kind != 'p' || tokens[end].text != "|") {
			end++
		}

		if end == start {
			return nil, malformed("Query has an empty command")
		}

		c, parseErr := parseCommand(tokens[start:end])
		if parseErr != nil {
			return nil, parseErr
		}

		commands = append(commands, c)
		start = end + 1
	}

	return commands, nil
}

// eventRow is an event's fields: the system fields and, for a JSON object
// message, its fields.
func eventRow(account, group, stream string, e *event) row {
	r := row{
		fieldTimestamp: time.UnixMilli(e.Timestamp).UTC().Format(insightsTime), "@message": e.Message,
		"@logStream": stream, "@log": account + ":" + group,
		"@ingestionTime": time.UnixMilli(e.Ingestion).UTC().Format(insightsTime),
	}
	var doc map[string]any
	if json.Unmarshal([]byte(e.Message), &doc) == nil {
		flatten(r, "", doc)
	}

	return r
}

func flatten(r row, prefix string, doc map[string]any) {
	for k, v := range doc {
		switch v := v.(type) {
		case map[string]any:
			flatten(r, prefix+k+".", v)
		case string:
			r[prefix+k] = v
		case nil:
		default:
			data, _ := json.Marshal(v)
			r[prefix+k] = string(data)
		}
	}
}

// run applies commands to rows. It returns the result, its fields in order,
// and how many rows matched the filters that came before any stats.
func run(commands []*command, rows []row, limit int) (result []row, fields []string, matched int) {
	fields = []string{fieldTimestamp, "@message"}
	grouped := false
	for _, c := range commands {
		switch c.kind {
		case cmdFields:
			fields = c.fields
		case cmdFilter:
			rows = slices.DeleteFunc(rows, func(r row) bool { return !c.filter(r) })
		case cmdParse:
			for _, r := range rows {
				m := c.parse.FindStringSubmatch(r[c.from])
				for i, name := range c.parse.SubexpNames() {
					if name != "" && m != nil {
						r[name] = m[i]
					}
				}
			}
		case cmdSort:
			slices.SortStableFunc(rows, func(a, b row) int {
				if c.desc {
					a, b = b, a
				}

				return compare(a[c.sortBy], b[c.sortBy])
			})
		case cmdLimit:
			limit = min(limit, c.limit)
		case cmdStats:
			if !grouped {
				matched, grouped = len(rows), true
			}

			rows, fields = stats(c, rows)
		}
	}

	if !grouped {
		matched = len(rows)
	}

	return rows[:min(len(rows), limit)], fields, matched
}

// stats groups rows by c's keys and computes its aggregates for each group,
// in key order.
func stats(c *command, rows []row) (result []row, fields []string) {
	type group struct {
		key  row
		rows []row
	}

	var order []string
	groups := map[string]*group{}
	for _, r := range rows {
		key, id := row{}, ""
		for _, g := range c.by {
			v := r[g.field]
			if g.bin > 0 {
				t, err := time.Parse(insightsTime, v)
				if err != nil {
					continue
				}

				v = t.Truncate(g.bin).Format(insightsTime)
			}

			key[g.name] = v
			id += v + "\x00"
		}

		if groups[id] == nil {
			groups[id] = &group{key: key}
			order = append(order, id)
		}

		groups[id].rows = append(groups[id].rows, r)
	}

	slices.Sort(order)
	for _, g := range c.by {
		fields = append(fields, g.name)
	}

	for _, a := range c.stats {
		fields = append(fields, a.name)
	}

	result = make([]row, 0, len(order))
	for _, id := range order {
		g := groups[id]
		r := g.key
		for _, a := range c.stats {
			r[a.name] = aggregateOf(a, g.rows)
		}

		result = append(result, r)
	}

	return result, fields
}

func aggregateOf(a aggregate, rows []row) string {
	var values []float64
	distinct := map[string]bool{}
	count := 0
	for _, r := range rows {
		v, ok := r[a.field]
		if a.field != "" && !ok {
			continue
		}

		count++
		distinct[v] = true
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			values = append(values, f)
		}
	}

	format := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	switch a.fn {
	case fnCount:
		return strconv.Itoa(count)
	case "count_distinct":
		return strconv.Itoa(len(distinct))
	}

	if len(values) == 0 {
		return ""
	}

	sum, low, high := 0.0, math.Inf(1), math.Inf(-1)
	for _, v := range values {
		sum, low, high = sum+v, min(low, v), max(high, v)
	}

	switch a.fn {
	case "sum":
		return format(sum)
	case "avg":
		return format(sum / float64(len(values)))
	case "min":
		return format(low)
	}

	return format(high)
}

// queryLimit is how many rows a query returns: StartQuery's limit, or 1000.
func queryLimit(limit *int64) (int, error) {
	if limit == nil {
		return defaultResults, nil
	}

	if *limit < 1 || *limit > maxResults {
		return 0, invalid("limit must be from 1 to %d", maxResults)
	}

	return int(*limit), nil
}
