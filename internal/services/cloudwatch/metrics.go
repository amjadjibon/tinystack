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
	maxData       = 1000
	maxValues     = 150
	listPage      = 500
	unitNone      = "None"
	statCount     = "SampleCount"
	statAverage   = "Average"
	statSum       = "Sum"
	statMinimum   = "Minimum"
	statMaximum   = "Maximum"
	defaultPoints = 100800
)

var units = map[string]bool{
	"Seconds": true, "Microseconds": true, "Milliseconds": true, "Bytes": true, "Kilobytes": true,
	"Megabytes": true, "Gigabytes": true, "Terabytes": true, "Bits": true, "Kilobits": true, "Megabits": true,
	"Gigabits": true, "Terabits": true, "Percent": true, "Count": true, "Bytes/Second": true,
	"Kilobytes/Second": true, "Megabytes/Second": true, "Gigabytes/Second": true, "Terabytes/Second": true,
	"Bits/Second": true, "Kilobits/Second": true, "Megabits/Second": true, "Gigabits/Second": true,
	"Terabits/Second": true, "Count/Second": true, unitNone: true,
}

// point is a stored data point: one value, a set of values with counts, or
// statistics. Values and Counts are kept when a percentile can use them.
type point struct {
	Unit    string
	Values  []float64 `json:",omitempty"`
	Counts  []float64 `json:",omitempty"`
	T       float64
	Count   float64
	Sum     float64
	Min     float64
	Max     float64
	Summary bool `json:",omitempty"`
}

func dataPrefix(scope request.Scope, id string) string {
	return scope.Prefix(service, kindData) + id + "/"
}

func metricKey(scope request.Scope, id string) string { return scope.Prefix(service, kindMetric) + id }

func (s *Service) PutMetricData(ctx context.Context, in *api.PutMetricDataInput) (*api.PutMetricDataOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	namespace := common.Value(in.Namespace)
	switch {
	case namespace == "":
		return nil, missing("Namespace")
	case len(namespace) > 255:
		return nil, invalidValue("The namespace must be at most 255 characters")
	case strings.HasPrefix(namespace, "AWS/"):
		return nil, invalidValue("The value AWS/ for parameter Namespace is invalid.")
	case len(in.MetricData) == 0:
		return nil, missing("MetricData")
	case len(in.MetricData) > maxData:
		return nil, invalidValue("The collection MetricData must not have a size greater than %d.", maxData)
	}

	now := s.now()
	points := make([]point, len(in.MetricData))
	for i := range in.MetricData {
		if points[i], err = datum(&in.MetricData[i], now); err != nil {
			return nil, err
		}
	}

	return &api.PutMetricDataOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		for i := range in.MetricData {
			d := &in.MetricData[i]
			dims := canonical(d.Dimensions)
			id := metricID(namespace, common.Value(d.MetricName), dims)
			rec := metric{
				Namespace:  namespace,
				Name:       common.Value(d.MetricName),
				Dimensions: dims,
				Last:       common.Unix(now),
			}
			if err := storage.Write(tx, metricKey(m.Scope, id), rec); err != nil {
				return err
			}

			if err := storage.Write(
				tx,
				dataPrefix(m.Scope, id)+s.sequenced(fromUnix(points[i].T)),
				points[i],
			); err != nil {
				return err
			}

			prune(tx, dataPrefix(m.Scope, id), now.Add(-retention))
		}

		return nil
	})
}

