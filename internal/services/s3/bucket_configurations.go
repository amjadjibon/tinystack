package s3

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxBucketConfigurations = 1000
	bucketConfigurationPage = 100
	configPrefix            = "/config/"
	configAnalytics         = "analytics"
	configMetrics           = "metrics"
	configInventory         = "inventory"
	configIntelligent       = "intelligent-tiering"
)

var (
	bucketConfigurationID = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
	metricsAccessPointARN = regexp.MustCompile(`^arn:aws:s3:[a-z0-9-]+:\d{12}:accesspoint/[a-z0-9-]+$`)
)

func configurationTags(tags []api.Tag) error {
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			return malformedXML()
		}
	}

	_, err := tagMap(tags)
	return err
}

func validateConfigurationFilter(prefix *string, tag *api.Tag, accessPoint *string, and bool) error {
	if count(prefix != nil, tag != nil, accessPoint != nil, and) != 1 ||
		len(common.Value(prefix)) > maxKeyBytes ||
		(accessPoint != nil && !metricsAccessPointARN.MatchString(*accessPoint)) {
		return malformedXML()
	}

	if tag != nil {
		return configurationTags([]api.Tag{*tag})
	}

	return nil
}

func validateConfigurationAnd(prefix *string, tags []api.Tag, accessPoint *string) error {
	if len(tags)+count(prefix != nil, accessPoint != nil) < 2 ||
		len(common.Value(prefix)) > maxKeyBytes ||
		(accessPoint != nil && !metricsAccessPointARN.MatchString(*accessPoint)) {
		return malformedXML()
	}

	return configurationTags(tags)
}

func getBucketConfiguration[T any](
	ctx context.Context,
	s *Service,
	bucket, owner, id *string,
	kind string,
) (*T, error) {
	if !bucketConfigurationID.MatchString(common.Value(id)) {
		return nil, invalidArgument("Invalid configuration ID")
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var record *T
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		bucketKey, resolveErr := resolve(tx, m, bucket, owner)
		if resolveErr != nil {
			return resolveErr
		}

		v, found, readErr := storage.Read[T](tx, bucketKey+configPrefix+kind+"/"+*id)
		if readErr != nil {
			return readErr
		}

		if !found {
			return fail(http.StatusNotFound, "NoSuchConfiguration", "The specified configuration does not exist")
		}

		record = v
		return nil
	})
	return record, err
}

func (s *Service) changeBucketConfiguration(
	ctx context.Context,
	bucket, owner, id *string,
	kind string,
	record any,
) error {
	if !bucketConfigurationID.MatchString(common.Value(id)) {
		return invalidArgument("Invalid configuration ID")
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		bucketKey, resolveErr := resolve(tx, m, bucket, owner)
		if resolveErr != nil {
			return resolveErr
		}

		prefix := bucketKey + configPrefix + kind + "/"
		key := prefix + *id
		if record == nil {
			tx.Delete(key)
			return nil
		}

		if _, exists := tx.Get(key); !exists && len(tx.Keys(prefix)) >= maxBucketConfigurations {
			return fail(
				http.StatusBadRequest,
				"TooManyConfigurations",
				"The bucket has reached the configuration limit",
			)
		}

		return storage.Write(tx, key, record)
	})
}

func listBucketConfigurations[T any](
	ctx context.Context, s *Service, bucket, owner, token *string, kind string,
) (records []T, next *string, err error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		bucketKey, resolveErr := resolve(tx, m, bucket, owner)
		if resolveErr != nil {
			return resolveErr
		}

		prefix := bucketKey + configPrefix + kind + "/"
		keys, continuation, pageErr := common.Page(tx.Keys(prefix), func(key string) string {
			return strings.TrimPrefix(key, prefix)
		}, token, bucketConfigurationPage, invalidArgument("Invalid continuation token"))
		if pageErr != nil {
			return pageErr
		}

		next = continuation
		for _, key := range keys {
			v, _, readErr := storage.Read[T](tx, key)
			if readErr != nil {
				return readErr
			}

			records = append(records, *v)
		}

		return nil
	})

	return records, next, err
}

func validateAnalytics(c *api.AnalyticsConfiguration) error {
	if c.StorageClassAnalysis == nil {
		return malformedXML()
	}

	if f := c.Filter; f != nil {
		if err := validateConfigurationFilter(f.Prefix, f.Tag, nil, f.And != nil); err != nil {
			return err
		}

		if f.And != nil {
			if err := validateConfigurationAnd(f.And.Prefix, f.And.Tags, nil); err != nil {
				return err
			}
		}
	}

	if d := c.StorageClassAnalysis.DataExport; d != nil {
		if common.Value(d.OutputSchemaVersion) != "V_1" || d.Destination == nil ||
			d.Destination.S3BucketDestination == nil {
			return malformedXML()
		}

		b := d.Destination.S3BucketDestination
		_, ok := bucketARNName(common.Value(b.Bucket))
		if !ok || common.Value(b.Format) != "CSV" ||
			(b.BucketAccountId != nil && !accountNumber.MatchString(*b.BucketAccountId)) {
			return malformedXML()
		}
	}

	return nil
}

