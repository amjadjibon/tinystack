package cloudwatch

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindStream  = "metric-stream"
	streamIndex = "aws/cloudwatch/metric-stream-index/"
)

var (
	streamName         = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,255}$`)
	streamFirehoseName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
)

type metricStream struct {
	Config     api.GetMetricStreamOutput
	Tags       map[string]string
	Generation string
	LastError  string
	Delivered  int64
	Failed     int64
}

func streamKey(scope request.Scope, name string) string { return scope.Key(service, kindStream, name) }

func streamARN(scope request.Scope, name string) string {
	return "arn:aws:cloudwatch:" + scope.Region + ":" + scope.AccountID + ":metric-stream/" + name
}

func streamIndexKey(scope request.Scope, name string) string {
	return streamIndex + scope.AccountID + "/" + scope.Region + "/" + name
}

func streamPending(scope request.Scope, name string) string {
	return scope.Prefix(service, "stream-pending") + name + "/"
}

func streamSent(scope request.Scope, name string) string {
	return scope.Prefix(service, "stream-sent") + name + "/"
}

func streamText(v string, namespace bool) bool {
	if v == "" || len(v) > 255 || strings.TrimSpace(v) == "" || (namespace && v[0] == ':') {
		return false
	}

	for _, c := range v {
		if c < 32 || c > 126 {
			return false
		}
	}

	return true
}

// validateStream follows https://docs.aws.amazon.com/AmazonCloudWatch/latest/APIReference/API_PutMetricStream.html.
func validateStream(in *api.PutMetricStreamInput, scope request.Scope) error {
	for _, field := range []struct {
		value *string
		name  string
	}{
		{in.Name, "Name"}, {in.FirehoseArn, "FirehoseArn"}, {in.RoleArn, "RoleArn"}, {in.OutputFormat, "OutputFormat"},
	} {
		if common.Value(field.value) == "" {
			return missing(field.name)
		}
	}

	switch {
	case !streamName.MatchString(*in.Name):
		return invalidValue("Invalid metric stream name")
	case in.IncludeFilters != nil && in.ExcludeFilters != nil:
		return awserror.New(combinationCode, "IncludeFilters and ExcludeFilters are mutually exclusive")
	case common.Value(in.IncludeLinkedAccountsMetrics):
		return invalidValue("Linked-account metrics are not supported")
	case *in.OutputFormat != "json":
		return invalidValue("Only json metric stream output is supported; OpenTelemetry formats are not implemented")
	case len(*in.FirehoseArn) > 1024 || !strings.HasPrefix(*in.FirehoseArn,
		"arn:aws:firehose:"+scope.Region+":"+scope.AccountID+":deliverystream/") ||
		!streamFirehoseName.MatchString(strings.TrimPrefix(*in.FirehoseArn,
			"arn:aws:firehose:"+scope.Region+":"+scope.AccountID+":deliverystream/")):
		return invalidValue("FirehoseArn must name a delivery stream in the same account and Region")
	case len(*in.RoleArn) > 1024 || !strings.HasPrefix(*in.RoleArn,
		"arn:aws:iam::"+scope.AccountID+":role/") || strings.HasSuffix(*in.RoleArn, "/"):
		return invalidValue("RoleArn must name an IAM role in the same account")
	case len(in.StatisticsConfigurations) > 100:
		return invalidValue("At most 100 statistics configurations are allowed")
	}

	names := 0
	for _, f := range slices.Concat(in.IncludeFilters, in.ExcludeFilters) {
		if !streamText(common.Value(f.Namespace), true) {
			return invalidValue("Invalid filter namespace")
		}

		names += 1 + len(f.MetricNames)
		for _, name := range f.MetricNames {
			if !streamText(name, false) {
				return invalidValue("Invalid filter metric name")
			}
		}
	}

	if names > 1000 {
		return invalidValue("Filters may contain at most 1000 namespace and metric names")
	}

	for _, config := range in.StatisticsConfigurations {
		if len(config.IncludeMetrics) < 1 || len(config.IncludeMetrics) > 100 ||
			len(config.AdditionalStatistics) < 1 || len(config.AdditionalStatistics) > 20 {
			return invalidValue("Statistics configurations require 1-100 metrics and 1-20 statistics")
		}

		for _, metric := range config.IncludeMetrics {
			if !streamText(common.Value(metric.Namespace), true) ||
				!streamText(common.Value(metric.MetricName), false) {
				return invalidValue("Invalid statistics metric")
			}
		}

		for _, stat := range config.AdditionalStatistics {
			if _, err := statistic(stat); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Service) PutMetricStream(
	ctx context.Context,
	in *api.PutMetricStreamInput,
) (*api.PutMetricStreamOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := validateStream(in, m.Scope); checkErr != nil {
		return nil, checkErr
	}

	arn := streamARN(m.Scope, *in.Name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		old, found, readErr := storage.Read[metricStream](tx, streamKey(m.Scope, *in.Name))
		if readErr != nil {
			return readErr
		}

		now := common.Unix(s.now())
		if !found {
			tags, tagErr := tagging.Merge(nil, tagsOf(in.Tags))
			if tagErr != nil {
				return invalidValue("%s", tagErr)
			}

			old = &metricStream{
				Tags: tags, Generation: common.RandomID(16),
				Config: api.GetMetricStreamOutput{CreationDate: new(now), State: new("running")},
			}
		}

		old.Config = api.GetMetricStreamOutput{
			Arn: new(arn), Name: in.Name, IncludeFilters: in.IncludeFilters, ExcludeFilters: in.ExcludeFilters,
			FirehoseArn: in.FirehoseArn, RoleArn: in.RoleArn, OutputFormat: in.OutputFormat,
			StatisticsConfigurations: in.StatisticsConfigurations, IncludeLinkedAccountsMetrics: new(false),
			State: old.Config.State, CreationDate: old.Config.CreationDate, LastUpdateDate: new(now),
		}

		tx.Put(streamIndexKey(m.Scope, *in.Name), nil)
		return storage.Write(tx, streamKey(m.Scope, *in.Name), old)
	})
	return &api.PutMetricStreamOutput{Arn: new(arn)}, err
}

func (s *Service) GetMetricStream(
	ctx context.Context,
	in *api.GetMetricStreamInput,
) (*api.GetMetricStreamOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.Name) == "" {
		return nil, missing("Name")
	}
	if !streamName.MatchString(*in.Name) {
		return nil, invalidValue("Invalid metric stream name")
	}

	var out *api.GetMetricStreamOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		st, found, readErr := storage.Read[metricStream](tx, streamKey(m.Scope, *in.Name))
		if readErr != nil || !found {
			return notFound(*in.Name, readErr)
		}

		out = &st.Config
		return nil
	})
	return out, err
}

func (s *Service) ListMetricStreams(
	ctx context.Context,
	in *api.ListMetricStreamsInput,
) (*api.ListMetricStreamsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(500)
	if in.MaxResults != nil {
		limit = *in.MaxResults
	}

	if limit < 1 || limit > 500 {
		return nil, invalidValue("MaxResults must be between 1 and 500")
	}

	var entries []api.MetricStreamEntry
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		streams, readErr := storage.Records[metricStream](tx, m.Prefix(service, kindStream))
		if readErr != nil {
			return readErr
		}

		for _, st := range streams {
			c := st.Config
			entries = append(entries, api.MetricStreamEntry{
				Arn:            c.Arn,
				Name:           c.Name,
				CreationDate:   c.CreationDate,
				LastUpdateDate: c.LastUpdateDate,
				FirehoseArn:    c.FirehoseArn,
				State:          c.State,
				OutputFormat:   c.OutputFormat,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := common.Page(entries, func(e api.MetricStreamEntry) string { return *e.Name },
		in.NextToken, int(limit), awserror.New("InvalidNextToken", "Invalid next token"))
	return &api.ListMetricStreamsOutput{Entries: selected, NextToken: next}, err
}

func (s *Service) DeleteMetricStream(
	ctx context.Context,
	in *api.DeleteMetricStreamInput,
) (*api.DeleteMetricStreamOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.Name) == "" {
		return nil, missing("Name")
	}
	if !streamName.MatchString(*in.Name) {
		return nil, invalidValue("Invalid metric stream name")
	}

	return &api.DeleteMetricStreamOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(streamKey(m.Scope, *in.Name))
		tx.Delete(streamIndexKey(m.Scope, *in.Name))
		for _, prefix := range []string{streamPending(m.Scope, *in.Name), streamSent(m.Scope, *in.Name)} {
			for _, key := range tx.Keys(prefix) {
				tx.Delete(key)
			}
		}

		return nil
	})
}

func (s *Service) streamState(ctx context.Context, names []string, state string) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	if len(names) == 0 {
		return missing("Names")
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, name := range names {
			if !streamName.MatchString(name) {
				return invalidValue("Invalid metric stream name")
			}
			st, found, err := storage.Read[metricStream](tx, streamKey(m.Scope, name))
			if err != nil {
				return err
			}

			if !found {
				return invalidValue("Metric stream %s does not exist", name)
			}

			st.Config.State, st.Config.LastUpdateDate = new(state), new(common.Unix(s.now()))
			if err := storage.Write(tx, streamKey(m.Scope, name), st); err != nil {
				return err
			}

			if state == "stopped" {
				for _, key := range tx.Keys(streamPending(m.Scope, name)) {
					tx.Delete(key)
				}
			}
		}

		return nil
	})
}

func (s *Service) StartMetricStreams(
	ctx context.Context,
	in *api.StartMetricStreamsInput,
) (*api.StartMetricStreamsOutput, error) {
	return &api.StartMetricStreamsOutput{}, s.streamState(ctx, in.Names, "running")
}

func (s *Service) StopMetricStreams(
	ctx context.Context,
	in *api.StopMetricStreamsInput,
) (*api.StopMetricStreamsOutput, error) {
	return &api.StopMetricStreamsOutput{}, s.streamState(ctx, in.Names, "stopped")
}

func streamMatches(c *api.GetMetricStreamOutput, namespace, name string) bool {
	matches := func(filters []api.MetricStreamFilter) bool {
		for _, f := range filters {
			if common.Value(f.Namespace) == namespace &&
				(len(f.MetricNames) == 0 || slices.Contains(f.MetricNames, name)) {
				return true
			}
		}

		return false
	}

	return (len(c.IncludeFilters) == 0 || matches(c.IncludeFilters)) && !matches(c.ExcludeFilters)
}
