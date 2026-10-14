// Package servicequotas serves informational quotas and stores increase requests.
// Values are never enforced; requests remain PENDING and never change applied quotas.
package servicequotas

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicequotas"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "servicequotas"

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type quota struct {
	service, serviceTitle, code, title string
	value                              float64
	adjustable, global                 bool
}

// This deliberately small snapshot is independent of live AWS account limits.
// Sources: AWS service quota references and aws/tools-for-devops-agent's common quota codes.
// Keep entries ordered by service code, then quota code, for common.Page.
var quotas = []quota{
	{"dynamodb", "Amazon DynamoDB", "L-F98FE922", "Tables per account per Region", 2500, true, false},
	{"ec2", "Amazon Elastic Compute Cloud (Amazon EC2)", "L-0263D0A3", "EC2-VPC Elastic IPs", 5, true, false},
	{
		"ec2", "Amazon Elastic Compute Cloud (Amazon EC2)", "L-1216C47A",
		"Running On-Demand Standard (A, C, D, H, I, M, R, T, Z) instances", 5, true, false,
	},
	{"iam", "AWS Identity and Access Management (IAM)", "L-FE177D64", "Roles per account", 1000, true, true},
	{"lambda", "AWS Lambda", "L-B99A9384", "Concurrent executions", 1000, true, false},
	{"s3", "Amazon Simple Storage Service (Amazon S3)", "L-DC2B2D3D", "General purpose buckets", 10000, true, true},
	{"sqs", "Amazon Simple Queue Service (Amazon SQS)", "L-9F628B95", "Statements per Queue Policy", 20, false, false},
	{"vpc", "Amazon Virtual Private Cloud (Amazon VPC)", "L-F678F1CE", "VPCs per Region", 5, true, false},
}

func lookup(service, code *string) (quota, error) {
	for _, q := range quotas {
		if q.service == common.Value(service) && q.code == common.Value(code) {
			return q, nil
		}
	}

	return quota{}, awserror.New("NoSuchResourceException", "Unknown service or quota code.")
}

func checkService(code *string) error {
	for _, q := range quotas {
		if q.service == common.Value(code) {
			return nil
		}
	}

	return awserror.New("NoSuchResourceException", "Unknown service code.")
}

func (q *quota) output(scope request.Scope, defaults bool) api.ServiceQuota {
	account := scope.AccountID
	if defaults {
		account = ""
	}

	return api.ServiceQuota{
		ServiceCode: new(q.service), ServiceName: new(q.serviceTitle), QuotaCode: new(q.code), QuotaName: new(q.title),
		QuotaArn: new(fmt.Sprintf("arn:aws:servicequotas:%s:%s:%s/%s", scope.Region, account, q.service, q.code)),
		Value:    new(q.value), Unit: new("None"), Adjustable: new(q.adjustable), GlobalQuota: new(q.global),
	}
}

func pageSize(value *int64) (int, error) {
	if value == nil {
		return 100, nil
	}

	if *value < 1 || *value > 100 {
		return 0, awserror.New("IllegalArgumentException", "MaxResults must be between 1 and 100.")
	}

	return int(*value), nil
}

func getQuota(ctx context.Context, service, code *string, defaults bool) (*api.ServiceQuota, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	q, err := lookup(service, code)
	if err != nil {
		return nil, err
	}

	return new(q.output(scope, defaults)), nil
}

func (s *Service) GetServiceQuota(
	ctx context.Context, in *api.GetServiceQuotaRequest,
) (*api.GetServiceQuotaResponse, error) {
	q, err := getQuota(ctx, in.ServiceCode, in.QuotaCode, false)

	return &api.GetServiceQuotaResponse{Quota: q}, err
}

func (s *Service) GetAWSDefaultServiceQuota(
	ctx context.Context, in *api.GetAWSDefaultServiceQuotaRequest,
) (*api.GetAWSDefaultServiceQuotaResponse, error) {
	q, err := getQuota(ctx, in.ServiceCode, in.QuotaCode, true)

	return &api.GetAWSDefaultServiceQuotaResponse{Quota: q}, err
}

