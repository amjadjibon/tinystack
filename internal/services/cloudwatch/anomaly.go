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
	kindDetector       = "anomaly-detector"
	trainingWindow     = 14 * 24 * time.Hour
	detectorPage       = 100
	detectorTypeSingle = "SINGLE_METRIC"
	bandFunction       = "ANOMALY_DETECTION_BAND"
	opOutsideBand      = "LessThanLowerOrGreaterThanUpperThreshold"
	opAboveBand        = "GreaterThanUpperThreshold"
	opBelowBand        = "LessThanLowerThreshold"
)

var (
	detectorIDPattern = regexp.MustCompile(`^[a-f0-9]{32}-[a-zA-Z0-9.]+$`)
	bandStart         = regexp.MustCompile(`^\s*ANOMALY_DETECTION_BAND\s*\(`)
	bandArguments     = regexp.MustCompile(
		`^\s*([a-z][a-zA-Z0-9_]*)\s*(?:,\s*(\d+(?:\.\d*)?|\.\d+)\s*)?\)`,
	)
)

func anomalyOperator(op string) bool {
	return op == opOutsideBand || op == opAboveBand || op == opBelowBand
}

func detectorID(namespace, name string, dims []api.Dimension, stat string) string {
	return metricID(namespace, name, dims) + "-" + stat
}

func detectorParams(namespace, name *string, dims []api.Dimension, stat *string,
	single *api.SingleMetricAnomalyDetector,
) (*api.SingleMetricAnomalyDetector, error) {
	if single != nil {
		if namespace != nil || name != nil || dims != nil || stat != nil {
			return nil, awserror.New(
				combinationCode,
				"Use either SingleMetricAnomalyDetector or the top-level metric fields",
			)
		}

		namespace, name, dims, stat = single.Namespace, single.MetricName, single.Dimensions, single.Stat
	}

	if common.Value(namespace) == "" || common.Value(name) == "" || common.Value(stat) == "" {
		return nil, missing("Namespace, MetricName, and Stat")
	}

	if len(*namespace) > 255 || len(*name) > 255 || (*namespace)[0] == ':' {
		return nil, invalidValue("Invalid anomaly detector metric name or namespace")
	}

	if err := checkDimensions(dims); err != nil {
		return nil, err
	}

	if _, err := statistic(*stat); err != nil {
		return nil, err
	}

	return &api.SingleMetricAnomalyDetector{
		Namespace: namespace, MetricName: name, Dimensions: canonical(dims),
		Stat: stat,
	}, nil
}

func (s *Service) PutAnomalyDetector(ctx context.Context, in *api.PutAnomalyDetectorInput,
) (*api.PutAnomalyDetectorOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	p, err := detectorParams(in.Namespace, in.MetricName, in.Dimensions, in.Stat, in.SingleMetricAnomalyDetector)
	if err != nil {
		return nil, err
	}

	if cfg := in.Configuration; cfg != nil {
		for _, r := range cfg.ExcludedTimeRanges {
			if r.StartTime == nil || r.EndTime == nil || !(*r.StartTime < *r.EndTime) {
				return nil, invalidValue("An excluded time range requires StartTime earlier than EndTime")
			}
		}
	}

	id := detectorID(*p.Namespace, *p.MetricName, p.Dimensions, *p.Stat)
	d := api.AnomalyDetector{
		AnomalyDetectorId: new(id), Namespace: p.Namespace, MetricName: p.MetricName, Dimensions: p.Dimensions,
		Stat: p.Stat, Configuration: in.Configuration, SingleMetricAnomalyDetector: p,
	}
	return &api.PutAnomalyDetectorOutput{
		AnomalyDetectorId: new(id),
	}, s.store.Update(
		ctx,
		func(tx storage.WriteTx) error {
			return storage.Write(tx, m.Key(service, kindDetector, id), d)
		},
	)
}

