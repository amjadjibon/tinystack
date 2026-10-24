package mediaconvert

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) list(
	ctx context.Context,
	kind string,
	mode, order, category, token *string,
	limit *int64,
	filter func(*record) bool,
) ([]*record, *string, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, nil, scopeErr
	}

	mode = common.OrElse(mode, new("NAME"))
	systemOnly := *mode == "SYSTEM"
	if *mode == "SYSTEM" {
		mode = new("NAME")
	}

	var items []*record
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := ensureDefault(tx, scope); err != nil {
			return err
		}

		names, err := storage.Records[string](tx, scope.Prefix(serviceName, kind+"-"+*mode))
		if err != nil {
			return err
		}

		for _, name := range names {
			r, err := read(tx, scope, kind, *name)
			if err != nil {
				return err
			}

			if systemOnly {
				continue
			}

			if category != nil {
				value := ""
				if r.Preset != nil {
					value = common.Value(r.Preset.Category)
				}

				if r.Template != nil {
					value = common.Value(r.Template.Category)
				}

				if value != *category {
					continue
				}
			}

			if filter != nil && !filter(r) {
				continue
			}

			if r.Queue != nil {
				if err := queueCounts(tx, scope, r.Queue); err != nil {
					return err
				}
			}

			items = append(items, r)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	if common.Value(order) == "DESCENDING" {
		slices.Reverse(items)
	}

	return common.Page(
		items,
		func(r *record) string { return r.Name },
		token,
		int(common.Value(common.OrElse(limit, new(int64(20))))),
		invalid("Invalid nextToken"),
	)
}

// ListQueues enumerates queues in the selected storage index order.
func (s *Service) ListQueues(ctx context.Context, in *api.ListQueuesRequest) (*api.ListQueuesResponse, error) {
	rs, next, err := s.list(
		ctx,
		kindQueue,
		common.OrElse(in.ListBy, new("CREATION_DATE")),
		in.Order,
		nil,
		in.NextToken,
		in.MaxResults,
		nil,
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListQueuesResponse{NextToken: next, Queues: []api.Queue{}}
	for _, r := range rs {
		out.Queues = append(out.Queues, *r.Queue)
	}

	return out, nil
}

// ListPresets lists custom presets only.
func (s *Service) ListPresets(ctx context.Context, in *api.ListPresetsRequest) (*api.ListPresetsResponse, error) {
	rs, next, err := s.list(ctx, kindPreset, in.ListBy, in.Order, in.Category, in.NextToken, in.MaxResults, nil)
	if err != nil {
		return nil, err
	}

	out := &api.ListPresetsResponse{NextToken: next, Presets: []api.Preset{}}
	for _, r := range rs {
		out.Presets = append(out.Presets, *r.Preset)
	}

	return out, nil
}

// ListJobTemplates lists custom job templates only.
func (s *Service) ListJobTemplates(
	ctx context.Context,
	in *api.ListJobTemplatesRequest,
) (*api.ListJobTemplatesResponse, error) {
	rs, next, err := s.list(ctx, kindTemplate, in.ListBy, in.Order, in.Category, in.NextToken, in.MaxResults, nil)
	if err != nil {
		return nil, err
	}

	out := &api.ListJobTemplatesResponse{NextToken: next, JobTemplates: []api.JobTemplate{}}
	for _, r := range rs {
		out.JobTemplates = append(out.JobTemplates, *r.Template)
	}

	return out, nil
}

func (s *Service) jobs(ctx context.Context, in *api.SearchJobsRequest) ([]api.Job, *string, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, nil, scopeErr
	}

	queue := ""
	if in.Queue != nil {
		name, err := nameRef(scope, kindQueue, *in.Queue)
		if err != nil {
			return nil, nil, err
		}

		queue = arn(scope, kindQueue, name)
	}

	rs, next, err := s.list(
		ctx,
		kindJob,
		new("CREATION_DATE"),
		common.OrElse(in.Order, new("DESCENDING")),
		nil,
		in.NextToken,
		in.MaxResults,
		func(r *record) bool {
			if queue != "" && common.Value(r.Job.Queue) != queue {
				return false
			}

			if in.Status != nil && common.Value(r.Job.Status) != *in.Status {
				return false
			}

			if in.InputFile != nil {
				for _, input := range r.Job.Settings.Inputs {
					if strings.Contains(common.Value(input.FileInput), *in.InputFile) {
						return true
					}
				}

				return false
			}

			return true
		},
	)
	out := []api.Job{}
	for _, r := range rs {
		out = append(out, *r.Job)
	}

	return out, next, err
}

// ListJobs lists jobs by creation date with queue and status filters.
func (s *Service) ListJobs(ctx context.Context, in *api.ListJobsRequest) (*api.ListJobsResponse, error) {
	jobs, next, err := s.jobs(
		ctx,
		&api.SearchJobsRequest{
			Queue:      in.Queue,
			Status:     in.Status,
			Order:      in.Order,
			NextToken:  in.NextToken,
			MaxResults: in.MaxResults,
		},
	)
	return &api.ListJobsResponse{Jobs: jobs, NextToken: next}, err
}

// SearchJobs additionally filters on the input file URI or partial file name.
func (s *Service) SearchJobs(ctx context.Context, in *api.SearchJobsRequest) (*api.SearchJobsResponse, error) {
	jobs, next, err := s.jobs(ctx, in)
	return &api.SearchJobsResponse{Jobs: jobs, NextToken: next}, err
}

// DescribeEndpoints returns the endpoint the client used for this request.
func (s *Service) DescribeEndpoints(
	ctx context.Context,
	in *api.DescribeEndpointsRequest,
) (*api.DescribeEndpointsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	endpoints, next, err := common.Page(
		[]api.Endpoint{{Url: new(m.Endpoint)}},
		func(e api.Endpoint) string { return *e.Url },
		in.NextToken,
		int(common.Value(common.OrElse(in.MaxResults, new(int64(20))))),
		invalid("Invalid nextToken"),
	)
	return &api.DescribeEndpointsResponse{Endpoints: endpoints, NextToken: next}, err
}
