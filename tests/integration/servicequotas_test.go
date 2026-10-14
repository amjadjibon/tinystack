package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
	"github.com/aws/smithy-go"
)

func wantQuotaError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestServiceQuotasSDK(t *testing.T) {
	server := server(t)
	ctx := context.Background()
	client := func(key, region string) *sdk.Client {
		return sdk.New(sdk.Options{
			Region: region, BaseEndpoint: new(server.URL), Credentials: creds(key), Retryer: aws.NopRetryer{},
		})
	}
	c := client("test", "us-east-1")
	services := []string{}
	var token *string
	for {
		page, err := c.ListServices(ctx, &sdk.ListServicesInput{MaxResults: new(int32(2)), NextToken: token})
		if err != nil {
			t.Fatal(err)
		}

		for _, item := range page.Services {
			services = append(services, aws.ToString(item.ServiceCode))
		}

		token = page.NextToken
		if token == nil {
			break
		}
	}

	if strings.Join(services, ",") != "dynamodb,ec2,iam,lambda,s3,sqs,vpc" {
		t.Fatal(services)
	}

	for _, service := range services {
		defaults, err := c.ListAWSDefaultServiceQuotas(
			ctx,
			&sdk.ListAWSDefaultServiceQuotasInput{ServiceCode: new(service)},
		)
		if err != nil || len(defaults.Quotas) == 0 {
			t.Fatal(defaults, err)
		}

		applied, err := c.ListServiceQuotas(ctx, &sdk.ListServiceQuotasInput{ServiceCode: new(service)})
		if err != nil || len(applied.Quotas) != len(defaults.Quotas) {
			t.Fatal(applied, err)
		}

		for _, q := range defaults.Quotas {
			got, err := c.GetServiceQuota(
				ctx,
				&sdk.GetServiceQuotaInput{ServiceCode: new(service), QuotaCode: q.QuotaCode},
			)
			if err != nil || *got.Quota.Value != *q.Value || !strings.Contains(*got.Quota.QuotaArn, ":000000000000:") {
				t.Fatal(got, err)
			}

			def, err := c.GetAWSDefaultServiceQuota(ctx, &sdk.GetAWSDefaultServiceQuotaInput{
				ServiceCode: new(service),
				QuotaCode:   q.QuotaCode,
			})
			if err != nil || *def.Quota.Value != *q.Value || !strings.Contains(*def.Quota.QuotaArn, "::"+service+"/") {
				t.Fatal(def, err)
			}
		}
	}

	page, err := c.ListServiceQuotas(
		ctx,
		&sdk.ListServiceQuotasInput{ServiceCode: new("ec2"), MaxResults: new(int32(1))},
	)
	if err != nil || len(page.Quotas) != 1 || page.NextToken == nil {
		t.Fatal(page, err)
	}

	last, err := c.ListServiceQuotas(ctx, &sdk.ListServiceQuotasInput{
		ServiceCode: new("ec2"),
		MaxResults:  new(int32(1)),
		NextToken:   page.NextToken,
	})
	if err != nil || len(last.Quotas) != 1 || last.NextToken != nil ||
		*last.Quotas[0].QuotaCode == *page.Quotas[0].QuotaCode {
		t.Fatal(last, err)
	}

	ids := []string{}
	for _, code := range []string{"L-0263D0A3", "L-1216C47A"} {
		created, createErr := c.RequestServiceQuotaIncrease(ctx, &sdk.RequestServiceQuotaIncreaseInput{
			ServiceCode: new("ec2"), QuotaCode: new(code), DesiredValue: new(10.0),
		})
		if createErr != nil || created.RequestedQuota.Status != types.RequestStatusPending ||
			created.RequestedQuota.Created == nil {
			t.Fatal(created, createErr)
		}

		ids = append(ids, *created.RequestedQuota.Id)
		got, getErr := c.GetRequestedServiceQuotaChange(ctx, &sdk.GetRequestedServiceQuotaChangeInput{
			RequestId: created.RequestedQuota.Id,
		})
		if getErr != nil || *got.RequestedQuota.DesiredValue != 10 || *got.RequestedQuota.QuotaCode != code {
			t.Fatal(got, getErr)
		}

		current, currentErr := c.GetServiceQuota(
			ctx,
			&sdk.GetServiceQuotaInput{ServiceCode: new("ec2"), QuotaCode: new(code)},
		)
		if currentErr != nil || *current.Quota.Value != 5 {
			t.Fatal(current, currentErr)
		}
	}

	history, err := c.ListRequestedServiceQuotaChangeHistory(ctx, &sdk.ListRequestedServiceQuotaChangeHistoryInput{
		MaxResults: new(int32(1)),
	})
	if err != nil || len(history.RequestedQuotas) != 1 || history.NextToken == nil {
		t.Fatal(history, err)
	}

	next, err := c.ListRequestedServiceQuotaChangeHistory(ctx, &sdk.ListRequestedServiceQuotaChangeHistoryInput{
		NextToken: history.NextToken,
	})
	if err != nil || len(next.RequestedQuotas) != 1 || *next.RequestedQuotas[0].Id == *history.RequestedQuotas[0].Id {
		t.Fatal(next, err)
	}

	filtered, err := c.ListRequestedServiceQuotaChangeHistoryByQuota(ctx,
		&sdk.ListRequestedServiceQuotaChangeHistoryByQuotaInput{
			ServiceCode: new("ec2"), QuotaCode: new("L-0263D0A3"), Status: types.RequestStatusPending,
		})
	if err != nil || len(filtered.RequestedQuotas) != 1 {
		t.Fatal(filtered, err)
	}

	approved, err := c.ListRequestedServiceQuotaChangeHistory(ctx, &sdk.ListRequestedServiceQuotaChangeHistoryInput{
		Status: types.RequestStatusApproved,
	})
	if err != nil || len(approved.RequestedQuotas) != 0 {
		t.Fatal(approved, err)
	}

	for _, other := range []*sdk.Client{client("team-b", "us-east-1"), client("test", "us-west-2")} {
		empty, listErr := other.ListRequestedServiceQuotaChangeHistory(
			ctx,
			&sdk.ListRequestedServiceQuotaChangeHistoryInput{},
		)
		if listErr != nil || len(empty.RequestedQuotas) != 0 {
			t.Fatal(empty, listErr)
		}

		_, lookupErr := other.GetRequestedServiceQuotaChange(
			ctx,
			&sdk.GetRequestedServiceQuotaChangeInput{RequestId: new(ids[0])},
		)
		wantQuotaError(t, lookupErr, "NoSuchResourceException")
	}

	_, err = c.RequestServiceQuotaIncrease(ctx, &sdk.RequestServiceQuotaIncreaseInput{
		ServiceCode:  new("ec2"),
		QuotaCode:    new("L-0263D0A3"),
		DesiredValue: new(20.0),
	})
	wantQuotaError(t, err, "ResourceAlreadyExistsException")
	_, err = c.RequestServiceQuotaIncrease(ctx, &sdk.RequestServiceQuotaIncreaseInput{
		ServiceCode:  new("lambda"),
		QuotaCode:    new("L-B99A9384"),
		DesiredValue: new(1000.0),
	})
	wantQuotaError(t, err, "IllegalArgumentException")
	_, err = c.GetServiceQuota(
		ctx,
		&sdk.GetServiceQuotaInput{ServiceCode: new("unknown"), QuotaCode: new("L-0263D0A3")},
	)
	wantQuotaError(t, err, "NoSuchResourceException")
	_, err = c.GetServiceQuota(ctx, &sdk.GetServiceQuotaInput{ServiceCode: new("ec2"), QuotaCode: new("missing")})
	wantQuotaError(t, err, "NoSuchResourceException")
	_, err = c.ListServiceQuotas(ctx, &sdk.ListServiceQuotasInput{ServiceCode: new("missing")})
	wantQuotaError(t, err, "NoSuchResourceException")
	_, err = c.RequestServiceQuotaIncrease(ctx, &sdk.RequestServiceQuotaIncreaseInput{
		ServiceCode: new("sqs"), QuotaCode: new("L-9F628B95"), DesiredValue: new(30.0),
	})
	wantQuotaError(t, err, "IllegalArgumentException")

	_, err = c.ListServices(ctx, &sdk.ListServicesInput{MaxResults: new(int32(101))})
	wantQuotaError(t, err, "IllegalArgumentException")
	_, err = c.ListServices(ctx, &sdk.ListServicesInput{NextToken: new("!")})
	wantQuotaError(t, err, "InvalidPaginationTokenException")
	_, err = c.ListRequestedServiceQuotaChangeHistory(ctx, &sdk.ListRequestedServiceQuotaChangeHistoryInput{
		Status: types.RequestStatus("BOGUS"),
	})
	wantQuotaError(t, err, "IllegalArgumentException")
	_, err = c.GetServiceQuota(ctx, &sdk.GetServiceQuotaInput{
		ServiceCode: new("ec2"),
		QuotaCode:   new("L-0263D0A3"),
		ContextId:   new("arn:aws:ec2:us-east-1:000000000000:vpc/vpc-x"),
	})
	wantQuotaError(t, err, "ValidationException")
}

func TestServiceQuotasSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	s, stop := persistent(t, path, &fakeRuntime{})
	c := sdk.New(sdk.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{},
	})
	created, err := c.RequestServiceQuotaIncrease(ctx, &sdk.RequestServiceQuotaIncreaseInput{
		ServiceCode: new("lambda"), QuotaCode: new("L-B99A9384"), DesiredValue: new(2000.0),
	})
	stop()
	if err != nil {
		t.Fatal(err)
	}

	s, stop = persistent(t, path, &fakeRuntime{})
	t.Cleanup(stop)
	c = sdk.New(sdk.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{},
	})
	got, err := c.GetRequestedServiceQuotaChange(ctx, &sdk.GetRequestedServiceQuotaChangeInput{
		RequestId: created.RequestedQuota.Id,
	})
	if err != nil || got.RequestedQuota.Status != types.RequestStatusPending ||
		*got.RequestedQuota.DesiredValue != 2000 || !got.RequestedQuota.Created.Equal(*created.RequestedQuota.Created) {
		t.Fatal(got, err)
	}

	current, err := c.GetServiceQuota(ctx, &sdk.GetServiceQuotaInput{
		ServiceCode: new("lambda"), QuotaCode: new("L-B99A9384"),
	})
	if err != nil || *current.Quota.Value != 1000 {
		t.Fatal(current, err)
	}

	_, err = c.RequestServiceQuotaIncrease(ctx, &sdk.RequestServiceQuotaIncreaseInput{
		ServiceCode: new("lambda"), QuotaCode: new("L-B99A9384"), DesiredValue: new(3000.0),
	})
	wantQuotaError(t, err, "ResourceAlreadyExistsException")
}
