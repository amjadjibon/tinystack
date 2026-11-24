package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3BucketRefreshOptions(t *testing.T) {
	ctx := context.Background()
	c, bucket := checksumBucket(t, server(t))

	acceleration, err := c.GetBucketAccelerateConfiguration(ctx, &s3.GetBucketAccelerateConfigurationInput{
		Bucket: bucket,
	})
	if err != nil || acceleration.Status != "" {
		t.Fatalf("unset acceleration: %+v %v", acceleration, err)
	}

	payment, err := c.GetBucketRequestPayment(ctx, &s3.GetBucketRequestPaymentInput{Bucket: bucket})
	if err != nil || payment.Payer != s3types.PayerBucketOwner {
		t.Fatalf("default request payment: %+v %v", payment, err)
	}

	_, err = c.PutBucketAccelerateConfiguration(ctx, &s3.PutBucketAccelerateConfigurationInput{
		Bucket: bucket, AccelerateConfiguration: &s3types.AccelerateConfiguration{
			Status: s3types.BucketAccelerateStatusEnabled,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	acceleration, err = c.GetBucketAccelerateConfiguration(
		ctx,
		&s3.GetBucketAccelerateConfigurationInput{Bucket: bucket},
	)
	if err != nil || acceleration.Status != s3types.BucketAccelerateStatusEnabled {
		t.Fatalf("stored acceleration: %+v %v", acceleration, err)
	}

	_, err = c.PutBucketRequestPayment(ctx, &s3.PutBucketRequestPaymentInput{
		Bucket:                      bucket,
		RequestPaymentConfiguration: &s3types.RequestPaymentConfiguration{Payer: s3types.PayerRequester},
	})
	if err != nil {
		t.Fatal(err)
	}

	payment, err = c.GetBucketRequestPayment(ctx, &s3.GetBucketRequestPaymentInput{Bucket: bucket})
	if err != nil || payment.Payer != s3types.PayerRequester {
		t.Fatalf("stored request payment: %+v %v", payment, err)
	}
}

func TestS3BucketReportConfigurations(t *testing.T) {
	ctx := context.Background()
	c, bucket := checksumBucket(t, server(t))
	id := new("daily-report")

	t.Run("analytics", func(t *testing.T) {
		_, err := c.PutBucketAnalyticsConfiguration(ctx, &s3.PutBucketAnalyticsConfigurationInput{
			Bucket: bucket, Id: id, AnalyticsConfiguration: &s3types.AnalyticsConfiguration{
				Id: id, Filter: &s3types.AnalyticsFilterMemberPrefix{Value: "photos/"},
				StorageClassAnalysis: &s3types.StorageClassAnalysis{},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		get := &s3.GetBucketAnalyticsConfigurationInput{Bucket: bucket, Id: id}
		out, err := c.GetBucketAnalyticsConfiguration(ctx, get)
		if err != nil || *out.AnalyticsConfiguration.Id != *id || out.AnalyticsConfiguration.Filter == nil {
			t.Fatalf("get: %+v %v", out, err)
		}

		list, err := c.ListBucketAnalyticsConfigurations(
			ctx,
			&s3.ListBucketAnalyticsConfigurationsInput{Bucket: bucket},
		)
		if err != nil || len(list.AnalyticsConfigurationList) != 1 {
			t.Fatalf("list: %+v %v", list, err)
		}

		if _, deleteErr := c.DeleteBucketAnalyticsConfiguration(ctx, &s3.DeleteBucketAnalyticsConfigurationInput{
			Bucket: bucket, Id: id,
		}); deleteErr != nil {
			t.Fatal(deleteErr)
		}

		_, err = c.GetBucketAnalyticsConfiguration(ctx, get)
		if code, status := s3Code(err); code != "NoSuchConfiguration" || status != http.StatusNotFound {
			t.Fatal(err)
		}
	})

	t.Run("inventory", func(t *testing.T) {
		_, err := c.PutBucketInventoryConfiguration(ctx, &s3.PutBucketInventoryConfigurationInput{
			Bucket: bucket, Id: id, InventoryConfiguration: &s3types.InventoryConfiguration{
				Id: id, IsEnabled: new(true), IncludedObjectVersions: s3types.InventoryIncludedObjectVersionsAll,
				Schedule: &s3types.InventorySchedule{Frequency: s3types.InventoryFrequencyDaily},
				Destination: &s3types.InventoryDestination{S3BucketDestination: &s3types.InventoryS3BucketDestination{
					Bucket: new("arn:aws:s3:::" + *bucket), Format: s3types.InventoryFormatCsv,
				}},
				OptionalFields: []s3types.InventoryOptionalField{s3types.InventoryOptionalFieldSize},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		get := &s3.GetBucketInventoryConfigurationInput{Bucket: bucket, Id: id}
		out, err := c.GetBucketInventoryConfiguration(ctx, get)
		if err != nil || !*out.InventoryConfiguration.IsEnabled || len(out.InventoryConfiguration.OptionalFields) != 1 {
			t.Fatalf("get: %+v %v", out, err)
		}

		list, err := c.ListBucketInventoryConfigurations(
			ctx,
			&s3.ListBucketInventoryConfigurationsInput{Bucket: bucket},
		)
		if err != nil || len(list.InventoryConfigurationList) != 1 {
			t.Fatalf("list: %+v %v", list, err)
		}

		if _, deleteErr := c.DeleteBucketInventoryConfiguration(ctx, &s3.DeleteBucketInventoryConfigurationInput{
			Bucket: bucket, Id: id,
		}); deleteErr != nil {
			t.Fatal(deleteErr)
		}

		_, err = c.GetBucketInventoryConfiguration(ctx, get)
		if code, _ := s3Code(err); code != "NoSuchConfiguration" {
			t.Fatal(err)
		}
	})

	t.Run("intelligent-tiering", func(t *testing.T) {
		cfg := &s3types.IntelligentTieringConfiguration{
			Id:     id,
			Status: s3types.IntelligentTieringStatusEnabled,
			Tierings: []s3types.Tiering{
				{AccessTier: s3types.IntelligentTieringAccessTierArchiveAccess, Days: new(int32(90))},
			},
		}
		if _, err := c.PutBucketIntelligentTieringConfiguration(ctx, &s3.PutBucketIntelligentTieringConfigurationInput{
			Bucket: bucket, Id: id, IntelligentTieringConfiguration: cfg,
		}); err != nil {
			t.Fatal(err)
		}

		get := &s3.GetBucketIntelligentTieringConfigurationInput{Bucket: bucket, Id: id}
		out, err := c.GetBucketIntelligentTieringConfiguration(ctx, get)
		if err != nil || len(out.IntelligentTieringConfiguration.Tierings) != 1 ||
			*out.IntelligentTieringConfiguration.Tierings[0].Days != 90 {
			t.Fatalf("get: %+v %v", out, err)
		}

		list, err := c.ListBucketIntelligentTieringConfigurations(ctx,
			&s3.ListBucketIntelligentTieringConfigurationsInput{Bucket: bucket})
		if err != nil || len(list.IntelligentTieringConfigurationList) != 1 {
			t.Fatalf("list: %+v %v", list, err)
		}

		if _, deleteErr := c.DeleteBucketIntelligentTieringConfiguration(
			ctx,
			&s3.DeleteBucketIntelligentTieringConfigurationInput{
				Bucket: bucket, Id: id,
			},
		); deleteErr != nil {
			t.Fatal(deleteErr)
		}

		_, err = c.GetBucketIntelligentTieringConfiguration(ctx, get)
		if code, _ := s3Code(err); code != "NoSuchConfiguration" {
			t.Fatal(err)
		}
	})

	t.Run("metrics pagination", func(t *testing.T) {
		for i := range 101 {
			name := new(fmt.Sprintf("metrics-%03d", i))
			cfg := &s3types.MetricsConfiguration{
				Id: name, Filter: &s3types.MetricsFilterMemberPrefix{Value: "photos/"},
			}
			_, err := c.PutBucketMetricsConfiguration(ctx, &s3.PutBucketMetricsConfigurationInput{
				Bucket: bucket, Id: name, MetricsConfiguration: cfg,
			})
			if err != nil {
				t.Fatal(err)
			}
		}

		get := &s3.GetBucketMetricsConfigurationInput{Bucket: bucket, Id: new("metrics-100")}
		out, err := c.GetBucketMetricsConfiguration(ctx, get)
		if err != nil || *out.MetricsConfiguration.Id != *get.Id || out.MetricsConfiguration.Filter == nil {
			t.Fatalf("get: %+v %v", out, err)
		}

		list := &s3.ListBucketMetricsConfigurationsInput{Bucket: bucket}
		first, err := c.ListBucketMetricsConfigurations(ctx, list)
		if err != nil || len(first.MetricsConfigurationList) != 100 || !aws.ToBool(first.IsTruncated) {
			t.Fatalf("first page: %+v %v", first, err)
		}

		list.ContinuationToken = first.NextContinuationToken
		last, err := c.ListBucketMetricsConfigurations(ctx, list)
		if err != nil || len(last.MetricsConfigurationList) != 1 || aws.ToBool(last.IsTruncated) ||
			*last.MetricsConfigurationList[0].Id != "metrics-100" {
			t.Fatalf("second page: %+v %v", last, err)
		}

		if _, deleteErr := c.DeleteBucketMetricsConfiguration(ctx, &s3.DeleteBucketMetricsConfigurationInput{
			Bucket: bucket, Id: get.Id,
		}); deleteErr != nil {
			t.Fatal(deleteErr)
		}

		_, err = c.GetBucketMetricsConfiguration(ctx, get)
		if code, _ := s3Code(err); code != "NoSuchConfiguration" {
			t.Fatal(err)
		}
	})
}
