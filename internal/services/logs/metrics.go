package logs

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxMetricFilters is how many metric filters a group may have.
const maxMetricFilters = 100

// metricFilter is a metric filter, kept in its group's record. TinyStack's
// filter patterns extract no fields, so each matching event adds Value.
type metricFilter struct {
	Default   *float64 `json:",omitempty"`
	Name      string
	Pattern   string
	Metric    string
	Namespace string
	Value     string
	Unit      string `json:",omitempty"`
	Created   int64
}

func (f *metricFilter) describe(group string) api.MetricFilter {
	return api.MetricFilter{
		FilterName: new(f.Name), FilterPattern: new(f.Pattern), LogGroupName: new(group), CreationTime: new(f.Created),
		MetricTransformations: []api.MetricTransformation{{
			MetricName: new(f.Metric), MetricNamespace: new(f.Namespace), MetricValue: new(f.Value),
			DefaultValue: f.Default, Unit: optional(f.Unit),
		}},
	}
}

// checkTransformation validates a metric filter's one transformation: its
// value must be a number, since TinyStack's patterns extract no fields.
func checkTransformation(transformations []api.MetricTransformation) (*api.MetricTransformation, error) {
	if len(transformations) != 1 {
		return nil, invalid("metricTransformations must hold exactly one transformation")
	}

	t := &transformations[0]
	switch {
	case value(t.MetricName) == "" || len(*t.MetricName) > 255:
		return nil, invalid("metricName must be 1 to 255 characters")
	case value(t.MetricNamespace) == "" || len(*t.MetricNamespace) > 255:
		return nil, invalid("metricNamespace must be 1 to 255 characters")
	case len(t.Dimensions) > 0:
		return nil, invalid("TinyStack does not support metric filter dimensions, which need JSON or " +
			"space-delimited filter patterns")
	}

	if _, err := strconv.ParseFloat(value(t.MetricValue), 64); err != nil {
		return nil, invalid("TinyStack supports only numeric metricValue, since its filter patterns extract no fields")
	}

	return t, nil
}

