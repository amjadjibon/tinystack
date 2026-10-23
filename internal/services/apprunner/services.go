package apprunner

import (
	"context"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/common"
)

func checkSource(source *api.SourceConfiguration) error {
	if source == nil || (source.ImageRepository == nil) == (source.CodeRepository == nil) {
		return invalid("SourceConfiguration must contain exactly one of ImageRepository or CodeRepository")
	}

	return nil
}

// instanceSizes are the CPU and memory values App Runner documents. The model's patterns anchor only their first
// and last alternatives, so they alone would accept values such as "1024junk".
var instanceSizes = map[string][]string{
	"Cpu": {"256", "512", "1024", "2048", "4096", "0.25 vCPU", "0.5 vCPU", "1 vCPU", "2 vCPU", "4 vCPU"},
	"Memory": {
		"512", "1024", "2048", "3072", "4096", "6144", "8192", "10240", "12288",
		"0.5 GB", "1 GB", "2 GB", "3 GB", "4 GB", "6 GB", "8 GB", "10 GB", "12 GB",
	},
}

func checkInstance(c *api.InstanceConfiguration) error {
	if c == nil {
		return nil
	}

	for field, value := range map[string]*string{"Cpu": c.Cpu, "Memory": c.Memory} {
		if value != nil && !slices.Contains(instanceSizes[field], *value) {
			return invalid("InstanceConfiguration.%s %q is not a supported value", field, *value)
		}
	}

	return nil
}

func (s *Service) CreateService(ctx context.Context, in *api.CreateServiceRequest) (*api.CreateServiceResponse, error) {
	if err := checkSource(in.SourceConfiguration); err != nil {
		return nil, err
	}

	if err := checkInstance(in.InstanceConfiguration); err != nil {
		return nil, err
	}

	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &record{Kind: kindService, Name: common.Value(in.ServiceName), Tags: tags, Service: &api.Service{
		AutoScalingConfigurationSummary: &api.AutoScalingConfigurationSummary{
			AutoScalingConfigurationArn: in.AutoScalingConfigurationArn,
		},
		ServiceName:                in.ServiceName,
		Status:                     new(statusRunning),
		CreatedAt:                  common.SecondsMilli(time.Now()),
		UpdatedAt:                  common.SecondsMilli(time.Now()),
		SourceConfiguration:        in.SourceConfiguration,
		InstanceConfiguration:      in.InstanceConfiguration,
		EncryptionConfiguration:    in.EncryptionConfiguration,
		HealthCheckConfiguration:   in.HealthCheckConfiguration,
		NetworkConfiguration:       in.NetworkConfiguration,
		ObservabilityConfiguration: in.ObservabilityConfiguration,
	}}
	if err := s.insert(ctx, r); err != nil {
		return nil, err
	}

	out := common.Clone(r.Service)
	out.Status = new(statusInProgress)
	return &api.CreateServiceResponse{Service: out, OperationId: r.Operations[0].Id}, nil
}

func (s *Service) DescribeService(
	ctx context.Context,
	in *api.DescribeServiceRequest,
) (*api.DescribeServiceResponse, error) {
	r, err := s.get(ctx, in.ServiceArn, kindService)
	if err != nil {
		return nil, err
	}

	return &api.DescribeServiceResponse{Service: r.Service}, nil
}

