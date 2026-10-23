package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/servicecatalog"
	"github.com/aws/aws-sdk-go-v2/service/servicecatalog/types"
	"github.com/aws/smithy-go"
)

func serviceCatalogClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func scError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func scOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func scCreateProduct(t *testing.T, c *sdk.Client, name string) *sdk.CreateProductOutput {
	t.Helper()
	out, err := c.CreateProduct(context.Background(), &sdk.CreateProductInput{
		Name: new(name), Owner: new("team"),
		ProductType: types.ProductTypeCloudFormationTemplate,
		ProvisioningArtifactParameters: &types.ProvisioningArtifactProperties{
			Name: new("v1"),
			Info: map[string]string{"LoadTemplateFromURL": "https://example.invalid/never-fetched.json"},
		},
		Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
	})
	scOK(t, err)
	return out
}

func TestServiceCatalogLifecycle(t *testing.T) {
	ctx := context.Background()
	srv := server(t)
	c := serviceCatalogClient(srv)
	pin := &sdk.CreatePortfolioInput{
		DisplayName: new("catalog"), ProviderName: new("TinyStack"), IdempotencyToken: new("portfolio"),
		Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
	}

	p, err := c.CreatePortfolio(ctx, pin)
	scOK(t, err)
	replay, err := c.CreatePortfolio(ctx, pin)
	scOK(t, err)
	if *p.PortfolioDetail.Id != *replay.PortfolioDetail.Id {
		t.Fatal("create token did not replay")
	}

	pin.DisplayName = new("changed")
	_, err = c.CreatePortfolio(ctx, pin)
	scError(t, err, "InvalidParametersException")
	port := p.PortfolioDetail.Id
	got, err := c.DescribePortfolio(ctx, &sdk.DescribePortfolioInput{Id: port})
	scOK(t, err)
	if len(got.Tags) != 1 || *got.PortfolioDetail.DisplayName != "catalog" {
		t.Fatalf("portfolio: %+v", got)
	}

	_, err = c.UpdatePortfolio(ctx, &sdk.UpdatePortfolioInput{
		Id:          port,
		DisplayName: new("updated"),
		Description: new("description"),
		ProviderName: new(
			"provider",
		),
		RemoveTags: []string{"team"},
		AddTags:    []types.Tag{{Key: new("env"), Value: new("test")}},
	})
	scOK(t, err)
	prod := scCreateProduct(t, c, "product")
	id := prod.ProductViewDetail.ProductViewSummary.ProductId
	pa := prod.ProvisioningArtifactDetail.Id
	_, err = c.DescribeProduct(ctx, &sdk.DescribeProductInput{Name: new("product")})
	scOK(t, err)
	admin, err := c.DescribeProductAsAdmin(ctx, &sdk.DescribeProductAsAdminInput{Id: id})
	scOK(t, err)
	if len(admin.ProvisioningArtifactSummaries) != 1 || admin.ProductViewDetail.Status != "AVAILABLE" {
		t.Fatalf("admin: %+v", admin)
	}

	_, err = c.UpdateProduct(ctx, &sdk.UpdateProductInput{
		Id:           id,
		Name:         new("renamed"),
		Owner:        new("platform"),
		Description:  new("stored product"),
		Distributor:  new("local"),
		SupportEmail: new("support@example.invalid"),
		SupportUrl: new(
			"https://example.invalid/support",
		),
		SupportDescription: new("support"),
		RemoveTags:         []string{"team"},
	})
	scOK(t, err)
	_, err = c.AssociateProductWithPortfolio(
		ctx,
		&sdk.AssociateProductWithPortfolioInput{PortfolioId: port, ProductId: id},
	)
	scOK(t, err)
	_, err = c.DeletePortfolio(ctx, &sdk.DeletePortfolioInput{Id: port})
	scError(t, err, "ResourceInUseException")
	_, err = c.DeleteProduct(ctx, &sdk.DeleteProductInput{Id: id})
	scError(t, err, "ResourceInUseException")
	associated, err := c.ListPortfoliosForProduct(ctx, &sdk.ListPortfoliosForProductInput{ProductId: id})
	scOK(t, err)
	if len(associated.PortfolioDetails) != 1 {
		t.Fatalf("portfolios: %+v", associated)
	}

	search, err := c.SearchProductsAsAdmin(ctx, &sdk.SearchProductsAsAdminInput{
		PortfolioId: port,
		Filters:     map[string][]string{"Owner": {"platform"}, "ProductType": {"CLOUD_FORMATION_TEMPLATE"}},
	})
	scOK(t, err)
	if len(search.ProductViewDetails) != 1 {
		t.Fatalf("search: %+v", search)
	}

	public, err := c.SearchProducts(
		ctx,
		&sdk.SearchProductsInput{Filters: map[string][]string{"FullTextSearch": {"rename"}}},
	)
	scOK(t, err)
	if len(public.ProductViewSummaries) != 1 {
		t.Fatalf("search: %+v", public)
	}

	_, err = c.SearchProductsAsAdmin(
		ctx,
		&sdk.SearchProductsAsAdminInput{Filters: map[string][]string{"Unsupported": {"x"}}},
	)
	scError(t, err, "InvalidParametersException")
	_, err = c.DeleteProvisioningArtifact(
		ctx,
		&sdk.DeleteProvisioningArtifactInput{ProductId: id, ProvisioningArtifactId: pa},
	)
	scError(t, err, "InvalidParametersException")
	a, err := c.CreateProvisioningArtifact(ctx, &sdk.CreateProvisioningArtifactInput{
		ProductId: id,
		Parameters: &types.ProvisioningArtifactProperties{
			Name: new("v2"), Description: new("new version"),
			Info: map[string]string{"LoadTemplateFromURL": "https://example.invalid/v2.json"},
		},
	})
	scOK(t, err)
	a2 := a.ProvisioningArtifactDetail.Id
	_, err = c.UpdateProvisioningArtifact(ctx, &sdk.UpdateProvisioningArtifactInput{
		ProductId:              id,
		ProvisioningArtifactId: a2,
		Name: new(
			"v2-renamed",
		),
		Description: new("deprecated version"),
		Active:      new(false),
		Guidance:    types.ProvisioningArtifactGuidanceDeprecated,
	})
	scOK(t, err)
	desc, err := c.DescribeProvisioningArtifact(ctx, &sdk.DescribeProvisioningArtifactInput{
		ProductName:              new("renamed"),
		ProvisioningArtifactName: new("v2-renamed"),
	})
	scOK(t, err)
	if *desc.ProvisioningArtifactDetail.Active ||
		desc.ProvisioningArtifactDetail.Guidance != types.ProvisioningArtifactGuidanceDeprecated ||
		desc.Info["TemplateUrl"] != "https://example.invalid/v2.json" {
		t.Fatalf("artifact: %+v", desc)
	}

	artifacts, err := c.ListProvisioningArtifacts(ctx, &sdk.ListProvisioningArtifactsInput{ProductId: id})
	scOK(t, err)
	if len(artifacts.ProvisioningArtifactDetails) != 2 {
		t.Fatalf("versions: %+v", artifacts)
	}

	_, err = c.DeleteProvisioningArtifact(
		ctx,
		&sdk.DeleteProvisioningArtifactInput{ProductId: id, ProvisioningArtifactId: a2},
	)
	scOK(t, err)
	for _, typ := range []types.PrincipalType{types.PrincipalTypeIam, types.PrincipalTypeIamPattern} {
		arn := "arn:aws:iam::000000000000:role/catalog-user"
		if typ == types.PrincipalTypeIamPattern {
			arn = "arn:aws:iam:::role/catalog-*"
		}

		_, err = c.AssociatePrincipalWithPortfolio(ctx, &sdk.AssociatePrincipalWithPortfolioInput{
			PortfolioId:  port,
			PrincipalARN: new(arn), PrincipalType: typ,
		})
		scOK(t, err)
	}

	principals, err := c.ListPrincipalsForPortfolio(
		ctx,
		&sdk.ListPrincipalsForPortfolioInput{PortfolioId: port, PageSize: 1},
	)
	scOK(t, err)
	if len(principals.Principals) != 1 || principals.NextPageToken == nil {
		t.Fatalf("principals: %+v", principals)
	}

	next, err := c.ListPrincipalsForPortfolio(
		ctx,
		&sdk.ListPrincipalsForPortfolioInput{PortfolioId: port, PageToken: principals.NextPageToken},
	)
	scOK(t, err)
	if len(next.Principals) != 1 {
		t.Fatalf("principal page: %+v", next)
	}

	for _, principal := range append(principals.Principals, next.Principals...) {
		_, err = c.DisassociatePrincipalFromPortfolio(ctx, &sdk.DisassociatePrincipalFromPortfolioInput{
			PortfolioId:  port,
			PrincipalARN: principal.PrincipalARN, PrincipalType: principal.PrincipalType,
		})
		scOK(t, err)
	}

	for _, typ := range []string{"LAUNCH", "NOTIFICATION", "RESOURCE_UPDATE", "STACKSET", "TEMPLATE"} {
		parameters := "{}"
		if typ == "LAUNCH" {
			parameters = `{"LocalRoleName":"launch"}`
		}

		con, e := c.CreateConstraint(
			ctx,
			&sdk.CreateConstraintInput{PortfolioId: port, ProductId: id, Type: new(typ), Parameters: new(parameters)},
		)
		scOK(t, e)
		cid := con.ConstraintDetail.ConstraintId
		_, e = c.DisassociateProductFromPortfolio(
			ctx,
			&sdk.DisassociateProductFromPortfolioInput{PortfolioId: port, ProductId: id},
		)
		scError(t, e, "ResourceInUseException")
		_, e = c.DeletePortfolio(ctx, &sdk.DeletePortfolioInput{Id: port})
		scError(t, e, "ResourceInUseException")
		d, e := c.DescribeConstraint(ctx, &sdk.DescribeConstraintInput{Id: cid})
		scOK(t, e)
		if *d.ConstraintParameters != parameters || d.Status != "AVAILABLE" {
			t.Fatalf("constraint: %+v", d)
		}

		_, e = c.UpdateConstraint(
			ctx,
			&sdk.UpdateConstraintInput{Id: cid, Description: new("updated"), Parameters: new(parameters)},
		)
		scOK(t, e)
		list, e := c.ListConstraintsForPortfolio(
			ctx,
			&sdk.ListConstraintsForPortfolioInput{PortfolioId: port, ProductId: id},
		)
		scOK(t, e)
		if len(list.ConstraintDetails) != 1 {
			t.Fatalf("constraints: %+v", list)
		}

		_, e = c.DeleteConstraint(ctx, &sdk.DeleteConstraintInput{Id: cid})
		scOK(t, e)
	}

	for _, parameters := range []string{"not-json", "{}", `{"RoleArn":"a","LocalRoleName":"b"}`} {
		_, err = c.CreateConstraint(
			ctx,
			&sdk.CreateConstraintInput{
				PortfolioId: port,
				ProductId:   id,
				Type:        new("LAUNCH"),
				Parameters:  new(parameters),
			},
		)
		scError(t, err, "InvalidParametersException")
	}

	tagger := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(srv.URL), Credentials: creds("test")})
	tagged, err := tagger.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{*p.PortfolioDetail.ARN, *prod.ProductViewDetail.ProductARN},
		Tags:            map[string]string{"team": "platform"},
	})
	scOK(t, err)
	if len(tagged.FailedResourcesMap) > 0 {
		t.Fatalf("tagging: %+v", tagged)
	}

	untagged, err := tagger.UntagResources(ctx, &rgta.UntagResourcesInput{
		ResourceARNList: []string{*p.PortfolioDetail.ARN, *prod.ProductViewDetail.ProductARN},
		TagKeys:         []string{"team"},
	})
	scOK(t, err)
	if len(untagged.FailedResourcesMap) > 0 {
		t.Fatalf("untagging: %+v", untagged)
	}

	afterTags, err := c.DescribePortfolio(ctx, &sdk.DescribePortfolioInput{Id: port})
	scOK(t, err)
	if len(afterTags.Tags) != 1 || *afterTags.Tags[0].Key != "env" || *afterTags.Tags[0].Value != "test" {
		t.Fatalf("portfolio tags after native update and tagging API: %+v", afterTags.Tags)
	}

	afterProductTags, err := c.DescribeProductAsAdmin(ctx, &sdk.DescribeProductAsAdminInput{Id: id})
	scOK(t, err)
	if len(afterProductTags.Tags) != 0 {
		t.Fatalf("product tags were not removed: %+v", afterProductTags.Tags)
	}

	_, err = c.ProvisionProduct(ctx, &sdk.ProvisionProductInput{
		ProductId: id, ProvisioningArtifactId: pa,
		ProvisionedProductName: new("unsupported"),
	})
	scError(t, err, "UnknownOperationException")
	_, err = c.DescribePortfolio(ctx, &sdk.DescribePortfolioInput{Id: port, AcceptLanguage: new("jp")})
	scError(t, err, "InvalidParametersException")
	_, err = c.DisassociateProductFromPortfolio(
		ctx,
		&sdk.DisassociateProductFromPortfolioInput{PortfolioId: port, ProductId: id},
	)
	scOK(t, err)
	_, err = c.DeleteProduct(ctx, &sdk.DeleteProductInput{Id: id})
	scOK(t, err)
	principalARN := new("arn:aws:iam::000000000000:role/principal-only")
	_, err = c.AssociatePrincipalWithPortfolio(ctx, &sdk.AssociatePrincipalWithPortfolioInput{
		PortfolioId:  port,
		PrincipalARN: principalARN, PrincipalType: types.PrincipalTypeIam,
	})
	scOK(t, err)
	_, err = c.DeletePortfolio(ctx, &sdk.DeletePortfolioInput{Id: port})
	scError(t, err, "ResourceInUseException")
	_, err = c.DisassociatePrincipalFromPortfolio(ctx, &sdk.DisassociatePrincipalFromPortfolioInput{
		PortfolioId:  port,
		PrincipalARN: principalARN, PrincipalType: types.PrincipalTypeIam,
	})
	scOK(t, err)
	_, err = c.DeletePortfolio(ctx, &sdk.DeletePortfolioInput{Id: port})
	scOK(t, err)
	_, err = c.DescribeProduct(ctx, &sdk.DescribeProductInput{Id: id})
	scError(t, err, "ResourceNotFoundException")
	_, err = c.DescribePortfolio(ctx, &sdk.DescribePortfolioInput{Id: port})
	scError(t, err, "ResourceNotFoundException")
}

