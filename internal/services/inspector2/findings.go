package inspector2

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) ListFindings(_ context.Context, in *api.ListFindingsRequest) (*api.ListFindingsResponse, error) {
	if err := validateCriteria(in.FilterCriteria); err != nil {
		return nil, err
	}

	return &api.ListFindingsResponse{Findings: []api.Finding{}}, emptyPage(in.NextToken)
}

func (s *Service) ListCoverage(_ context.Context, in *api.ListCoverageRequest) (*api.ListCoverageResponse, error) {
	if err := validateCoverage(in.FilterCriteria); err != nil {
		return nil, err
	}

	return &api.ListCoverageResponse{CoveredResources: []api.CoveredResource{}}, emptyPage(in.NextToken)
}

func (s *Service) ListCoverageStatistics(_ context.Context, in *api.ListCoverageStatisticsRequest) (
	*api.ListCoverageStatisticsResponse, error,
) {
	if err := validateCoverage(in.FilterCriteria); err != nil {
		return nil, err
	}

	return &api.ListCoverageStatisticsResponse{CountsByGroup: []api.Counts{}, TotalCounts: new(int64(0))},
		emptyPage(in.NextToken)
}

func (s *Service) ListFindingAggregations(ctx context.Context, in *api.ListFindingAggregationsRequest) (
	*api.ListFindingAggregationsResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	for _, f := range in.AccountIds {
		if common.Value(f.Comparison) != "EQUALS" || common.Value(f.Value) != scope.AccountID {
			return nil, awserror.Validation("Only EQUALS with the caller's own account is supported.")
		}
	}

	switch common.Value(in.AggregationType) {
	case "ACCOUNT", "AMI", "AWS_ECR_CONTAINER", "EC2_INSTANCE", "FINDING_TYPE", "IMAGE_LAYER",
		"PACKAGE", "REPOSITORY", "TITLE", "LAMBDA_LAYER", "LAMBDA_FUNCTION":
	default:
		return nil, awserror.Validation("This aggregation type is not implemented.")
	}

	if r := in.AggregationRequest; r != nil {
		kinds := map[string]bool{
			"ACCOUNT": r.AccountAggregation != nil, "AMI": r.AmiAggregation != nil,
			"AWS_ECR_CONTAINER": r.AwsEcrContainerAggregation != nil, "EC2_INSTANCE": r.Ec2InstanceAggregation != nil,
			"FINDING_TYPE": r.FindingTypeAggregation != nil, "IMAGE_LAYER": r.ImageLayerAggregation != nil,
			"PACKAGE": r.PackageAggregation != nil, "REPOSITORY": r.RepositoryAggregation != nil,
			"TITLE": r.TitleAggregation != nil, "LAMBDA_LAYER": r.LambdaLayerAggregation != nil,
			"LAMBDA_FUNCTION": r.LambdaFunctionAggregation != nil,
		}

		count := 0
		for kind, present := range kinds {
			if !present {
				continue
			}

			count++
			if kind != common.Value(in.AggregationType) {
				return nil, awserror.Validation("aggregationRequest must match aggregationType.")
			}
		}

		if count != 1 {
			return nil, awserror.Validation("Specify exactly one aggregation request member.")
		}
	}

	return &api.ListFindingAggregationsResponse{
			AggregationType: in.AggregationType,
			Responses:       []api.AggregationResponse{},
		},
		emptyPage(
			in.NextToken,
		)
}

func (s *Service) BatchGetFindingDetails(_ context.Context, _ *api.BatchGetFindingDetailsRequest) (
	*api.BatchGetFindingDetailsResponse, error,
) {
	return &api.BatchGetFindingDetailsResponse{
		FindingDetails: []api.FindingDetail{},
		Errors:         []api.FindingDetailsError{},
	}, nil
}

func (s *Service) ListUsageTotals(
	ctx context.Context,
	in *api.ListUsageTotalsRequest,
) (*api.ListUsageTotalsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = ownAccount(scope, in.AccountIds)
	if err != nil {
		return nil, err
	}

	return &api.ListUsageTotalsResponse{
			Totals: []api.UsageTotal{{AccountId: new(scope.AccountID), Usage: []api.Usage{}}},
		},
		emptyPage(
			in.NextToken,
		)
}