// datum checks one MetricDatum and returns its data point.
func datum(d *api.MetricDatum, now time.Time) (point, error) {
	name := common.Value(d.MetricName)
	if name == "" || len(name) > 255 {
		return point{}, missing("MetricData.member.N.MetricName")
	}

	if err := checkDimensions(d.Dimensions); err != nil {
		return point{}, err
	}

	unit := common.Value(d.Unit)
	if unit == "" {
		unit = unitNone
	}

	if !units[unit] {
		return point{}, invalidValue("The value %s for parameter MetricData.member.N.Unit is invalid.", unit)
	}

	if r := common.Value(d.StorageResolution); d.StorageResolution != nil && r != 1 && r != 60 {
		return point{}, invalidValue("StorageResolution must be 1 or 60")
	}

	t := now
	if d.Timestamp != nil {
		t = fromUnix(*d.Timestamp)
	}

	if t.Before(now.Add(-14*24*time.Hour)) || t.After(now.Add(2*time.Hour)) {
		return point{}, invalidValue("The parameter MetricData.member.N.Timestamp must be within the last two weeks " +
			"and at most two hours in the future.")
	}

	p := point{T: common.Unix(t), Unit: unit}
	given := 0
	if d.Value != nil {
		given++
		p.Values, p.Counts = []float64{*d.Value}, []float64{1}
	}

	if d.Values != nil {
		given++
		if len(d.Values) > maxValues || (d.Counts != nil && len(d.Counts) != len(d.Values)) {
			return point{}, invalidValue("Values must have at most %d members and Counts as many as Values", maxValues)
		}

		p.Values, p.Counts = d.Values, d.Counts
		if p.Counts == nil {
			p.Counts = slices.Repeat([]float64{1}, len(d.Values))
		}
	} else if d.Counts != nil {
		return point{}, awserror.New("InvalidParameterCombinationException", "Counts requires Values")
	}

	if st := d.StatisticValues; st != nil {
		given++
		if st.Maximum == nil || st.Minimum == nil || st.SampleCount == nil || st.Sum == nil {
			return point{}, missing("StatisticValues.Maximum, Minimum, SampleCount, and Sum")
		}

		if *st.SampleCount <= 0 || *st.Minimum > *st.Maximum {
			return point{}, invalidValue("StatisticValues must have a positive SampleCount and Minimum <= Maximum")
		}

		p.Count, p.Sum, p.Min, p.Max, p.Summary = *st.SampleCount, *st.Sum, *st.Minimum, *st.Maximum, true
		return p, only(given)
	}

	if err := only(given); err != nil {
		return point{}, err
	}

	p.Min, p.Max = math.Inf(1), math.Inf(-1)
	for i, v := range p.Values {
		if math.IsNaN(v) || math.IsInf(v, 0) || p.Counts[i] < 0 {
			return point{}, invalidValue("Metric values must be finite numbers and counts not negative")
		}

		p.Count += p.Counts[i]
		p.Sum += v * p.Counts[i]
		p.Min, p.Max = min(p.Min, v), max(p.Max, v)
	}

	if p.Count == 0 {
		return point{}, invalidValue("A metric datum must have at least one value")
	}

	return p, nil
}

func only(given int) error {
	if given != 1 {
		return awserror.New("InvalidParameterCombinationException",
			"A metric datum must have exactly one of Value, Values, and StatisticValues")
	}

	return nil
}

// prune deletes the data points under p stored before cutoff.
func prune(tx storage.WriteTx, p string, cutoff time.Time) {
	for _, k := range tx.Keys(p) {
		if pointTime(k, p) >= cutoff.UnixMilli() {
			return
		}

		tx.Delete(k)
	}
}

// pointTime reads the milliseconds in a data point's or history item's key.
func pointTime(key, p string) int64 {
	suffix := strings.TrimPrefix(key, p)
	if len(suffix) < 15 {
		return 0
	}

	ms, _ := strconv.ParseInt(suffix[:15], 10, 64)
	return ms
}

// bucket aggregates the data points of one period.
type bucket struct {
	unit    string
	values  []float64
	counts  []float64
	count   float64
	sum     float64
	min     float64
	max     float64
	summary bool
}

func (b *bucket) add(p *point) {
	if b.count == 0 {
		b.min, b.max, b.unit = p.Min, p.Max, p.Unit
	}

	b.count += p.Count
	b.sum += p.Sum
	b.min, b.max = min(b.min, p.Min), max(b.max, p.Max)
	b.values = append(b.values, p.Values...)
	b.counts = append(b.counts, p.Counts...)
	b.summary = b.summary || p.Summary
}