func TestServiceCatalogPaginationAndIsolation(t *testing.T) {
	ctx := context.Background()
	srv := server(t)
	c := serviceCatalogClient(srv)
	for i := range 3 {
		_, err := c.CreatePortfolio(
			ctx,
			&sdk.CreatePortfolioInput{DisplayName: new("page"), ProviderName: new("local")},
		)
		scOK(t, err)
		scCreateProduct(t, c, []string{"one", "two", "three"}[i])
	}

	var token *string
	var last string
	total := 0
	for {
		page, err := c.ListPortfolios(ctx, &sdk.ListPortfoliosInput{PageSize: 1, PageToken: token})
		scOK(t, err)
		if len(page.PortfolioDetails) != 1 || *page.PortfolioDetails[0].Id <= last {
			t.Fatalf("page: %+v", page)
		}

		last = *page.PortfolioDetails[0].Id
		total++
		token = page.NextPageToken
		if token == nil {
			break
		}
	}

	if total != 3 {
		t.Fatalf("got %d portfolios", total)
	}

	token = nil
	total = 0
	for {
		page, err := c.SearchProductsAsAdmin(
			ctx,
			&sdk.SearchProductsAsAdminInput{PageSize: 1, PageToken: token},
		)
		scOK(t, err)
		total += len(page.ProductViewDetails)
		token = page.NextPageToken
		if token == nil {
			break
		}
	}

	if total != 3 {
		t.Fatalf("got %d products", total)
	}

	page, err := c.SearchProducts(ctx, &sdk.SearchProductsInput{PageSize: 1})
	scOK(t, err)
	if page.NextPageToken == nil {
		t.Fatal("missing search page")
	}

	page, err = c.SearchProducts(ctx, &sdk.SearchProductsInput{PageToken: page.NextPageToken})
	scOK(t, err)
	if len(page.ProductViewSummaries) != 2 {
		t.Fatalf("page: %+v", page)
	}

	_, err = c.ListPortfolios(ctx, &sdk.ListPortfoliosInput{PageToken: new("bad!")})
	scError(t, err, "InvalidParametersException")
	for _, options := range []sdk.Options{
		{Region: "us-west-2", BaseEndpoint: new(srv.URL), Credentials: creds("test")},
		{Region: "us-east-1", BaseEndpoint: new(srv.URL), Credentials: creds("111111111111")},
	} {
		other := sdk.New(options)
		list, e := other.ListPortfolios(ctx, &sdk.ListPortfoliosInput{})
		scOK(t, e)
		if len(list.PortfolioDetails) != 0 {
			t.Fatal("scope leaked")
		}
	}

	// The model allows all these product types; records do not claim to execute any of them.
	for _, typ := range []types.ProductType{
		types.ProductTypeMarketplace, types.ProductTypeTerraformOpenSource,
		types.ProductTypeTerraformCloud, types.ProductTypeExternal,
	} {
		_, err = c.CreateProduct(ctx, &sdk.CreateProductInput{
			Name: new(string(typ)), Owner: new("local"), ProductType: typ,
			ProvisioningArtifactParameters: &types.ProvisioningArtifactProperties{Info: map[string]string{
				"LoadTemplateFromURL": "https://example.invalid/record",
			}},
		})
		scOK(t, err)
	}
}

func TestServiceCatalogSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srv, stop := persistent(t, dir, &fakeRuntime{})
	c := serviceCatalogClient(srv)
	p, err := c.CreatePortfolio(ctx, &sdk.CreatePortfolioInput{
		DisplayName: new("persist"), ProviderName: new("local"),
		Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
	})
	scOK(t, err)
	prod := scCreateProduct(t, c, "persisted")
	id := prod.ProductViewDetail.ProductViewSummary.ProductId
	port := p.PortfolioDetail.Id
	_, err = c.AssociateProductWithPortfolio(
		ctx,
		&sdk.AssociateProductWithPortfolioInput{PortfolioId: port, ProductId: id},
	)
	scOK(t, err)
	_, err = c.AssociatePrincipalWithPortfolio(ctx, &sdk.AssociatePrincipalWithPortfolioInput{
		PortfolioId:  port,
		PrincipalARN: new("arn:aws:iam::000000000000:role/persist"), PrincipalType: types.PrincipalTypeIam,
	})
	scOK(t, err)
	con, err := c.CreateConstraint(ctx, &sdk.CreateConstraintInput{
		PortfolioId: port, ProductId: id, Type: new("LAUNCH"),
		Parameters: new(`{"LocalRoleName":"persist"}`),
	})
	scOK(t, err)
	stop()
	srv, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = serviceCatalogClient(srv)
	got, err := c.DescribePortfolio(ctx, &sdk.DescribePortfolioInput{Id: port})
	scOK(t, err)
	if len(got.Tags) != 1 || *got.Tags[0].Value != "core" {
		t.Fatalf("tags: %+v", got)
	}

	product, err := c.DescribeProductAsAdmin(ctx, &sdk.DescribeProductAsAdminInput{Id: id})
	scOK(t, err)
	if len(product.ProvisioningArtifactSummaries) != 1 {
		t.Fatalf("product: %+v", product)
	}

	artifact, err := c.DescribeProvisioningArtifact(ctx, &sdk.DescribeProvisioningArtifactInput{
		ProductId:              id,
		ProvisioningArtifactId: prod.ProvisioningArtifactDetail.Id,
	})
	scOK(t, err)
	if artifact.Info["TemplateUrl"] != "https://example.invalid/never-fetched.json" {
		t.Fatalf("artifact: %+v", artifact)
	}

	principals, err := c.ListPrincipalsForPortfolio(ctx, &sdk.ListPrincipalsForPortfolioInput{PortfolioId: port})
	scOK(t, err)
	if len(principals.Principals) != 1 {
		t.Fatalf("principals: %+v", principals)
	}

	_, err = c.DescribeConstraint(ctx, &sdk.DescribeConstraintInput{Id: con.ConstraintDetail.ConstraintId})
	scOK(t, err)
	_, err = c.DeleteProduct(ctx, &sdk.DeleteProductInput{Id: id})
	scError(t, err, "ResourceInUseException")
}

