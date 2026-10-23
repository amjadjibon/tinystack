package servicecatalog

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicecatalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestGeneratedValidation(t *testing.T) {
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
	svc := New(memory.New(nil, nil))
	descriptor := api.Descriptor(svc)
	tests := []struct {
		input any
		op    string
	}{
		{
			op:    "CreatePortfolio",
			input: &api.CreatePortfolioInput{ProviderName: new("local"), IdempotencyToken: new("test")},
		},
		{
			op: "CreateProduct",
			input: &api.CreateProductInput{
				Name:             new("p"),
				Owner:            new("owner"),
				ProductType:      new("invalid"),
				IdempotencyToken: new("test"),
			},
		},
		{op: "AssociatePrincipalWithPortfolio", input: &api.AssociatePrincipalWithPortfolioInput{
			PortfolioId:  new("port-test"),
			PrincipalARN: new("arn:aws:iam::000000000000:role/x"), PrincipalType: new("invalid"),
		}},
		{op: "ListPortfolios", input: &api.ListPortfoliosInput{PageSize: new(int64(-1))}},
		{op: "ListPortfolios", input: &api.ListPortfoliosInput{PageSize: new(int64(101))}},
		{op: "UpdateProvisioningArtifact", input: &api.UpdateProvisioningArtifactInput{
			ProductId:              new("prod-test"),
			ProvisioningArtifactId: new("pa-test"), Guidance: new("invalid"),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.op, func(t *testing.T) {
			_, err := descriptor.Operations[tt.op].Invoke(ctx, tt.input)
			var awsErr *awserror.Error
			if !errors.As(err, &awsErr) || awsErr.Code != "InvalidParametersException" {
				t.Fatalf("want InvalidParametersException, got %v", err)
			}
		})
	}
}

func TestConstraintRules(t *testing.T) {
	for _, typ := range []string{"LAUNCH", "NOTIFICATION", "RESOURCE_UPDATE", "STACKSET", "TEMPLATE"} {
		parameters := "{}"
		if typ == "LAUNCH" {
			parameters = `{"RoleArn":"arn:aws:iam::000000000000:role/launch"}`
		}

		if err := validateConstraint(typ, parameters); err != nil {
			t.Fatal(err)
		}
	}

	for _, parameters := range []string{
		"[]", "null", "not-json", "{}", `{"RoleArn":2}`,
		`{"RoleArn":"","LocalRoleName":"b"}`, `{"RoleArn":"a","LocalRoleName":"b"}`,
	} {
		if err := validateConstraint("LAUNCH", parameters); err == nil {
			t.Fatalf("accepted %s", parameters)
		}
	}

	if err := validateConstraint("UNKNOWN", "{}"); err == nil {
		t.Fatal("accepted unknown constraint type")
	}
}