func listQuotas(
	ctx context.Context, service, code, token *string, maxResults *int64, defaults bool,
) ([]api.ServiceQuota, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	if err := checkService(service); err != nil {
		return nil, nil, err
	}

	if code != nil {
		if _, err = lookup(service, code); err != nil {
			return nil, nil, err
		}
	}

	limit, err := pageSize(maxResults)
	if err != nil {
		return nil, nil, err
	}

	items := []api.ServiceQuota{}
	for _, q := range quotas {
		if q.service == *service && (code == nil || q.code == *code) {
			items = append(items, q.output(scope, defaults))
		}
	}

	return common.Page(items, func(q api.ServiceQuota) string { return *q.QuotaCode }, token, limit,
		awserror.New("InvalidPaginationTokenException", "Invalid NextToken."))
}

func (s *Service) ListServiceQuotas(
	ctx context.Context, in *api.ListServiceQuotasRequest,
) (*api.ListServiceQuotasResponse, error) {
	items, token, err := listQuotas(ctx, in.ServiceCode, in.QuotaCode, in.NextToken, in.MaxResults, false)

	return &api.ListServiceQuotasResponse{Quotas: items, NextToken: token}, err
}

func (s *Service) ListAWSDefaultServiceQuotas(
	ctx context.Context, in *api.ListAWSDefaultServiceQuotasRequest,
) (*api.ListAWSDefaultServiceQuotasResponse, error) {
	items, token, err := listQuotas(ctx, in.ServiceCode, nil, in.NextToken, in.MaxResults, true)

	return &api.ListAWSDefaultServiceQuotasResponse{Quotas: items, NextToken: token}, err
}

func (s *Service) ListServices(
	ctx context.Context, in *api.ListServicesRequest,
) (*api.ListServicesResponse, error) {
	if _, err := request.RegionalScope(ctx); err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	items := []api.ServiceInfo{}
	for _, q := range quotas {
		if len(items) == 0 || *items[len(items)-1].ServiceCode != q.service {
			items = append(items, api.ServiceInfo{ServiceCode: new(q.service), ServiceName: new(q.serviceTitle)})
		}
	}

	items, token, err := common.Page(
		items,
		func(i api.ServiceInfo) string { return *i.ServiceCode },
		in.NextToken,
		limit,
		awserror.New("InvalidPaginationTokenException", "Invalid NextToken."),
	)
	return &api.ListServicesResponse{Services: items, NextToken: token}, err
}

func (s *Service) RequestServiceQuotaIncrease(
	ctx context.Context, in *api.RequestServiceQuotaIncreaseRequest,
) (*api.RequestServiceQuotaIncreaseResponse, error) {
	meta, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	q, err := lookup(in.ServiceCode, in.QuotaCode)
	if err != nil {
		return nil, err
	}

	value := common.Value(in.DesiredValue)
	if !q.adjustable || math.IsNaN(value) || math.IsInf(value, 0) || value <= q.value {
		return nil, awserror.New("IllegalArgumentException",
			"DesiredValue must be finite and greater than the adjustable quota value.")
	}

	caller := fmt.Sprintf("arn:aws:iam::%s:root", meta.AccountID)
	if meta.Principal != nil {
		caller = meta.Principal.ARN
	}

	now := common.SecondsMilli(time.Now())
	r := api.RequestedServiceQuotaChange{
		Id: new(common.RandomID(32)), ServiceCode: new(q.service), ServiceName: new(q.serviceTitle),
		QuotaCode: new(q.code), QuotaName: new(q.title), DesiredValue: new(value), Status: new("PENDING"),
		Created: now, LastUpdated: now, Requester: new(caller), QuotaArn: q.output(meta.Scope, false).QuotaArn,
		GlobalQuota: new(q.global), Unit: new("None"),
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		records, e := storage.Records[api.RequestedServiceQuotaChange](tx, meta.Prefix(serviceName, "request"))
		if e != nil {
			return e
		}

		for _, existing := range records {
			if *existing.ServiceCode == q.service && *existing.QuotaCode == q.code {
				return awserror.New("ResourceAlreadyExistsException",
					"Only one open service quota increase request is allowed per quota.")
			}
		}

		return storage.Write(tx, meta.Key(serviceName, "request", *r.Id), r)
	})

	return &api.RequestServiceQuotaIncreaseResponse{RequestedQuota: &r}, err
}

