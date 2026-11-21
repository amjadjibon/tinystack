package cloudwatch

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	insightLimit        = 500
	insightPeriod       = 60
	insightHistory      = 14 * 24 * time.Hour
	insightAlarmHistory = 3 * time.Hour
)

var insightToken = regexp.MustCompile(`^([a-zA-Z0-9_]+|"(?:[^"\\]|\\["'\\])*"|'(?:[^'\\]|\\["'\\])*'|!=|[(),=;])`)

var insightStats = map[string]string{
	"AVG": statAverage, "COUNT": statCount, "MAX": statMaximum, "MIN": statMinimum, "SUM": statSum,
}

type insightFilter struct {
	key, value string
	notEqual   bool
}

type insightQuery struct {
	stat, metric, namespace string
	order                   string
	schema, group           []string
	filters                 []insightFilter
	descending              bool
	limit                   int
}

// insightParser keeps the first error so grammar productions can read required tokens in sequence.
type insightParser struct {
	err  error
	rest string
}

func insightExpression(src string) bool {
	token := insightToken.FindString(strings.TrimSpace(src))
	return strings.EqualFold(token, "SELECT")
}

func (p *insightParser) peek() string {
	p.rest = strings.TrimSpace(p.rest)
	return insightToken.FindString(p.rest)
}

func (p *insightParser) take() string {
	token := p.peek()
	p.rest = p.rest[len(token):]
	return token
}

func (p *insightParser) accept(token string) bool {
	if !strings.EqualFold(p.peek(), token) {
		return false
	}

	p.take()
	return true
}

func (p *insightParser) fail(expected string) {
	if p.err == nil {
		p.err = invalidValue("Invalid Metrics Insights expression: expected %s near %q", expected, p.rest)
	}
}

func (p *insightParser) require(token string) {
	if !p.accept(token) {
		p.fail(token)
	}
}

func (p *insightParser) aggregate() string {
	if st, ok := insightStats[strings.ToUpper(p.peek())]; ok {
		p.take()
		return st
	}

	p.fail("AVG, COUNT, MAX, MIN, or SUM")
	return ""
}

var insightEscapes = strings.NewReplacer("\\\"", "\"", "\\'", "'", "\\\\", "\\")

func insightUnquote(token string) string {
	return insightEscapes.Replace(token[1 : len(token)-1])
}

func (p *insightParser) identifier() string {
	token := p.peek()
	if len(token) > 2 && token[0] == '"' {
		p.take()
		return insightUnquote(token)
	}

	switch strings.ToUpper(token) {
	case "SELECT", "FROM", "SCHEMA", "WHERE", "AND", "GROUP", "BY", "ORDER", "ASC", "DESC", "LIMIT":
		p.fail("a quoted identifier")
	case "":
		p.fail("an identifier")
	default:
		if isWord(token[0]) {
			p.take()
			return token
		}

		p.fail("an identifier")
	}

	return ""
}

func parseInsight(src string) (insightQuery, error) {
	p := insightParser{rest: src}
	p.require("SELECT")
	q := insightQuery{stat: p.aggregate(), limit: insightLimit}
	p.require("(")
	q.metric = p.identifier()
	p.require(")")
	p.require("FROM")
	if p.accept("SCHEMA") {
		p.require("(")
		q.namespace = p.identifier()
		q.schema = []string{}
		for p.accept(",") {
			q.schema = append(q.schema, p.identifier())
		}

		p.require(")")
	} else {
		q.namespace = p.identifier()
	}

	if p.accept("WHERE") {
		for {
			filter := insightFilter{key: p.identifier(), notEqual: p.accept("!=")}
			if !filter.notEqual {
				p.require("=")
			}

			token := p.peek()
			if len(token) >= 2 && token[0] == '\'' {
				filter.value = insightUnquote(p.take())
			} else {
				p.fail("a single-quoted dimension value")
			}

			q.filters = append(q.filters, filter)
			if !p.accept("AND") {
				break
			}
		}
	}

	if p.accept("GROUP") {
		p.require("BY")
		q.group = append(q.group, p.identifier())
		for p.accept(",") {
			q.group = append(q.group, p.identifier())
		}
	}

	if p.accept("ORDER") {
		p.require("BY")
		q.order = p.aggregate()
		p.require("(")
		p.require(")")
		q.descending = p.accept("DESC")
		if !q.descending {
			p.accept("ASC")
		}
	}

	if p.accept("LIMIT") {
		n, err := strconv.Atoi(p.peek())
		if err != nil || n < 1 || n > insightLimit {
			p.fail("a LIMIT between 1 and 500")
		} else {
			p.take()
			q.limit = n
		}
	}

	p.accept(";")
	if strings.TrimSpace(p.rest) != "" {
		p.fail("the end of the query")
	}

	if p.err != nil {
		return q, p.err
	}

	for _, names := range [][]string{q.schema, q.group} {
		if len(slices.Compact(slices.Sorted(slices.Values(names)))) != len(names) {
			return q, invalidValue("Duplicate dimension in Metrics Insights SCHEMA or GROUP BY")
		}
	}

	// AWS.AccountId and tag.* are reserved label keys even when quoted.
	keys := slices.Concat(q.schema, q.group)
	for _, f := range q.filters {
		keys = append(keys, f.key)
	}

	for _, key := range keys {
		if key == "AWS.AccountId" || strings.HasPrefix(key, "tag.") {
			return q, invalidValue("TinyStack does not support account or tag labels in Metrics Insights")
		}
	}

	return q, nil
}