func (s *Service) UpdateService(ctx context.Context, in *api.UpdateServiceRequest) (*api.UpdateServiceResponse, error) {
	if in.SourceConfiguration != nil {
		if err := checkSource(in.SourceConfiguration); err != nil {
			return nil, err
		}
	}

	if err := checkInstance(in.InstanceConfiguration); err != nil {
		return nil, err
	}

	out, id, err := s.changeService(ctx, in.ServiceArn, "UPDATE_SERVICE", statusRunning, func(r *api.Service) {
		if in.SourceConfiguration != nil {
			r.SourceConfiguration = in.SourceConfiguration
		}

		if in.InstanceConfiguration != nil {
			if r.InstanceConfiguration == nil {
				r.InstanceConfiguration = &api.InstanceConfiguration{}
			}

			if in.InstanceConfiguration.Cpu != nil {
				r.InstanceConfiguration.Cpu = in.InstanceConfiguration.Cpu
			}

			if in.InstanceConfiguration.Memory != nil {
				r.InstanceConfiguration.Memory = in.InstanceConfiguration.Memory
			}

			if in.InstanceConfiguration.InstanceRoleArn != nil {
				r.InstanceConfiguration.InstanceRoleArn = in.InstanceConfiguration.InstanceRoleArn
			}
		}

		if in.HealthCheckConfiguration != nil {
			r.HealthCheckConfiguration = in.HealthCheckConfiguration
		}

		if in.NetworkConfiguration != nil {
			r.NetworkConfiguration = in.NetworkConfiguration
		}

		if in.ObservabilityConfiguration != nil {
			r.ObservabilityConfiguration = in.ObservabilityConfiguration
		}
	}, in.AutoScalingConfigurationArn)
	return &api.UpdateServiceResponse{Service: out, OperationId: id}, err
}

func (s *Service) DeleteService(ctx context.Context, in *api.DeleteServiceRequest) (*api.DeleteServiceResponse, error) {
	out, id, err := s.changeService(ctx, in.ServiceArn, "DELETE_SERVICE", statusDeleted, nil, nil)
	return &api.DeleteServiceResponse{Service: out, OperationId: id}, err
}

func (s *Service) PauseService(ctx context.Context, in *api.PauseServiceRequest) (*api.PauseServiceResponse, error) {
	out, id, err := s.changeService(ctx, in.ServiceArn, "PAUSE_SERVICE", statusPaused, nil, nil)
	return &api.PauseServiceResponse{Service: out, OperationId: id}, err
}

func (s *Service) ResumeService(ctx context.Context, in *api.ResumeServiceRequest) (*api.ResumeServiceResponse, error) {
	out, id, err := s.changeService(ctx, in.ServiceArn, "RESUME_SERVICE", statusRunning, nil, nil)
	return &api.ResumeServiceResponse{Service: out, OperationId: id}, err
}

func (s *Service) StartDeployment(
	ctx context.Context,
	in *api.StartDeploymentRequest,
) (*api.StartDeploymentResponse, error) {
	_, id, err := s.changeService(ctx, in.ServiceArn, "START_DEPLOYMENT", statusRunning, nil, nil)
	return &api.StartDeploymentResponse{OperationId: id}, err
}

func (s *Service) ListServices(ctx context.Context, in *api.ListServicesRequest) (*api.ListServicesResponse, error) {
	items, next, err := s.list(ctx, kindService, nil, nil, in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.ListServicesResponse{NextToken: next, ServiceSummaryList: []api.ServiceSummary{}}
	for _, r := range items {
		v := r.Service
		out.ServiceSummaryList = append(
			out.ServiceSummaryList,
			api.ServiceSummary{
				ServiceName: v.ServiceName,
				ServiceId:   v.ServiceId,
				ServiceArn:  v.ServiceArn,
				ServiceUrl:  v.ServiceUrl,
				Status:      v.Status,
				CreatedAt:   v.CreatedAt,
				UpdatedAt:   v.UpdatedAt,
			},
		)
	}

	return out, nil
}

func (s *Service) ListOperations(
	ctx context.Context,
	in *api.ListOperationsRequest,
) (*api.ListOperationsResponse, error) {
	r, err := s.get(ctx, in.ServiceArn, kindService)
	if err != nil {
		return nil, err
	}

	limit := len(r.Operations) + 1
	if in.MaxResults != nil {
		limit = int(*in.MaxResults)
	}

	operations := make([]api.OperationSummary, len(r.Operations))
	for i, item := range r.Operations {
		operations[len(operations)-1-i] = item
	}

	items, next, err := common.Page(
		operations,
		func(o api.OperationSummary) string { return common.Value(o.Id) },
		in.NextToken,
		limit,
		invalid("Invalid NextToken"),
	)
	return &api.ListOperationsResponse{OperationSummaryList: items, NextToken: next}, err
}