func (s *Service) DescribeAnomalyDetectors(ctx context.Context, in *api.DescribeAnomalyDetectorsInput,
) (*api.DescribeAnomalyDetectorsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	maxResults := common.Value(in.MaxResults)
	if in.MaxResults != nil && (maxResults < 1 || maxResults > detectorPage) {
		return nil, invalidValue("MaxResults must be from 1 to %d", detectorPage)
	}

	if checkErr := checkDimensions(in.Dimensions); checkErr != nil {
		return nil, checkErr
	}

	for _, kind := range in.AnomalyDetectorTypes {
		if kind != detectorTypeSingle {
			return nil, invalidValue("TinyStack supports only SINGLE_METRIC anomaly detectors")
		}
	}

	for _, id := range in.AnomalyDetectorIds {
		if !detectorIDPattern.MatchString(id) {
			return nil, invalidValue("Invalid AnomalyDetectorId %q", id)
		}
	}

	if maxResults == 0 {
		maxResults = detectorPage
	}

	out := &api.DescribeAnomalyDetectorsOutput{AnomalyDetectors: []api.AnomalyDetector{}}
	now := s.now().Truncate(time.Minute)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[api.AnomalyDetector](tx, m.Prefix(service, kindDetector))
		if readErr != nil {
			return readErr
		}

		selected := slices.DeleteFunc(records, func(d *api.AnomalyDetector) bool {
			return (in.Namespace != nil && *in.Namespace != *d.Namespace) ||
				(in.MetricName != nil && *in.MetricName != *d.MetricName) ||
				(len(in.AnomalyDetectorIds) > 0 && !slices.Contains(in.AnomalyDetectorIds, *d.AnomalyDetectorId)) ||
				!detectorDimensions(d.Dimensions, in.Dimensions)
		})
		page, next, pageErr := common.Page(
			selected,
			func(d *api.AnomalyDetector) string { return *d.AnomalyDetectorId },
			in.NextToken,
			int(maxResults),
			awserror.New("InvalidNextToken", "Invalid anomaly detector next token"),
		)
		if pageErr != nil {
			return pageErr
		}

		for _, d := range page {
			training, statErr := stat(tx, m.Scope, &api.MetricStat{
				Metric: &api.Metric{Namespace: d.Namespace, MetricName: d.MetricName, Dimensions: d.Dimensions},
				Period: new(int64(60)), Stat: d.Stat,
			}, now.Add(-trainingWindow), now)
			if statErr != nil {
				return statErr
			}

			state := "TRAINED_INSUFFICIENT_DATA"
			if len(trainingPoints(training, 60, d.Configuration)) >= 2 {
				state = "TRAINED"
			}

			d.StateValue = new(state)
			out.AnomalyDetectors = append(out.AnomalyDetectors, *d)
		}

		out.NextToken = next
		return nil
	})
	return out, err
}

func detectorDimensions(dims, filters []api.Dimension) bool {
	for _, filter := range filters {
		if !slices.ContainsFunc(dims, func(d api.Dimension) bool {
			return *d.Name == *filter.Name && *d.Value == *filter.Value
		}) {
			return false
		}
	}

	return true
}

func (s *Service) DeleteAnomalyDetector(ctx context.Context, in *api.DeleteAnomalyDetectorInput,
) (*api.DeleteAnomalyDetectorOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.AnomalyDetectorId)
	if in.AnomalyDetectorId == nil {
		p, paramErr := detectorParams(
			in.Namespace,
			in.MetricName,
			in.Dimensions,
			in.Stat,
			in.SingleMetricAnomalyDetector,
		)
		if paramErr != nil {
			return nil, paramErr
		}

		id = detectorID(*p.Namespace, *p.MetricName, p.Dimensions, *p.Stat)
	} else if in.Namespace != nil || in.MetricName != nil || in.Dimensions != nil || in.Stat != nil ||
		in.SingleMetricAnomalyDetector != nil {
		return nil, awserror.New(combinationCode, "Use either AnomalyDetectorId or the metric fields")
	}

	if !detectorIDPattern.MatchString(id) {
		return nil, invalidValue("Invalid AnomalyDetectorId %q", id)
	}

	return &api.DeleteAnomalyDetectorOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := m.Key(service, kindDetector, id)
		if _, found := tx.Get(key); !found {
			return awserror.New("ResourceNotFoundException", "The specified anomaly detector does not exist")
		}

		tx.Delete(key)
		return nil
	})
}

type bandSpec struct {
	metricID string
	width    float64
}

func parseBand(expr string) (bandSpec, error) {
	start := bandStart.FindString(expr)
	if start == "" {
		return bandSpec{}, invalidValue("ThresholdMetricId must identify an ANOMALY_DETECTION_BAND query")
	}

	spec, length, err := parseBandArguments(expr[len(start):])
	if err != nil {
		return bandSpec{}, err
	}

	if strings.TrimSpace(expr[len(start)+length:]) != "" {
		return bandSpec{}, invalidValue("ThresholdMetricId must identify an ANOMALY_DETECTION_BAND query")
	}

	return spec, nil
}