// statistic returns the function computing stat from a bucket, which reports
// false when the bucket cannot answer it (a percentile of statistic sets).
func statistic(stat string) (func(*bucket) (float64, bool), error) {
	switch stat {
	case statCount:
		return func(b *bucket) (float64, bool) { return b.count, true }, nil
	case statAverage:
		return func(b *bucket) (float64, bool) { return b.sum / b.count, true }, nil
	case statSum:
		return func(b *bucket) (float64, bool) { return b.sum, true }, nil
	case statMinimum:
		return func(b *bucket) (float64, bool) { return b.min, true }, nil
	case statMaximum:
		return func(b *bucket) (float64, bool) { return b.max, true }, nil
	}

	p, ok := percentile(stat)
	if !ok {
		return nil, invalidValue("TinyStack does not support the statistic %s", stat)
	}

	return func(b *bucket) (float64, bool) {
		if b.summary {
			return 0, false
		}

		return weighted(b.values, b.counts, p), true
	}, nil
}

var percentileStat = regexp.MustCompile(`^p(\d{1,2}(\.\d{1,2})?|100)$`)

func percentile(stat string) (float64, bool) {
	if !percentileStat.MatchString(stat) {
		return 0, false
	}

	p, err := strconv.ParseFloat(stat[1:], 64)
	return p, err == nil
}

// weighted is the nearest-rank percentile p of values repeated by counts.
func weighted(values, counts []float64, p float64) float64 {
	idx := make([]int, len(values))
	var total float64
	for i := range idx {
		idx[i] = i
		total += counts[i]
	}

	slices.SortFunc(idx, func(a, b int) int { return compare(values[a], values[b]) })
	rank, seen := math.Max(1, math.Ceil(p/100*total)), 0.0
	for _, i := range idx {
		if seen += counts[i]; seen >= rank {
			return values[i]
		}
	}

	return values[idx[len(idx)-1]]
}