func validateMetrics(c *api.MetricsConfiguration) error {
	f := c.Filter
	if f == nil {
		return nil
	}

	if err := validateConfigurationFilter(f.Prefix, f.Tag, f.AccessPointArn, f.And != nil); err != nil {
		return err
	}

	if f.And != nil {
		return validateConfigurationAnd(f.And.Prefix, f.And.Tags, f.And.AccessPointArn)
	}

	return nil
}

var inventoryFields = []string{
	"BucketKeyStatus", "ChecksumAlgorithm", attributeETag, "EncryptionStatus", "IntelligentTieringAccessTier",
	"IntelligentTieringReferenceDate", "IsMultipartUploaded", "LastModifiedDate", "LifecycleExpirationDate",
	"ObjectAccessControlList", "ObjectLockEventHoldDuration", "ObjectLockEventHoldStatus", "ObjectLockLegalHoldStatus",
	"ObjectLockMode", "ObjectLockRetainUntilDate", "ObjectOwner", "ReplicationStatus", "Size", attributeStorageClass,
}

func validateInventory(c *api.InventoryConfiguration) error {
	if c.IsEnabled == nil || c.Destination == nil || c.Destination.S3BucketDestination == nil || c.Schedule == nil {
		return malformedXML()
	}

	if versions := common.Value(c.IncludedObjectVersions); versions != "All" && versions != "Current" {
		return malformedXML()
	}

	if frequency := common.Value(c.Schedule.Frequency); frequency != "Daily" && frequency != "Weekly" {
		return malformedXML()
	}

	if c.Filter != nil && (c.Filter.Prefix == nil || len(*c.Filter.Prefix) > maxKeyBytes) {
		return malformedXML()
	}

	dest := c.Destination.S3BucketDestination
	_, ok := bucketARNName(common.Value(dest.Bucket))
	if !ok || (dest.AccountId != nil && !accountNumber.MatchString(*dest.AccountId)) ||
		!slices.Contains([]string{"CSV", "ORC", "Parquet"}, common.Value(dest.Format)) {
		return malformedXML()
	}

	if e := dest.Encryption; e != nil && (count(e.SSES3 != nil, e.SSEKMS != nil) != 1 ||
		(e.SSEKMS != nil && common.Value(e.SSEKMS.KeyId) == "")) {
		return malformedXML()
	}

	seen := map[string]bool{}
	for _, field := range c.OptionalFields {
		if seen[field] || !slices.Contains(inventoryFields, field) {
			return malformedXML()
		}

		seen[field] = true
	}

	return nil
}

func validateIntelligentTiering(c *api.IntelligentTieringConfiguration) error {
	if !enabledDisabled(c.Status) || len(c.Tierings) == 0 || len(c.Tierings) > 2 {
		return malformedXML()
	}

	if f := c.Filter; f != nil {
		if err := validateConfigurationFilter(f.Prefix, f.Tag, nil, f.And != nil); err != nil {
			return err
		}

		if f.And != nil {
			if err := validateConfigurationAnd(f.And.Prefix, f.And.Tags, nil); err != nil {
				return err
			}
		}
	}

	var archive, deep int64
	for _, tier := range c.Tierings {
		days := common.Value(tier.Days)
		switch common.Value(tier.AccessTier) {
		case "ARCHIVE_ACCESS":
			if archive != 0 || days < 90 || days > 730 {
				return malformedXML()
			}

			archive = days
		case "DEEP_ARCHIVE_ACCESS":
			if deep != 0 || days < 180 || days > 730 {
				return malformedXML()
			}

			deep = days
		default:
			return malformedXML()
		}
	}

	if deep != 0 && archive > deep {
		return malformedXML()
	}

	return nil
}

func (s *Service) PutBucketAnalyticsConfiguration(
	ctx context.Context, in *api.PutBucketAnalyticsConfigurationRequest,
) (*api.PutBucketAnalyticsConfigurationOutput, error) {
	cfg := in.AnalyticsConfiguration
	if cfg == nil || cfg.Id == nil || common.Value(in.Id) != *cfg.Id {
		return nil, malformedXML()
	}

	if err := validateAnalytics(cfg); err != nil {
		return nil, err
	}

	err := s.changeBucketConfiguration(ctx, in.Bucket, in.ExpectedBucketOwner, in.Id, configAnalytics, cfg)
	if err != nil {
		return nil, err
	}

	return &api.PutBucketAnalyticsConfigurationOutput{}, nil
}

