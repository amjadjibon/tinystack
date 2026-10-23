package securityhub_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/securityhub"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func securityHubContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
}

func TestDisabledHubGatesEveryOperation(t *testing.T) {
	service := securityhub.New(memory.New(nil, nil))
	descriptor := api.Descriptor(service)
	ctx := securityHubContext()
	for name, operation := range descriptor.Operations {
		if name == "EnableSecurityHub" {
			continue
		}

		t.Run(name, func(t *testing.T) {
			// Call the method itself: Invoke checks the model constraints first, as AWS does.
			results := reflect.ValueOf(service).MethodByName(name).Call(
				[]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(operation.NewInput())},
			)
			err, _ := results[1].Interface().(error)
			var awsErr *awserror.Error
			if !errors.As(err, &awsErr) || awsErr.Code != "InvalidAccessException" {
				t.Fatalf("want InvalidAccessException, got %v", err)
			}
		})
	}
}

func validFinding() api.AwsSecurityFinding {
	return api.AwsSecurityFinding{
		SchemaVersion: new(
			"2018-10-08",
		),
		Id:           new("local"),
		ProductArn:   new("arn:aws:securityhub:us-east-1:123456789012:product/123456789012/default"),
		GeneratorId:  new("local"),
		AwsAccountId: new("123456789012"),
		Types:        []string{"Effects/Data Exposure"},
		CreatedAt: new(
			"2026-01-01T00:00:00Z",
		),
		UpdatedAt: new("2026-01-01T00:00:00Z"),
		Severity:  &api.Severity{Label: new("HIGH")},
		Title: new(
			"Finding",
		),
		Description: new("A local finding"),
		Resources:   []api.Resource{{Type: new("AwsS3Bucket"), Id: new("arn:aws:s3:::local")}},
	}
}

func TestImportRequiredFieldsAndConstraints(t *testing.T) {
	service := securityhub.New(memory.New(nil, nil))
	ctx := securityHubContext()
	if _, err := service.EnableSecurityHub(
		ctx,
		&api.EnableSecurityHubRequest{EnableDefaultStandards: new(false)},
	); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{
		"SchemaVersion", "Id", "ProductArn", "GeneratorId", "AwsAccountId", "Types",
		"CreatedAt", "UpdatedAt", "Severity", "Title", "Description", "Resources",
	} {
		t.Run(field, func(t *testing.T) {
			finding := validFinding()
			value := reflect.ValueOf(&finding).Elem().FieldByName(field)
			value.SetZero()
			out, err := service.BatchImportFindings(
				ctx,
				&api.BatchImportFindingsRequest{Findings: []api.AwsSecurityFinding{finding}},
			)
			if err != nil || *out.FailedCount != 1 || *out.SuccessCount != 0 || len(out.FailedFindings) != 1 {
				t.Fatalf("missing %s: %+v %v", field, out, err)
			}
		})
	}

	for name, mutate := range map[string]func(*api.AwsSecurityFinding){
		"enum":     func(f *api.AwsSecurityFinding) { f.Severity.Label = new("INVALID") },
		"resource": func(f *api.AwsSecurityFinding) { f.Resources[0].Type = nil },
		"title":    func(f *api.AwsSecurityFinding) { f.Title = new("") },
		"date":     func(f *api.AwsSecurityFinding) { f.CreatedAt = new("invalid") },
	} {
		t.Run(name, func(t *testing.T) {
			finding := validFinding()
			mutate(&finding)
			out, err := service.BatchImportFindings(
				ctx,
				&api.BatchImportFindingsRequest{Findings: []api.AwsSecurityFinding{finding}},
			)
			if err != nil || *out.FailedCount != 1 {
				t.Fatalf("constraint %s: %+v %v", name, out, err)
			}
		})
	}

	out, err := service.GetFindings(ctx, &api.GetFindingsRequest{})
	if err != nil || len(out.Findings) != 0 {
		t.Fatalf("invalid records were stored: %+v %v", out, err)
	}
}