func TestServiceCatalogCloudFormation(t *testing.T) {
	ctx := context.Background()
	srv := server(t)
	cf := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(srv.URL), Credentials: creds("test")})
	template := `{
  "Resources": {
    "Portfolio": {
      "Type": "AWS::ServiceCatalog::Portfolio",
      "Properties": {
        "DisplayName": "stack-catalog",
        "ProviderName": "local",
        "Tags": [
          {
            "Key": "team",
            "Value": "core"
          }
        ]
      }
    },
    "Product": {
      "Type": "AWS::ServiceCatalog::CloudFormationProduct",
      "Properties": {
        "Name": "stack-product",
        "Owner": "local",
        "ProvisioningArtifactParameters": [
          {
            "Name": "v1",
            "Info": {
              "LoadTemplateFromURL": "https://example.invalid/template"
            }
          }
        ]
      }
    },
    "Association": {
      "Type": "AWS::ServiceCatalog::PortfolioProductAssociation",
      "Properties": {
        "PortfolioId": {
          "Ref": "Portfolio"
        },
        "ProductId": {
          "Ref": "Product"
        }
      }
    },
    "Principal": {
      "Type": "AWS::ServiceCatalog::PortfolioPrincipalAssociation",
      "Properties": {
        "PortfolioId": {
          "Ref": "Portfolio"
        },
        "PrincipalARN": "arn:aws:iam::000000000000:role/stack-user",
        "PrincipalType": "IAM"
      }
    },
    "Constraint": {
      "Type": "AWS::ServiceCatalog::LaunchRoleConstraint",
      "DependsOn": "Association",
      "Properties": {
        "PortfolioId": {
          "Ref": "Portfolio"
        },
        "ProductId": {
          "Ref": "Product"
        },
        "LocalRoleName": "launch"
      }
    }
  },
  "Outputs": {
    "PortfolioId": {
      "Value": {
        "Fn::GetAtt": [
          "Portfolio",
          "Id"
        ]
      }
    },
    "ProductId": {
      "Value": {
        "Ref": "Product"
      }
    },
    "ConstraintId": {
      "Value": {
        "Fn::GetAtt": [
          "Constraint",
          "Id"
        ]
      }
    }
  }
}`
	_, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("servicecatalog-stack"), TemplateBody: new(template)},
	)
	scOK(t, err)
	err = cfn.NewStackCreateCompleteWaiter(cf, func(o *cfn.StackCreateCompleteWaiterOptions) {
		o.MinDelay = 10 * time.Millisecond
		o.MaxDelay = 50 * time.Millisecond
	}).Wait(ctx, &cfn.DescribeStacksInput{StackName: new("servicecatalog-stack")}, 30*time.Second)
	scOK(t, err)
	stacks, err := cf.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("servicecatalog-stack")})
	scOK(t, err)
	if string(stacks.Stacks[0].StackStatus) != "CREATE_COMPLETE" {
		t.Fatalf("stack: %+v", stacks.Stacks[0])
	}

	c := serviceCatalogClient(srv)
	products, err := c.SearchProductsAsAdmin(ctx, &sdk.SearchProductsAsAdminInput{})
	scOK(t, err)
	if len(products.ProductViewDetails) != 1 {
		t.Fatalf("stack product: %+v", products)
	}

	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("servicecatalog-stack")})
	scOK(t, err)
	err = cfn.NewStackDeleteCompleteWaiter(cf, func(o *cfn.StackDeleteCompleteWaiterOptions) {
		o.MinDelay = 10 * time.Millisecond
		o.MaxDelay = 50 * time.Millisecond
	}).Wait(ctx, &cfn.DescribeStacksInput{StackName: new("servicecatalog-stack")}, 30*time.Second)
	scOK(t, err)
	products, err = c.SearchProductsAsAdmin(ctx, &sdk.SearchProductsAsAdminInput{})
	scOK(t, err)
	if len(products.ProductViewDetails) != 0 {
		t.Fatal("stack product not deleted")
	}
}