func (s *Service) GetBucketAnalyticsConfiguration(
	ctx context.Context, in *api.GetBucketAnalyticsConfigurationRequest,
) (*api.GetBucketAnalyticsConfigurationOutput, error) {
	cfg, err := getBucketConfiguration[api.AnalyticsConfiguration](
		ctx, s, in.Bucket, in.ExpectedBucketOwner, in.Id, configAnalytics,
	)
	if err != nil {
		return nil, err
	}

	return &api.GetBucketAnalyticsConfigurationOutput{AnalyticsConfiguration: cfg}, nil
}

func (s *Service) DeleteBucketAnalyticsConfiguration(
	ctx context.Context, in *api.DeleteBucketAnalyticsConfigurationRequest,
) (*api.DeleteBucketAnalyticsConfigurationOutput, error) {
	err := s.changeBucketConfiguration(ctx, in.Bucket, in.ExpectedBucketOwner, in.Id, configAnalytics, nil)
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketAnalyticsConfigurationOutput{}, nil
}

func (s *Service) ListBucketAnalyticsConfigurations(
	ctx context.Context, in *api.ListBucketAnalyticsConfigurationsRequest,
) (*api.ListBucketAnalyticsConfigurationsOutput, error) {
	list, next, err := listBucketConfigurations[api.AnalyticsConfiguration](
		ctx, s, in.Bucket, in.ExpectedBucketOwner, in.ContinuationToken, configAnalytics,
	)
	if err != nil {
		return nil, err
	}

	return &api.ListBucketAnalyticsConfigurationsOutput{
		AnalyticsConfigurationList: list, ContinuationToken: in.ContinuationToken,
		NextContinuationToken: next, IsTruncated: new(next != nil),
	}, nil
}

func (s *Service) PutBucketMetricsConfiguration(
	ctx context.Context, in *api.PutBucketMetricsConfigurationRequest,
) (*api.PutBucketMetricsConfigurationOutput, error) {
	cfg := in.MetricsConfiguration
	if cfg == nil || cfg.Id == nil || common.Value(in.Id) != *cfg.Id {
		return nil, malformedXML()
	}

	if err := validateMetrics(cfg); err != nil {
		return nil, err
	}

	err := s.changeBucketConfiguration(ctx, in.Bucket, in.ExpectedBucketOwner, in.Id, configMetrics, cfg)
	if err != nil {
		return nil, err
	}

	return &api.PutBucketMetricsConfigurationOutput{}, nil
}

func (s *Service) GetBucketMetricsConfiguration(
	ctx context.Context, in *api.GetBucketMetricsConfigurationRequest,
) (*api.GetBucketMetricsConfigurationOutput, error) {
	cfg, err := getBucketConfiguration[api.MetricsConfiguration](
		ctx, s, in.Bucket, in.ExpectedBucketOwner, in.Id, configMetrics,
	)
	if err != nil {
		return nil, err
	}

	return &api.GetBucketMetricsConfigurationOutput{MetricsConfiguration: cfg}, nil
}

func (s *Service) DeleteBucketMetricsConfiguration(
	ctx context.Context, in *api.DeleteBucketMetricsConfigurationRequest,
) (*api.DeleteBucketMetricsConfigurationOutput, error) {
	err := s.changeBucketConfiguration(ctx, in.Bucket, in.ExpectedBucketOwner, in.Id, configMetrics, nil)
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketMetricsConfigurationOutput{}, nil
}

func (s *Service) ListBucketMetricsConfigurations(
	ctx context.Context, in *api.ListBucketMetricsConfigurationsRequest,
) (*api.ListBucketMetricsConfigurationsOutput, error) {
	list, next, err := listBucketConfigurations[api.MetricsConfiguration](
		ctx, s, in.Bucket, in.ExpectedBucketOwner, in.ContinuationToken, configMetrics,
	)
	if err != nil {
		return nil, err
	}

	return &api.ListBucketMetricsConfigurationsOutput{
		MetricsConfigurationList: list, ContinuationToken: in.ContinuationToken,
		NextContinuationToken: next, IsTruncated: new(next != nil),
	}, nil
}