func (s *Service) GetRequestedServiceQuotaChange(
	ctx context.Context, in *api.GetRequestedServiceQuotaChangeRequest,
) (*api.GetRequestedServiceQuotaChangeResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *api.RequestedServiceQuotaChange
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var e error
		r, found, e = storage.Read[api.RequestedServiceQuotaChange](
			tx, scope.Key(serviceName, "request", common.Value(in.RequestId)))
		if e != nil {
			return e
		}

		if !found {
			return awserror.New("NoSuchResourceException", "Unknown quota increase request.")
		}

		return nil
	})

	return &api.GetRequestedServiceQuotaChangeResponse{RequestedQuota: r}, err
}

func (s *Service) history(
	ctx context.Context, service, code, status, token *string, maxResults *int64,
) ([]api.RequestedServiceQuotaChange, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	if service != nil {
		if err := checkService(service); err != nil {
			return nil, nil, err
		}
	}

	if code != nil {
		if _, err = lookup(service, code); err != nil {
			return nil, nil, err
		}
	}

	statuses := []string{
		"PENDING",
		"CASE_OPENED",
		"APPROVED",
		"DENIED",
		"CASE_CLOSED",
		"NOT_APPROVED",
		"INVALID_REQUEST",
	}
	if status != nil && !slices.Contains(statuses, *status) {
		return nil, nil, awserror.New("IllegalArgumentException", "Invalid Status.")
	}

	limit, err := pageSize(maxResults)
	if err != nil {
		return nil, nil, err
	}

	items := []api.RequestedServiceQuotaChange{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.RequestedServiceQuotaChange](tx, scope.Prefix(serviceName, "request"))
		if e != nil {
			return e
		}

		for _, r := range records {
			if (service == nil || *r.ServiceCode == *service) && (code == nil || *r.QuotaCode == *code) &&
				(status == nil || *r.Status == *status) {
				items = append(items, *r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(items, func(r api.RequestedServiceQuotaChange) string { return *r.Id }, token, limit,
		awserror.New("InvalidPaginationTokenException", "Invalid NextToken."))
}

func (s *Service) ListRequestedServiceQuotaChangeHistory(
	ctx context.Context, in *api.ListRequestedServiceQuotaChangeHistoryRequest,
) (*api.ListRequestedServiceQuotaChangeHistoryResponse, error) {
	items, token, err := s.history(ctx, in.ServiceCode, nil, in.Status, in.NextToken, in.MaxResults)

	return &api.ListRequestedServiceQuotaChangeHistoryResponse{RequestedQuotas: items, NextToken: token}, err
}

func (s *Service) ListRequestedServiceQuotaChangeHistoryByQuota(
	ctx context.Context, in *api.ListRequestedServiceQuotaChangeHistoryByQuotaRequest,
) (*api.ListRequestedServiceQuotaChangeHistoryByQuotaResponse, error) {
	if _, err := lookup(in.ServiceCode, in.QuotaCode); err != nil {
		return nil, err
	}

	items, token, err := s.history(ctx, in.ServiceCode, in.QuotaCode, in.Status, in.NextToken, in.MaxResults)

	return &api.ListRequestedServiceQuotaChangeHistoryByQuotaResponse{RequestedQuotas: items, NextToken: token}, err
}
