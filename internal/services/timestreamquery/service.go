// Package timestreamquery stores Amazon Timestream scheduled queries and account settings as metadata. Scheduled
// queries validate their schedule expression and cross-service references (SNS topic, IAM execution role, target
// Timestream database and table, S3 error-report bucket, and KMS key) and compute NextInvocationTime, but queries are
// never executed: Query, PrepareQuery, CancelQuery, and ExecuteScheduledQuery are not served.
package timestreamquery

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName          = "timestreamquery"
	kindScheduledQuery   = "scheduled-query"
	kindAccountSettings  = "account-settings"
	stateEnabled         = "ENABLED"
	stateDisabled        = "DISABLED"
	pricingBytesScanned  = "BYTES_SCANNED"
	computeModeOnDemand  = "ON_DEMAND"
	computeModeProvision = "PROVISIONED"
	statusSucceeded      = "SUCCEEDED"
	defaultMaxQueryTCU   = 1000
	minQueryTCU          = 4
	maxQueryTCU          = 1000
	queryTCUStep         = 4
	defaultMaxResults    = 1000
	defaultTagMaxResults = 200
	endpointCache        = 1440
)

var _ api.API = (*Service)(nil)

type Service struct {
	store      storage.Store
	timestream *timestreamwrite.Service
	topics     *sns.Service
	roles      *iam.Service
	buckets    *s3.Service
	keys       *kms.Service
}

func New(
	store storage.Store,
	timestream *timestreamwrite.Service,
	topics *sns.Service,
	roles *iam.Service,
	buckets *s3.Service,
	keys *kms.Service,
) *Service {
	return &Service{
		store:      store,
		timestream: timestream,
		topics:     topics,
		roles:      roles,
		buckets:    buckets,
		keys:       keys,
	}
}

func fault(code, format string, args ...any) error {
	return awserror.New(code, fmt.Sprintf(format, args...))
}

func invalid(format string, args ...any) error {
	return awserror.Validation(format, args...)
}

func key(scope request.Scope, kind, id string) string {
	return scope.Key(serviceName, kind, id)
}

func scheduledQueryARN(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:timestream:%s:%s:scheduled-query/%s", scope.Region, scope.AccountID, name)
}

func scheduledQueryNotFound(arn string) error {
	return fault("ResourceNotFoundException", "The scheduled query %s does not exist.", arn)
}

func (s *Service) checkTopic(ctx context.Context, topicARN string) error {
	if s.topics == nil {
		return invalid("SNS topic %s does not exist.", topicARN)
	}

	_, err := s.topics.GetTopicAttributes(ctx, &snsapi.GetTopicAttributesInput{TopicArn: new(topicARN)})
	if err != nil {
		return invalid("SNS topic %s does not exist.", topicARN)
	}

	return nil
}

func (s *Service) checkRole(ctx context.Context, scope request.Scope, roleARN string) error {
	prefix := "arn:aws:iam::" + scope.AccountID + ":role/"
	if s.roles == nil || !strings.HasPrefix(roleARN, prefix) || len(roleARN) == len(prefix) {
		return invalid("IAM role %s does not exist.", roleARN)
	}

	_, ok, err := s.roles.RoleByARN(ctx, roleARN)
	if err != nil {
		return err
	}

	if !ok {
		return invalid("IAM role %s does not exist.", roleARN)
	}

	return nil
}

func (s *Service) checkTable(ctx context.Context, scope request.Scope, database, table string) error {
	if s.timestream == nil {
		return invalid("Timestream table %s.%s does not exist.", database, table)
	}

	ok, err := s.timestream.HasTable(ctx, scope, database, table)
	if err != nil {
		return err
	}

	if !ok {
		return invalid("Timestream table %s.%s does not exist.", database, table)
	}

	return nil
}

func (s *Service) checkBucket(ctx context.Context, scope request.Scope, bucket string) error {
	if s.buckets == nil {
		return invalid("S3 bucket %s does not exist.", bucket)
	}

	_, err := s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{
		Bucket:              new(bucket),
		ExpectedBucketOwner: new(scope.AccountID),
	})
	if err != nil {
		return invalid("S3 bucket %s does not exist.", bucket)
	}

	return nil
}

func (s *Service) checkKMSKey(ctx context.Context, scope request.Scope, keyID string) error {
	if s.keys == nil {
		return invalid("KMS key %s does not exist.", keyID)
	}

	if err := s.keys.Check(ctx, scope, keyID); err != nil {
		return invalid("KMS key %s is not valid: %s", keyID, err.Error())
	}

	return nil
}