func (q *insightQuery) matches(dims map[string]string) bool {
	if q.schema != nil {
		if len(dims) != len(q.schema) {
			return false
		}

		for _, key := range q.schema {
			if _, ok := dims[key]; !ok {
				return false
			}
		}
	}

	for _, f := range q.filters {
		v, ok := dims[f.key]
		if !ok || (v == f.value) == f.notEqual {
			return false
		}
	}

	return true
}

type insightGroup struct {
	buckets map[int64]*bucket
	values  []string
}

func insightGroupKey(labels []string, dims map[string]string) (groupKey string, values []string) {
	values = make([]string, len(labels))
	var key strings.Builder
	for i, label := range labels {
		v, ok := dims[label]
		values[i] = v
		key.WriteString(strconv.Quote(v))
		if !ok {
			values[i] = "Other"
		}
	}

	return key.String(), values
}

func (q *insightQuery) run(
	tx storage.ReadTx,
	scope request.Scope,
	period int64,
	start, end time.Time,
) (operand, error) {
	groups := map[string]*insightGroup{}
	if len(q.group) == 0 {
		groups[""] = &insightGroup{buckets: map[int64]*bucket{}}
	}

	prefix := scope.Prefix(service, kindMetric)
	for _, key := range tx.Keys(prefix) {
		m, _, err := storage.Read[metric](tx, key)
		if err != nil {
			return operand{}, err
		}

		if m.Namespace != q.namespace || m.Name != q.metric {
			continue
		}

		dims := make(map[string]string, len(m.Dimensions))
		for _, d := range m.Dimensions {
			dims[*d.Name] = *d.Value
		}

		if !q.matches(dims) {
			continue
		}

		got, err := buckets(tx, scope, strings.TrimPrefix(key, prefix), period, "", start, end, false)
		if err != nil {
			return operand{}, err
		}

		if len(got) == 0 {
			continue
		}

		groupKey, values := insightGroupKey(q.group, dims)
		group := groups[groupKey]
		if group == nil {
			group = &insightGroup{values: values, buckets: map[int64]*bucket{}}
			groups[groupKey] = group
		}

		for at, b := range got {
			if aggregate := group.buckets[at]; aggregate != nil {
				aggregate.addStats(b.count, b.sum, b.min, b.max, b.unit)
			} else {
				group.buckets[at] = b
			}
		}
	}

	return q.result(groups), nil
}

func (q *insightQuery) result(groups map[string]*insightGroup) operand {
	f, _ := statistic(q.stat)
	order, _ := statistic(q.order)
	type ranked struct {
		series *series
		values []string
		rank   float64
	}

	rows := make([]ranked, 0, len(groups))
	for _, key := range slices.Sorted(maps.Keys(groups)) {
		group := groups[key]
		label := q.metric
		if len(group.values) > 0 {
			label = strings.Join(group.values, ", ")
		}

		row := ranked{series: &series{label: label, points: map[int64]float64{}}, values: group.values}
		summary := &bucket{}
		for _, at := range slices.Sorted(maps.Keys(group.buckets)) {
			v, _ := f(group.buckets[at])
			row.series.points[at] = v
			summary.addStats(1, v, v, v, "")
		}

		if order != nil && summary.count > 0 {
			row.rank, _ = order(summary)
		}

		rows = append(rows, row)
	}

	slices.SortStableFunc(rows, func(a, b ranked) int {
		c := cmp.Compare(a.rank, b.rank)
		if q.descending {
			c = -c
		}

		if c == 0 {
			c = slices.Compare(a.values, b.values)
		}

		return c
	})
	out := operand{list: make([]*series, 0, min(len(rows), q.limit)), array: len(q.group) > 0}
	for _, row := range rows[:min(len(rows), q.limit)] {
		out.list = append(out.list, row.series)
	}

	return out
}

func insightCutoff(now time.Time, alarm bool) time.Time {
	history := insightHistory
	if alarm {
		history = insightAlarmHistory
	}

	return now.Add(-history)
}
