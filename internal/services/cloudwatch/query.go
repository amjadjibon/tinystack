package cloudwatch

import (
	"context"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxQueries      = 500
	scanAscending   = "TimestampAscending"
	scanDescending  = "TimestampDescending"
	statusComplete  = "Complete"
	statusPartial   = "PartialData"
	combinationCode = "InvalidParameterCombinationException"
)

var queryID = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*$`)

// series is a time series keyed by the epoch second each period starts.
type series struct {
	points map[int64]float64
	label  string
}

// checkQueries checks metric data queries. An alarm's queries must return
// exactly one series; ReturnData defaults to true.
func checkQueries(queries []api.MetricDataQuery, alarm bool) error {
	switch {
	case len(queries) == 0:
		return missing("MetricDataQueries")
	case len(queries) > maxQueries:
		return invalidValue("At most %d metric data queries are allowed", maxQueries)
	}

	ids, returned := map[string]bool{}, 0
	for i := range queries {
		q := &queries[i]
		id := common.Value(q.Id)
		switch {
		case !queryID.MatchString(id) || len(id) > 255:
			return invalidValue("The value %s for parameter MetricDataQueries.member.%d.Id is invalid.", id, i+1)
		case ids[id]:
			return invalidValue("The value %s for parameter MetricDataQueries.member.%d.Id is not unique.", id, i+1)
		case (q.MetricStat == nil) == (q.Expression == nil):
			return awserror.New(combinationCode, "Each query must have exactly one of MetricStat and Expression")
		case q.Period != nil:
			return invalidValue("TinyStack does not support Metrics Insights queries (Period on a query)")
		}

		ids[id] = true
		if common.Value(q.ReturnData) || q.ReturnData == nil {
			returned++
		}

		if st := q.MetricStat; st != nil {
			if err := checkStat(st, alarm); err != nil {
				return err
			}
		}
	}

	if alarm && returned != 1 {
		return invalidValue("Exactly one metric data query of an alarm must return data")
	}

	return nil
}

func checkStat(st *api.MetricStat, alarm bool) error {
	switch {
	case st.Metric == nil || common.Value(st.Metric.Namespace) == "" || common.Value(st.Metric.MetricName) == "":
		return missing("MetricStat.Metric.Namespace and MetricName")
	case st.Period == nil:
		return missing("MetricStat.Period")
	case common.Value(st.Stat) == "":
		return missing("MetricStat.Stat")
	case st.Unit != nil && !units[*st.Unit]:
		return invalidValue("The value %s for parameter Unit is invalid.", *st.Unit)
	}

	if err := checkPeriod(*st.Period, alarm); err != nil {
		return err
	}

	if _, err := statistic(*st.Stat); err != nil {
		return err
	}

	return checkDimensions(st.Metric.Dimensions)
}

// stat computes one metric's statistic over [start, end).
func stat(tx storage.ReadTx, scope request.Scope, st *api.MetricStat, start, end time.Time) (*series, error) {
	f, err := statistic(*st.Stat)
	if err != nil {
		return nil, err
	}

	mt := st.Metric
	id := metricID(common.Value(mt.Namespace), common.Value(mt.MetricName), mt.Dimensions)
	got, err := buckets(tx, scope, id, *st.Period, common.Value(st.Unit), start, end)
	if err != nil {
		return nil, err
	}

	out := &series{label: common.Value(mt.MetricName), points: map[int64]float64{}}
	for at, b := range got {
		if v, ok := f(b); ok {
			out.points[at] = v
		}
	}

	return out, nil
}

// run evaluates checked queries over [start, end), keyed by query ID.
func run(tx storage.ReadTx, scope request.Scope, queries []api.MetricDataQuery, start, end time.Time,
) (map[string]*series, error) {
	byID := map[string]*api.MetricDataQuery{}
	out := map[string]*series{}
	var metrics []*series
	for i := range queries {
		q := &queries[i]
		byID[*q.Id] = q
		if q.MetricStat == nil {
			continue
		}

		sr, err := stat(tx, scope, q.MetricStat, start, end)
		if err != nil {
			return nil, err
		}

		if q.Label != nil {
			sr.label = *q.Label
		}

		out[*q.Id] = sr
		metrics = append(metrics, sr)
	}

	visiting := map[string]bool{}
	var resolve func(id string) (*series, error)
	resolve = func(id string) (*series, error) {
		if sr, ok := out[id]; ok {
			return sr, nil
		}

		q, ok := byID[id]
		if !ok {
			return nil, invalidValue("Unknown metric data query ID %s in an expression", id)
		}

		if visiting[id] {
			return nil, invalidValue("The expression of %s refers to itself", id)
		}

		visiting[id] = true
		e := &expression{src: *q.Expression, resolve: resolve, metrics: metrics}
		sr, err := e.evaluate()
		if err != nil {
			return nil, err
		}

		sr.label = id
		if q.Label != nil {
			sr.label = *q.Label
		}

		out[id] = sr
		return sr, nil
	}

	for i := range queries {
		if _, err := resolve(*queries[i].Id); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (s *Service) GetMetricData(ctx context.Context, in *api.GetMetricDataInput) (*api.GetMetricDataOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scan := common.Value(in.ScanBy)
	switch {
	case in.StartTime == nil:
		return nil, missing("StartTime")
	case in.EndTime == nil:
		return nil, missing("EndTime")
	case !(*in.StartTime < *in.EndTime):
		return nil, invalidValue("The parameter StartTime must be less than the parameter EndTime.")
	case scan != "" && scan != scanAscending && scan != scanDescending:
		return nil, invalidValue("The value %s for parameter ScanBy is invalid.", scan)
	case in.NextToken != nil:
		return nil, awserror.New("InvalidNextToken", "The service returned no next token to continue from.")
	case in.MaxDatapoints != nil && *in.MaxDatapoints <= 0:
		return nil, invalidValue("The parameter MaxDatapoints must be greater than 0.")
	}

	if checkErr := checkQueries(in.MetricDataQueries, false); checkErr != nil {
		return nil, checkErr
	}

	var got map[string]*series
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		got, err = run(tx, m.Scope, in.MetricDataQueries, fromUnix(*in.StartTime), fromUnix(*in.EndTime))
		return err
	})
	if err != nil {
		return nil, err
	}

	limit := int64(defaultPoints)
	if in.MaxDatapoints != nil {
		limit = *in.MaxDatapoints
	}

	out := &api.GetMetricDataOutput{MetricDataResults: []api.MetricDataResult{}}
	for i := range in.MetricDataQueries {
		q := &in.MetricDataQueries[i]
		if q.ReturnData != nil && !*q.ReturnData {
			continue
		}

		sr := got[*q.Id]
		times := slices.Sorted(maps.Keys(sr.points))
		if scan != scanAscending {
			slices.Reverse(times)
		}

		status := statusComplete
		if int64(len(times)) > limit {
			times, status = times[:limit], statusPartial
		}

		limit -= int64(len(times))
		r := api.MetricDataResult{
			Id: q.Id, Label: new(sr.label), StatusCode: new(status), Timestamps: []float64{}, Values: []float64{},
		}
		for _, t := range times {
			r.Timestamps = append(r.Timestamps, float64(t))
			r.Values = append(r.Values, sr.points[t])
		}

		out.MetricDataResults = append(out.MetricDataResults, r)
	}

	return out, nil
}

// expression evaluates a metric math expression: numbers, query IDs, the
// arithmetic operators, parentheses, [a, b] arrays, METRICS([filter]), and
// SUM, AVG, MIN, MAX, and ABS.
type expression struct {
	resolve func(id string) (*series, error)
	src     string
	metrics []*series
	pos     int
}

// operand is a scalar, one series, or an array of series.
type operand struct {
	list   []*series
	scalar float64
	array  bool
}

func (o operand) isScalar() bool { return o.list == nil }

func (e *expression) fail(format string, args ...any) error {
	return invalidValue("Invalid metric math expression %q: "+format, append([]any{e.src}, args...)...)
}

func (e *expression) evaluate() (*series, error) {
	v, err := e.sum()
	if err != nil {
		return nil, err
	}

	e.skip()
	if e.pos != len(e.src) {
		return nil, e.fail("unexpected %q", e.src[e.pos:])
	}

	if v.isScalar() || len(v.list) != 1 {
		return nil, e.fail("the expression must return one time series")
	}

	return &series{points: v.list[0].points}, nil
}

func (e *expression) skip() {
	for e.pos < len(e.src) && e.src[e.pos] == ' ' {
		e.pos++
	}
}

// accept consumes c after spaces.
func (e *expression) accept(c byte) bool {
	e.skip()
	if e.pos < len(e.src) && e.src[e.pos] == c {
		e.pos++
		return true
	}

	return false
}

func (e *expression) sum() (operand, error) {
	left, err := e.product()
	for err == nil {
		op := byte('+')
		if !e.accept('+') {
			if !e.accept('-') {
				return left, nil
			}

			op = '-'
		}

		var right operand
		if right, err = e.product(); err == nil {
			left, err = e.binary(left, right, op)
		}
	}

	return operand{}, err
}

func (e *expression) product() (operand, error) {
	left, err := e.unary()
	for err == nil {
		op := byte('*')
		if !e.accept('*') {
			if !e.accept('/') {
				return left, nil
			}

			op = '/'
		}

		var right operand
		if right, err = e.unary(); err == nil {
			left, err = e.binary(left, right, op)
		}
	}

	return operand{}, err
}

func (e *expression) unary() (operand, error) {
	if e.accept('-') {
		v, err := e.unary()
		if err != nil {
			return operand{}, err
		}

		return e.binary(operand{scalar: 0}, v, '-')
	}

	return e.primary()
}

//nolint:gocyclo,cyclop // One case per token kind.
func (e *expression) primary() (operand, error) {
	e.skip()
	if e.pos == len(e.src) {
		return operand{}, e.fail("unexpected end")
	}

	c := e.src[e.pos]
	switch {
	case c == '(':
		e.pos++
		v, err := e.sum()
		if err != nil {
			return operand{}, err
		}

		if !e.accept(')') {
			return operand{}, e.fail("missing )")
		}

		return v, nil
	case c == '[':
		e.pos++
		out := operand{list: []*series{}, array: true}
		for !e.accept(']') {
			if len(out.list) > 0 && !e.accept(',') {
				return operand{}, e.fail("missing , or ]")
			}

			v, err := e.sum()
			if err != nil {
				return operand{}, err
			}

			if v.isScalar() {
				return operand{}, e.fail("arrays hold time series")
			}

			out.list = append(out.list, v.list...)
		}

		return out, nil
	case c >= '0' && c <= '9' || c == '.':
		start := e.pos
		for e.pos < len(e.src) && (e.src[e.pos] >= '0' && e.src[e.pos] <= '9' || e.src[e.pos] == '.') {
			e.pos++
		}

		f, err := strconv.ParseFloat(e.src[start:e.pos], 64)
		if err != nil {
			return operand{}, e.fail("bad number %q", e.src[start:e.pos])
		}

		return operand{scalar: f}, nil
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		start := e.pos
		for e.pos < len(e.src) && isWord(e.src[e.pos]) {
			e.pos++
		}

		name := e.src[start:e.pos]
		if e.accept('(') {
			return e.call(name)
		}

		sr, err := e.resolve(name)
		if err != nil {
			return operand{}, err
		}

		return operand{list: []*series{sr}}, nil
	}

	return operand{}, e.fail("unexpected %q", string(c))
}

func isWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func (e *expression) call(name string) (operand, error) {
	if name == "METRICS" {
		filter := ""
		if e.accept('"') {
			end := strings.IndexByte(e.src[e.pos:], '"')
			if end < 0 {
				return operand{}, e.fail("unterminated string")
			}

			filter, e.pos = e.src[e.pos:e.pos+end], e.pos+end+1
		}

		if !e.accept(')') {
			return operand{}, e.fail("missing )")
		}

		out := operand{list: []*series{}, array: true}
		for _, sr := range e.metrics {
			if strings.Contains(sr.label, filter) {
				out.list = append(out.list, sr)
			}
		}

		return out, nil
	}

	arg, err := e.sum()
	if err != nil {
		return operand{}, err
	}

	if !e.accept(')') {
		return operand{}, e.fail("missing )")
	}

	var fold func([]float64) float64
	switch name {
	case "SUM":
		fold = total
	case "AVG":
		fold = func(v []float64) float64 { return total(v) / float64(len(v)) }
	case "MIN":
		fold = slices.Min[[]float64]
	case "MAX":
		fold = slices.Max[[]float64]
	case "ABS":
		return e.binary(arg, operand{scalar: 0}, 'a')
	default:
		return operand{}, e.fail("TinyStack does not support the function %s", name)
	}

	if arg.isScalar() {
		return operand{}, e.fail("%s takes time series", name)
	}

	if !arg.array {
		// A single series folds to a scalar.
		values := slices.Collect(maps.Values(arg.list[0].points))
		if len(values) == 0 {
			return operand{scalar: math.NaN()}, nil
		}

		return operand{scalar: fold(values)}, nil
	}

	byTime := map[int64][]float64{}
	for _, sr := range arg.list {
		for t, v := range sr.points {
			byTime[t] = append(byTime[t], v)
		}
	}

	out := &series{points: map[int64]float64{}}
	for t, values := range byTime {
		out.points[t] = fold(values)
	}

	return operand{list: []*series{out}}, nil
}

func total(v []float64) (sum float64) {
	for _, x := range v {
		sum += x
	}

	return sum
}

// binary applies op to a and b; 'a' is ABS of a. A scalar applies to every
// point; two series combine at the times both have.
func (e *expression) binary(a, b operand, op byte) (operand, error) {
	apply := func(x, y float64) float64 {
		switch op {
		case '+':
			return x + y
		case '-':
			return x - y
		case '*':
			return x * y
		case '/':
			return x / y
		}

		return math.Abs(x)
	}

	switch {
	case a.isScalar() && b.isScalar():
		return operand{scalar: apply(a.scalar, b.scalar)}, nil
	case !a.isScalar() && !b.isScalar() && (len(a.list) != 1 || len(b.list) != 1):
		return operand{}, e.fail("arithmetic on two arrays of time series")
	}

	each := func(list []*series, f func(t int64, v float64) (float64, bool)) operand {
		out := operand{list: make([]*series, len(list)), array: len(list) != 1}
		for i, sr := range list {
			out.list[i] = &series{label: sr.label, points: map[int64]float64{}}
			for t, v := range sr.points {
				if r, ok := f(t, v); ok && !math.IsNaN(r) && !math.IsInf(r, 0) {
					out.list[i].points[t] = r
				}
			}
		}

		return out
	}

	switch {
	case b.isScalar():
		return each(a.list, func(_ int64, v float64) (float64, bool) { return apply(v, b.scalar), true }), nil
	case a.isScalar():
		return each(b.list, func(_ int64, v float64) (float64, bool) { return apply(a.scalar, v), true }), nil
	}

	other := b.list[0].points
	return each(a.list, func(t int64, v float64) (float64, bool) {
		w, ok := other[t]
		return apply(v, w), ok
	}), nil
}