// DescribeEndpoints answers the endpoint discovery that the SDK performs before calling Timestream Query operations.
func (s *Service) DescribeEndpoints(
	ctx context.Context,
	_ *api.DescribeEndpointsRequest,
) (*api.DescribeEndpointsResponse, error) {
	meta, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	address := meta.Endpoint
	if address == "" {
		address = "query.timestream." + meta.Region + ".amazonaws.com"
	}

	return &api.DescribeEndpointsResponse{
		Endpoints: []api.Endpoint{{Address: new(address), CachePeriodInMinutes: new(int64(endpointCache))}},
	}, nil
}

type accountSettingsRecord struct {
	QueryCompute      *api.QueryComputeResponse
	QueryPricingModel string
	MaxQueryTCU       int64
}

func defaultAccountSettings() accountSettingsRecord {
	return accountSettingsRecord{
		MaxQueryTCU:       defaultMaxQueryTCU,
		QueryPricingModel: pricingBytesScanned,
		QueryCompute:      &api.QueryComputeResponse{ComputeMode: new(computeModeOnDemand)},
	}
}

func checkTCU(field string, v int64) error {
	if v < minQueryTCU || v > maxQueryTCU || v%queryTCUStep != 0 {
		return invalid("%s must be a multiple of %d between %d and %d.", field, queryTCUStep, minQueryTCU, maxQueryTCU)
	}

	return nil
}

func (s *Service) DescribeAccountSettings(
	ctx context.Context,
	_ *api.DescribeAccountSettingsRequest,
) (*api.DescribeAccountSettingsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	settings := defaultAccountSettings()
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, e := storage.Read[accountSettingsRecord](tx, key(scope, kindAccountSettings, "default"))
		if e != nil || !found {
			return e
		}

		settings = *r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeAccountSettingsResponse{
		MaxQueryTCU:       new(settings.MaxQueryTCU),
		QueryPricingModel: new(settings.QueryPricingModel),
		QueryCompute:      common.Clone(settings.QueryCompute),
	}, nil
}

func (s *Service) UpdateAccountSettings(
	ctx context.Context,
	in *api.UpdateAccountSettingsRequest,
) (*api.UpdateAccountSettingsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.MaxQueryTCU != nil {
		err = checkTCU("MaxQueryTCU", *in.MaxQueryTCU)
		if err != nil {
			return nil, err
		}
	}

	var compute *api.QueryComputeResponse
	if in.QueryCompute != nil {
		compute, err = s.validateQueryCompute(ctx, scope, in.QueryCompute)
		if err != nil {
			return nil, err
		}
	}

	settings := defaultAccountSettings()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, kindAccountSettings, "default")
		r, found, e := storage.Read[accountSettingsRecord](tx, k)
		if e != nil {
			return e
		}

		if found {
			settings = *r
		}

		if in.MaxQueryTCU != nil {
			settings.MaxQueryTCU = *in.MaxQueryTCU
		}

		if in.QueryPricingModel != nil {
			settings.QueryPricingModel = *in.QueryPricingModel
		}

		if compute != nil {
			settings.QueryCompute = compute
		}

		return storage.Write(tx, k, &settings)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAccountSettingsResponse{
		MaxQueryTCU:       new(settings.MaxQueryTCU),
		QueryPricingModel: new(settings.QueryPricingModel),
		QueryCompute:      common.Clone(settings.QueryCompute),
	}, nil
}

func (s *Service) validateQueryCompute(
	ctx context.Context,
	scope request.Scope,
	in *api.QueryComputeRequest,
) (*api.QueryComputeResponse, error) {
	mode := common.Value(in.ComputeMode)
	if mode == "" {
		mode = computeModeOnDemand
	}

	out := &api.QueryComputeResponse{ComputeMode: new(mode)}
	if mode == computeModeProvision && in.ProvisionedCapacity == nil {
		return nil, invalid("ProvisionedCapacity is required when ComputeMode is PROVISIONED.")
	}

	if in.ProvisionedCapacity == nil {
		return out, nil
	}

	tcu := *in.ProvisionedCapacity.TargetQueryTCU
	if mode == computeModeProvision || tcu != 0 {
		if err := checkTCU("TargetQueryTCU", tcu); err != nil {
			return nil, err
		}
	}

	notif := in.ProvisionedCapacity.NotificationConfiguration
	if notif != nil {
		if err := s.checkRole(ctx, scope, *notif.RoleArn); err != nil {
			return nil, err
		}

		if notif.SnsConfiguration != nil && notif.SnsConfiguration.TopicArn != nil {
			if err := s.checkTopic(ctx, *notif.SnsConfiguration.TopicArn); err != nil {
				return nil, err
			}
		}
	}

	out.ProvisionedCapacity = &api.ProvisionedCapacityResponse{
		ActiveQueryTCU:            new(tcu),
		NotificationConfiguration: common.Clone(notif),
		LastUpdate: &api.LastUpdate{
			TargetQueryTCU: new(tcu),
			Status:         new(statusSucceeded),
		},
	}

	return out, nil
}