func compare(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

// buckets aggregates a metric's data points in [start, end) by period
// (seconds, aligned to the epoch), keeping only unit's when unit is set.
func buckets(tx storage.ReadTx, scope request.Scope, id string, period int64, unit string,
	start, end time.Time,
) (map[int64]*bucket, error) {
	out := map[int64]*bucket{}
	p := dataPrefix(scope, id)
	for _, k := range tx.Keys(p) {
		ms := pointTime(k, p)
		if ms < start.UnixMilli() || ms >= end.UnixMilli() {
			continue
		}

		pt, _, err := storage.Read[point](tx, k)
		if err != nil {
			return nil, err
		}

		if unit != "" && pt.Unit != unit {
			continue
		}

		at := ms / 1000 / period * period
		b := out[at]
		if b == nil {
			b = &bucket{}
			out[at] = b
		}

		b.add(pt)
	}

	return out, nil
}

func checkPeriod(period int64, alarm bool) error {
	switch {
	case period <= 0:
		return invalidValue("The parameter Period must be greater than 0.")
	case alarm && period != 10 && period != 20 && period != 30 && period%60 != 0:
		return invalidValue("The parameter Period must be 10, 20, 30, or a multiple of 60.")
	case !alarm && period != 1 && period != 5 && period != 10 && period != 30 && period%60 != 0:
		return invalidValue("The parameter Period must be 1, 5, 10, 30, or a multiple of 60.")
	}

	return nil
}

func (s *Service) GetMetricStatistics(
	ctx context.Context, in *api.GetMetricStatisticsInput,
) (*api.GetMetricStatisticsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	stats := slices.Concat(in.Statistics, in.ExtendedStatistics)
	switch {
	case common.Value(in.Namespace) == "":
		return nil, missing("Namespace")
	case common.Value(in.MetricName) == "":
		return nil, missing("MetricName")
	case in.StartTime == nil:
		return nil, missing("StartTime")
	case in.EndTime == nil:
		return nil, missing("EndTime")
	case in.Period == nil:
		return nil, missing("Period")
	case len(stats) == 0:
		return nil, missing("Statistics or ExtendedStatistics")
	case len(stats) > 5:
		return nil, invalidValue("At most 5 statistics can be requested")
	case !(*in.StartTime < *in.EndTime):
		return nil, invalidValue("The parameter StartTime must be less than the parameter EndTime.")
	case in.Unit != nil && !units[*in.Unit]:
		return nil, invalidValue("The value %s for parameter Unit is invalid.", *in.Unit)
	}

	for _, st := range in.Statistics {
		if _, ok := percentile(st); ok {
			return nil, invalidValue("Percentiles go in ExtendedStatistics, not Statistics")
		}
	}

	for _, st := range in.ExtendedStatistics {
		if _, ok := percentile(st); !ok {
			return nil, invalidValue("TinyStack does not support the extended statistic %s", st)
		}
	}

	if checkErr := checkPeriod(*in.Period, false); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkDimensions(in.Dimensions); checkErr != nil {
		return nil, checkErr
	}

	funcs := make([]func(*bucket) (float64, bool), len(stats))
	for i, st := range stats {
		if funcs[i], err = statistic(st); err != nil {
			return nil, err
		}
	}

	id := metricID(*in.Namespace, *in.MetricName, in.Dimensions)
	var got map[int64]*bucket
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		got, err = buckets(
			tx,
			m.Scope,
			id,
			*in.Period,
			common.Value(in.Unit),
			fromUnix(*in.StartTime),
			fromUnix(*in.EndTime),
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := &api.GetMetricStatisticsOutput{Label: in.MetricName, Datapoints: []api.Datapoint{}}
	for _, at := range slices.Sorted(maps.Keys(got)) {
		b := got[at]
		dp := api.Datapoint{Timestamp: new(float64(at)), Unit: new(b.unit)}
		for i, st := range stats {
			v, ok := funcs[i](b)
			if !ok {
				continue
			}

			switch st {
			case statCount:
				dp.SampleCount = new(v)
			case statAverage:
				dp.Average = new(v)
			case statSum:
				dp.Sum = new(v)
			case statMinimum:
				dp.Minimum = new(v)
			case statMaximum:
				dp.Maximum = new(v)
			default:
				if dp.ExtendedStatistics == nil {
					dp.ExtendedStatistics = map[string]float64{}
				}

				dp.ExtendedStatistics[st] = v
			}
		}

		out.Datapoints = append(out.Datapoints, dp)
	}

	return out, nil
}

func (s *Service) ListMetrics(ctx context.Context, in *api.ListMetricsInput) (*api.ListMetricsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var since time.Time
	switch common.Value(in.RecentlyActive) {
	case "":
		since = s.now().Add(-retention)
	case "PT3H":
		since = s.now().Add(-3 * time.Hour)
	default:
		return nil, invalidValue("The value %s for parameter RecentlyActive is invalid.", *in.RecentlyActive)
	}

	if len(in.Dimensions) > 30 {
		return nil, invalidValue("At most 30 dimension filters are allowed")
	}

	out := &api.ListMetricsOutput{Metrics: []api.Metric{}}
	var last string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p := m.Prefix(service, kindMetric)
		for _, k := range tx.Keys(p) {
			id := strings.TrimPrefix(k, p)
			if in.NextToken != nil && id <= *in.NextToken {
				continue
			}

			rec, _, readErr := storage.Read[metric](tx, k)
			if readErr != nil {
				return readErr
			}

			if rec.Last < common.Unix(since) || !listed(rec, in) {
				continue
			}

			if len(out.Metrics) == listPage {
				out.NextToken = new(last)
				return nil
			}

			out.Metrics = append(out.Metrics, api.Metric{
				Namespace: new(rec.Namespace), MetricName: new(rec.Name), Dimensions: rec.Dimensions,
			})
			last = id
		}

		return nil
	})

	return out, err
}

// listed reports whether a metric passes ListMetrics' filters.
func listed(rec *metric, in *api.ListMetricsInput) bool {
	if in.Namespace != nil && rec.Namespace != *in.Namespace || in.MetricName != nil && rec.Name != *in.MetricName {
		return false
	}

	for _, f := range in.Dimensions {
		found := false
		for _, d := range rec.Dimensions {
			if common.Value(d.Name) == common.Value(f.Name) && (f.Value == nil || common.Value(d.Value) == *f.Value) {
				found = true
			}
		}

		if !found {
			return false
		}
	}

	return true
}