func (s *Service) PutBucketInventoryConfiguration(
	ctx context.Context, in *api.PutBucketInventoryConfigurationRequest,
) (*api.PutBucketInventoryConfigurationOutput, error) {
	cfg := in.InventoryConfiguration
	if cfg == nil || cfg.Id == nil || common.Value(in.Id) != *cfg.Id {
		return nil, malformedXML()
	}

	if err := validateInventory(cfg); err != nil {
		return nil, err
	}

	err := s.changeBucketConfiguration(ctx, in.Bucket, in.ExpectedBucketOwner, in.Id, configInventory, cfg)
	if err != nil {
		return nil, err
	}

	return &api.PutBucketInventoryConfigurationOutput{}, nil
}

func (s *Service) GetBucketInventoryConfiguration(
	ctx context.Context, in *api.GetBucketInventoryConfigurationRequest,
) (*api.GetBucketInventoryConfigurationOutput, error) {
	cfg, err := getBucketConfiguration[api.InventoryConfiguration](
		ctx, s, in.Bucket, in.ExpectedBucketOwner, in.Id, configInventory,
	)
	if err != nil {
		return nil, err
	}

	return &api.GetBucketInventoryConfigurationOutput{InventoryConfiguration: cfg}, nil
}

func (s *Service) DeleteBucketInventoryConfiguration(
	ctx context.Context, in *api.DeleteBucketInventoryConfigurationRequest,
) (*api.DeleteBucketInventoryConfigurationOutput, error) {
	err := s.changeBucketConfiguration(ctx, in.Bucket, in.ExpectedBucketOwner, in.Id, configInventory, nil)
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketInventoryConfigurationOutput{}, nil
}

func (s *Service) ListBucketInventoryConfigurations(
	ctx context.Context, in *api.ListBucketInventoryConfigurationsRequest,
) (*api.ListBucketInventoryConfigurationsOutput, error) {
	list, next, err := listBucketConfigurations[api.InventoryConfiguration](
		ctx, s, in.Bucket, in.ExpectedBucketOwner, in.ContinuationToken, configInventory,
	)
	if err != nil {
		return nil, err
	}

	return &api.ListBucketInventoryConfigurationsOutput{
		InventoryConfigurationList: list, ContinuationToken: in.ContinuationToken,
		NextContinuationToken: next, IsTruncated: new(next != nil),
	}, nil
}

func (s *Service) PutBucketIntelligentTieringConfiguration(
	ctx context.Context, in *api.PutBucketIntelligentTieringConfigurationRequest,
) (*api.PutBucketIntelligentTieringConfigurationOutput, error) {
	cfg := in.IntelligentTieringConfiguration
	if cfg == nil || cfg.Id == nil || common.Value(in.Id) != *cfg.Id {
		return nil, malformedXML()
	}

	if err := validateIntelligentTiering(cfg); err != nil {
		return nil, err
	}

	err := s.changeBucketConfiguration(ctx, in.Bucket, in.ExpectedBucketOwner, in.Id, configIntelligent, cfg)
	if err != nil {
		return nil, err
	}

	return &api.PutBucketIntelligentTieringConfigurationOutput{}, nil
}

func (s *Service) GetBucketIntelligentTieringConfiguration(
	ctx context.Context, in *api.GetBucketIntelligentTieringConfigurationRequest,
) (*api.GetBucketIntelligentTieringConfigurationOutput, error) {
	cfg, err := getBucketConfiguration[api.IntelligentTieringConfiguration](
		ctx, s, in.Bucket, in.ExpectedBucketOwner, in.Id, configIntelligent,
	)
	if err != nil {
		return nil, err
	}

	return &api.GetBucketIntelligentTieringConfigurationOutput{IntelligentTieringConfiguration: cfg}, nil
}

func (s *Service) DeleteBucketIntelligentTieringConfiguration(
	ctx context.Context, in *api.DeleteBucketIntelligentTieringConfigurationRequest,
) (*api.DeleteBucketIntelligentTieringConfigurationOutput, error) {
	err := s.changeBucketConfiguration(ctx, in.Bucket, in.ExpectedBucketOwner, in.Id, configIntelligent, nil)
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketIntelligentTieringConfigurationOutput{}, nil
}

func (s *Service) ListBucketIntelligentTieringConfigurations(
	ctx context.Context, in *api.ListBucketIntelligentTieringConfigurationsRequest,
) (*api.ListBucketIntelligentTieringConfigurationsOutput, error) {
	list, next, err := listBucketConfigurations[api.IntelligentTieringConfiguration](
		ctx, s, in.Bucket, in.ExpectedBucketOwner, in.ContinuationToken, configIntelligent,
	)
	if err != nil {
		return nil, err
	}

	return &api.ListBucketIntelligentTieringConfigurationsOutput{
		IntelligentTieringConfigurationList: list, ContinuationToken: in.ContinuationToken,
		NextContinuationToken: next, IsTruncated: new(next != nil),
	}, nil
}