func (s *Service) PutMetricFilter(
	ctx context.Context,
	in *api.PutMetricFilterRequest,
) (*api.PutMetricFilterOutput, error) {
	name := value(in.FilterName)
	if !filterName.MatchString(name) {
		return nil, invalid("filterName must be 1 to 512 characters without ':' or '*'")
	}

	if _, err := compilePattern(value(in.FilterPattern)); err != nil {
		return nil, err
	}

	t, err := checkTransformation(in.MetricTransformations)
	if err != nil {
		return nil, err
	}

	f := metricFilter{
		Name: name, Pattern: value(in.FilterPattern), Metric: *t.MetricName, Namespace: *t.MetricNamespace,
		Value: *t.MetricValue, Default: t.DefaultValue, Unit: value(t.Unit), Created: s.now().UnixMilli(),
	}
	err = s.change(ctx, in.LogGroupName, func(g *group) error {
		if i := slices.IndexFunc(g.MetricFilters, func(o metricFilter) bool { return o.Name == name }); i >= 0 {
			g.MetricFilters[i] = f
			return nil
		}

		if len(g.MetricFilters) == maxMetricFilters {
			return awserror.New("LimitExceededException", "Resource limit exceeded.")
		}

		g.MetricFilters = append(g.MetricFilters, f)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutMetricFilterOutput{}, nil
}

// DescribeMetricFilters lists the metric filters of one group, or of every
// group, by group and then filter name.
func (s *Service) DescribeMetricFilters(
	ctx context.Context,
	in *api.DescribeMetricFiltersRequest,
) (*api.DescribeMetricFiltersResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(maxDescribe)
	switch l := value(in.Limit); {
	case in.Limit != nil && (l < 1 || l > maxDescribe):
		return nil, invalid("limit must be from 1 to %d", maxDescribe)
	case in.Limit != nil:
		limit = l
	case in.FilterNamePrefix != nil && in.LogGroupName == nil:
		return nil, invalid("filterNamePrefix needs logGroupName")
	case (in.MetricName == nil) != (in.MetricNamespace == nil):
		return nil, invalid("metricName and metricNamespace must be given together")
	}

	after, err := decodeToken(in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeMetricFiltersResponse{MetricFilters: []api.MetricFilter{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		keys := tx.Keys(groupKey(scope, ""))
		if in.LogGroupName != nil {
			if _, loadErr := loadGroup(tx, scope, *in.LogGroupName); loadErr != nil {
				return loadErr
			}

			keys = []string{groupKey(scope, *in.LogGroupName)}
		}

		for _, k := range keys {
			g, _, readErr := read[group](tx, k)
			if readErr != nil {
				return readErr
			}

			for i := range g.MetricFilters {
				f := &g.MetricFilters[i]
				position := g.Name + "\x00" + f.Name
				if position <= after || !strings.HasPrefix(f.Name, value(in.FilterNamePrefix)) ||
					(in.MetricName != nil && (f.Metric != *in.MetricName || f.Namespace != *in.MetricNamespace)) {
					continue
				}

				if int64(len(out.MetricFilters)) == limit {
					out.NextToken = encodeToken(after)
					return nil
				}

				out.MetricFilters = append(out.MetricFilters, f.describe(g.Name))
				after = position
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteMetricFilter(
	ctx context.Context,
	in *api.DeleteMetricFilterRequest,
) (*api.DeleteMetricFilterOutput, error) {
	err := s.change(ctx, in.LogGroupName, func(g *group) error {
		i := slices.IndexFunc(g.MetricFilters, func(o metricFilter) bool { return o.Name == value(in.FilterName) })
		if i < 0 {
			return notFound("The specified metric filter does not exist.")
		}

		g.MetricFilters = slices.Delete(g.MetricFilters, i, i+1)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteMetricFilterOutput{}, nil
}

// TestMetricFilter reports which messages a filter pattern matches.
func (s *Service) TestMetricFilter(
	_ context.Context,
	in *api.TestMetricFilterRequest,
) (*api.TestMetricFilterResponse, error) {
	if len(in.LogEventMessages) == 0 || len(in.LogEventMessages) > 50 {
		return nil, invalid("logEventMessages must hold 1 to 50 messages")
	}

	match, err := compilePattern(value(in.FilterPattern))
	if err != nil {
		return nil, err
	}

	out := &api.TestMetricFilterResponse{Matches: []api.MetricFilterMatchRecord{}}
	for i, m := range in.LogEventMessages {
		if match(m) {
			out.Matches = append(out.Matches, api.MetricFilterMatchRecord{
				EventNumber: new(int64(i + 1)), EventMessage: new(m), ExtractedValues: map[string]string{},
			})
		}
	}

	return out, nil
}

// measure sends CloudWatch the data points a write's events add to its
// group's metric filters: one per matching event, at the event's time, or the
// filter's default value when none matches. A failure is logged and dropped.
func (s *Service) measure(ctx context.Context, d *delivered) {
	if s.caller == nil || d == nil || len(d.events) == 0 {
		return
	}

	ctx = request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{Scope: d.scope})
	for i := range d.group.MetricFilters {
		f := &d.group.MetricFilters[i]
		match, err := compilePattern(f.Pattern)
		if err != nil {
			continue
		}

		v, _ := strconv.ParseFloat(f.Value, 64)
		var data []any
		for _, e := range d.events {
			if match(e.Message) {
				data = append(data, datum(f, v, float64(e.Timestamp)/1000))
			}
		}

		if len(data) == 0 && f.Default != nil {
			data = append(data, datum(f, *f.Default, float64(d.events[len(d.events)-1].Timestamp)/1000))
		}

		// PutMetricData takes at most 1000 data points.
		for len(data) > 0 {
			batch := data[:min(len(data), 1000)]
			data = data[len(batch):]
			if _, err = s.caller.Call(ctx, "cloudwatch", "PutMetricData",
				map[string]any{"Namespace": f.Namespace, "MetricData": batch}); err != nil {
				slog.Warn("emitting a log metric filter's data failed", "group", d.group.Name, "filter", f.Name,
					"error", err)
				break
			}
		}
	}
}

func datum(f *metricFilter, v, seconds float64) map[string]any {
	d := map[string]any{"MetricName": f.Metric, "Value": v, "Timestamp": seconds}
	if f.Unit != "" {
		d["Unit"] = f.Unit
	}

	return d
}