func parseBandArguments(args string) (bandSpec, int, error) {
	match := bandArguments.FindStringSubmatch(args)
	if match == nil {
		return bandSpec{}, 0, invalidValue("Expected ANOMALY_DETECTION_BAND(metric_id[, positive width])")
	}

	width := 2.0
	if match[2] != "" {
		w, err := strconv.ParseFloat(match[2], 64)
		if err != nil || w <= 0 {
			return bandSpec{}, 0, invalidValue("The anomaly detection band width must be a positive number")
		}

		width = w
	}

	return bandSpec{metricID: match[1], width: width}, len(match[0]), nil
}

func findQuery(queries []api.MetricDataQuery, id string) *api.MetricDataQuery {
	for i := range queries {
		if common.Value(queries[i].Id) == id {
			return &queries[i]
		}
	}

	return nil
}

func checkBandAlarm(queries []api.MetricDataQuery, id string) error {
	q := findQuery(queries, id)
	if q == nil || q.Expression == nil {
		return invalidValue("ThresholdMetricId must identify an ANOMALY_DETECTION_BAND query")
	}

	spec, err := parseBand(*q.Expression)
	if err != nil {
		return err
	}

	for _, other := range queries {
		if *other.Id != id && *other.Id != spec.metricID && (other.ReturnData == nil || *other.ReturnData) {
			return invalidValue("An anomaly alarm can only return the threshold band and its source metric")
		}
	}

	return nil
}

type trainingPoint struct {
	at    int64
	value float64
}

func trainingPoints(s *series, period int64, cfg *api.AnomalyDetectorConfiguration) []trainingPoint {
	var excluded []api.Range
	if cfg != nil {
		excluded = cfg.ExcludedTimeRanges
	}

	var out []trainingPoint
	for _, at := range slices.Sorted(maps.Keys(s.points)) {
		if slices.ContainsFunc(excluded, func(r api.Range) bool {
			return float64(at) < *r.EndTime && *r.StartTime < float64(at+period)
		}) {
			continue
		}

		out = append(out, trainingPoint{at, s.points[at]})
	}

	return out
}

// variance maintains mean and squared deviations over a sliding training window.
type variance struct {
	count int
	mean  float64
	m2    float64
}

func (v *variance) add(x float64) {
	v.count++
	delta := x - v.mean
	v.mean += delta / float64(v.count)
	v.m2 += delta * (x - v.mean)
}

func (v *variance) remove(x float64) {
	if v.count == 1 {
		*v = variance{}
		return
	}

	v.count--
	delta := x - v.mean
	v.mean -= delta / float64(v.count)
	v.m2 = max(0, v.m2-delta*(x-v.mean))
}

func buildBand(tx storage.ReadTx, scope request.Scope, st *api.MetricStat, observed *series,
	start, end time.Time, width float64,
) (operand, error) {
	lower := &series{label: "Lower band", points: map[int64]float64{}}
	upper := &series{label: "Upper band", points: map[int64]float64{}}
	out := operand{list: []*series{lower, upper}, array: true}
	if len(observed.points) == 0 {
		return out, nil
	}

	mt := st.Metric
	id := detectorID(*mt.Namespace, *mt.MetricName, mt.Dimensions, *st.Stat)
	d, _, err := storage.Read[api.AnomalyDetector](tx, scope.Key(service, kindDetector, id))
	if err != nil {
		return operand{}, err
	}

	var cfg *api.AnomalyDetectorConfiguration
	if d != nil {
		cfg = d.Configuration
	}

	history, err := stat(tx, scope, st, start.Add(-trainingWindow), end)
	if err != nil {
		return operand{}, err
	}

	training := trainingPoints(history, *st.Period, cfg)
	var head, tail int
	var window variance
	for _, at := range slices.Sorted(maps.Keys(observed.points)) {
		cutoff := at - int64(trainingWindow/time.Second)
		for tail < len(training) && training[tail].at < at {
			window.add(training[tail].value)
			tail++
		}

		for head < tail && training[head].at < cutoff {
			window.remove(training[head].value)
			head++
		}

		if window.count < 2 {
			continue
		}

		spread := width * math.Sqrt(max(0, window.m2)/float64(window.count))
		lo, hi := window.mean-spread, window.mean+spread
		if !math.IsInf(lo, 0) && !math.IsInf(hi, 0) && !math.IsNaN(lo) && !math.IsNaN(hi) {
			lower.points[at], upper.points[at] = lo, hi
		}
	}

	return out, nil
}
