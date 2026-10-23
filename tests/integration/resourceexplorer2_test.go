package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	sdk "github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func explorerClient(s *httptest.Server, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{}},
	)
}

func TestResourceExplorerInventory(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := explorerClient(s, "us-east-1")
	_, err := c.Search(ctx, &sdk.SearchInput{QueryString: new("")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.CreateView(ctx, &sdk.CreateViewInput{ViewName: new("missing-index")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	index, err := c.CreateIndex(ctx, &sdk.CreateIndexInput{Tags: map[string]string{"team": "core"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateIndex(ctx, &sdk.CreateIndexInput{})
	wantAccessAnalyzerError(t, err, "ConflictException")
	got, err := c.GetIndex(ctx, &sdk.GetIndexInput{})
	if err != nil || got.State != types.IndexStateActive || got.Tags["team"] != "core" {
		t.Fatalf("index: %+v %v", got, err)
	}

	_, err = c.UpdateIndexType(ctx, &sdk.UpdateIndexTypeInput{Arn: index.Arn, Type: types.IndexTypeAggregator})
	if err != nil {
		t.Fatal(err)
	}

	west := explorerClient(s, "us-west-2")
	westIndex, err := west.CreateIndex(ctx, &sdk.CreateIndexInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = west.UpdateIndexType(ctx, &sdk.UpdateIndexTypeInput{Arn: westIndex.Arn, Type: types.IndexTypeAggregator})
	wantAccessAnalyzerError(t, err, "ConflictException")
	indexes, err := c.ListIndexes(
		ctx,
		&sdk.ListIndexesInput{Type: types.IndexTypeAggregator, Regions: []string{"us-east-1"}},
	)
	if err != nil || len(indexes.Indexes) != 1 {
		t.Fatalf("indexes: %+v %v", indexes, err)
	}

	page, err := c.ListIndexes(ctx, &sdk.ListIndexesInput{MaxResults: new(int32(1))})
	if err != nil || page.NextToken == nil {
		t.Fatalf("index page: %+v %v", page, err)
	}

	next, err := c.ListIndexes(ctx, &sdk.ListIndexesInput{NextToken: page.NextToken})
	if err != nil || len(next.Indexes) != 1 {
		t.Fatalf("index next: %+v %v", next, err)
	}

	_, err = c.UpdateIndexType(ctx, &sdk.UpdateIndexTypeInput{Arn: index.Arn, Type: types.IndexTypeLocal})
	if err != nil {
		t.Fatal(err)
	}

	v, err := c.CreateView(
		ctx,
		&sdk.CreateViewInput{
			ViewName:           new("inventory"),
			IncludedProperties: []types.IncludedProperty{{Name: new("tags")}},
			Tags:               map[string]string{"team": "core"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	view := v.View.ViewArn
	_, err = c.CreateView(ctx, &sdk.CreateViewInput{ViewName: new("inventory")})
	wantAccessAnalyzerError(t, err, "ConflictException")
	_, err = c.AssociateDefaultView(ctx, &sdk.AssociateDefaultViewInput{ViewArn: view})
	if err != nil {
		t.Fatal(err)
	}

	def, err := c.GetDefaultView(ctx, &sdk.GetDefaultViewInput{})
	if err != nil || *def.ViewArn != *view {
		t.Fatalf("default: %+v %v", def, err)
	}

	b := s3Client(s, "test", "us-east-1")
	for _, name := range []string{"explorer-alpha", "explorer-beta"} {
		if _, callErr := b.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(name)}); callErr != nil {
			t.Fatal(callErr)
		}
	}

	q := sqsClient(s, "test")
	queue, err := q.CreateQueue(
		ctx,
		&sqs.CreateQueueInput{QueueName: new("explorer-queue"), Tags: map[string]string{"team": "core"}},
	)
	if err != nil || queue.QueueUrl == nil {
		t.Fatalf("queue: %v", err)
	}

	if _, callErr := b.PutBucketTagging(
		ctx,
		&s3.PutBucketTaggingInput{
			Bucket:  new("explorer-alpha"),
			Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: new("team"), Value: new("core")}}},
		},
	); callErr != nil {
		t.Fatal(callErr)
	}

	// Keywords are joined with OR, filters with AND, and a filter's comma-separated values with OR.
	queries := map[string]int{
		"EXPLORER-ALPHA":                           1,
		"explorer-alpha explorer-beta":             2,
		"explorer-alpha explorer-beta service:sqs": 0,
		"service:s3":                               2,
		"resourcetype:s3:bucket":                   2,
		"resourcetype:s3:buck*":                    2,
		"service:s3 region:us-east-1":              2,
		"service:s3 region:us-west-2,us-east-1":    2,
		"service:s3 tag:team=core":                 1,
		"service:s3 tag.key:team":                  1,
		"service:s3,sqs tag.value:core":            2,
		"service:s3 tag:all":                       1,
		"service:s3 tag:none":                      1,
		"service:s3 -tag.key:team":                 1,
		"service:s3 -explorer-alpha":               1,
		"service:sqs tag:team=core":                1,
		"region:us-west-2":                         0,
	}

	for query, want := range queries {
		out, searchErr := c.Search(ctx, &sdk.SearchInput{QueryString: new(query)})
		if searchErr != nil || len(out.Resources) != want || int(*out.Count.TotalResources) != want {
			t.Fatalf("query %q: %+v %v", query, out, searchErr)
		}

		for _, r := range out.Resources {
			if r.Arn == nil || r.LastReportedAt == nil || r.ResourceType == nil || r.Service == nil ||
				r.OwningAccountId == nil ||
				len(r.Properties) != 1 {
				t.Fatalf("resource mapping: %+v", r)
			}
		}
	}

	for _, query := range []string{
		"foo:bar", "a OR b", "a AND b", "(foo)", "service:", "tag:", "fo*o", "tag:team", `"quoted phrase"`,
		"tag-key:team", "resourcetype:s3", "a|b", "a=b", "service:s3,", `explorer\-alpha`,
	} {
		_, queryErr := c.Search(ctx, &sdk.SearchInput{QueryString: new(query)})
		wantAccessAnalyzerError(t, queryErr, "ValidationException")
	}

	// Global resources report the Region "global", which region:global selects.
	_, err = iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 new("explorer-role"),
		AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	global, err := c.Search(ctx, &sdk.SearchInput{QueryString: new("region:global explorer-role")})
	if err != nil || len(global.Resources) != 1 || *global.Resources[0].Region != "global" {
		t.Fatalf("global search: %+v %v", global, err)
	}

	first, err := c.Search(ctx, &sdk.SearchInput{QueryString: new("service:s3"), MaxResults: new(int32(1))})
	if err != nil || first.NextToken == nil || *first.Count.TotalResources != 2 {
		t.Fatalf("search page: %+v %v", first, err)
	}

	second, err := c.Search(ctx, &sdk.SearchInput{QueryString: new("service:s3"), NextToken: first.NextToken})
	if err != nil || len(second.Resources) != 1 || *first.Resources[0].Arn >= *second.Resources[0].Arn {
		t.Fatalf("search next: %+v %v", second, err)
	}

	_, err = c.Search(ctx, &sdk.SearchInput{QueryString: new(""), NextToken: new("!")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.UpdateView(
		ctx,
		&sdk.UpdateViewInput{
			ViewArn:            view,
			Filters:            &types.SearchFilter{FilterString: new("service:s3")},
			IncludedProperties: []types.IncludedProperty{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	filtered, err := c.Search(ctx, &sdk.SearchInput{QueryString: new("explorer")})
	if err != nil || len(filtered.Resources) != 2 || len(filtered.Resources[0].Properties) != 0 {
		t.Fatalf("view intersection: %+v %v", filtered, err)
	}

	// Tag filters need a view that includes tags.
	_, err = c.Search(ctx, &sdk.SearchInput{QueryString: new("tag:team=core")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	listed, err := c.ListResources(
		ctx,
		&sdk.ListResourcesInput{Filters: &types.SearchFilter{FilterString: new("explorer-beta")}},
	)
	if err != nil || len(listed.Resources) != 1 {
		t.Fatalf("ListResources: %+v %v", listed, err)
	}

	supported, err := c.ListSupportedResourceTypes(ctx, &sdk.ListSupportedResourceTypesInput{})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	previous := ""
	for _, r := range supported.ResourceTypes {
		if *r.ResourceType < previous {
			t.Fatal("unordered types")
		}

		previous = *r.ResourceType
		if *r.ResourceType == "s3:bucket" {
			found = true
		}
	}

	if !found {
		t.Fatal("missing populated inventory type")
	}

	for _, a := range []*string{index.Arn, view} {
		_, tagErr := c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: a, Tags: map[string]string{"env": "dev"}})
		if tagErr != nil {
			t.Fatal(tagErr)
		}

		tags, tagErr := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: a})
		if tagErr != nil || tags.Tags["env"] != "dev" {
			t.Fatalf("tags: %+v %v", tags, tagErr)
		}

		_, tagErr = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: a, TagKeys: []string{"env"}})
		if tagErr != nil {
			t.Fatal(tagErr)
		}
	}

	tagClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*view, *index.Arn}, Tags: map[string]string{"shared": "yes"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("inventory tagger: %+v %v", tagged, err)
	}

	gotView, err := c.GetView(ctx, &sdk.GetViewInput{ViewArn: view})
	if err != nil || gotView.Tags["shared"] != "yes" {
		t.Fatalf("GetView: %+v %v", gotView, err)
	}

	views, err := c.ListViews(ctx, &sdk.ListViewsInput{})
	if err != nil || len(views.Views) != 1 {
		t.Fatalf("views: %+v %v", views, err)
	}

	batch, err := c.BatchGetView(ctx, &sdk.BatchGetViewInput{ViewArns: []string{*view, *view + "missing"}})
	if err != nil || len(batch.Views) != 1 || len(batch.Errors) != 1 {
		t.Fatalf("batch: %+v %v", batch, err)
	}

	_, err = c.DisassociateDefaultView(ctx, &sdk.DisassociateDefaultViewInput{})
	if err != nil {
		t.Fatal(err)
	}

	emptyDefault, err := c.GetDefaultView(ctx, &sdk.GetDefaultViewInput{})
	if err != nil || emptyDefault.ViewArn != nil {
		t.Fatalf("empty default: %+v %v", emptyDefault, err)
	}

	_, err = c.Search(ctx, &sdk.SearchInput{QueryString: new("")})
	wantAccessAnalyzerError(t, err, "UnauthorizedException")
	_, err = c.DeleteView(ctx, &sdk.DeleteViewInput{ViewArn: view})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetView(ctx, &sdk.GetViewInput{ViewArn: view})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	deleted, err := c.DeleteIndex(ctx, &sdk.DeleteIndexInput{Arn: index.Arn})
	if err != nil || deleted.State != types.IndexStateDeleted {
		t.Fatalf("delete: %+v %v", deleted, err)
	}

	_, err = c.GetIndex(ctx, &sdk.GetIndexInput{})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func TestResourceExplorerPersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := explorerClient(s, "us-east-1")
	index, err := c.CreateIndex(ctx, &sdk.CreateIndexInput{Tags: map[string]string{"persist": "yes"}})
	if err != nil {
		t.Fatal(err)
	}

	v, err := c.CreateView(
		ctx,
		&sdk.CreateViewInput{
			ViewName: new("persistent"),
			Filters:  &types.SearchFilter{FilterString: new("service:resource-explorer-2")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, callErr := c.AssociateDefaultView(
		ctx,
		&sdk.AssociateDefaultViewInput{ViewArn: v.View.ViewArn},
	); callErr != nil {
		t.Fatal(callErr)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = explorerClient(s, "us-east-1")
	got, err := c.GetIndex(ctx, &sdk.GetIndexInput{})
	if err != nil || *got.Arn != *index.Arn || got.Tags["persist"] != "yes" {
		t.Fatalf("persisted index: %+v %v", got, err)
	}

	out, err := c.Search(ctx, &sdk.SearchInput{QueryString: new("")})
	if err != nil || len(out.Resources) != 2 || *out.ViewArn != *v.View.ViewArn {
		t.Fatalf("persisted search: %+v %v", out, err)
	}
}

func TestResourceExplorerCloudFormation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := `{
  "Resources": {
    "Index": {
      "Type": "AWS::ResourceExplorer2::Index",
      "Properties": {
        "Type": "LOCAL",
        "Tags": [
          {
            "Key": "team",
            "Value": "core"
          }
        ]
      }
    },
    "View": {
      "Type": "AWS::ResourceExplorer2::View",
      "DependsOn": "Index",
      "Properties": {
        "ViewName": "stack-inventory",
        "IncludedProperties": [
          {
            "Name": "tags"
          }
        ],
        "Filters": {
          "FilterString": "service:s3"
        },
        "Tags": [
          {
            "Key": "team",
            "Value": "core"
          }
        ]
      }
    },
    "Default": {
      "Type": "AWS::ResourceExplorer2::DefaultViewAssociation",
      "Properties": {
        "ViewArn": {
          "Ref": "View"
        }
      }
    }
  },
  "Outputs": {
    "IndexArn": {
      "Value": {
        "Fn::GetAtt": [
          "Index",
          "Arn"
        ]
      }
    },
    "ViewArn": {
      "Value": {
        "Fn::GetAtt": [
          "View",
          "ViewArn"
        ]
      }
    }
  }
}`
	if _, callErr := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("resource-explorer"), TemplateBody: new(template)},
	); callErr != nil {
		t.Fatal(callErr)
	}

	stack := wantStatus(ctx, t, c, "resource-explorer", cfntypes.StackStatusCreateComplete)

	ec := explorerClient(s, "us-east-1")
	index, err := ec.GetIndex(ctx, &sdk.GetIndexInput{})
	if err != nil || index.Tags["team"] != "core" {
		t.Fatalf("CFN index: %+v %v", index, err)
	}

	def, err := ec.GetDefaultView(ctx, &sdk.GetDefaultViewInput{})
	if err != nil {
		t.Fatal(err)
	}

	view, err := ec.GetView(ctx, &sdk.GetViewInput{ViewArn: def.ViewArn})
	if err != nil || *view.View.ViewName != "stack-inventory" {
		t.Fatalf("CFN view: %+v %v", view, err)
	}

	updated := strings.ReplaceAll(strings.ReplaceAll(template, "core", "platform"), "LOCAL", "AGGREGATOR")
	if _, callErr := c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("resource-explorer"), TemplateBody: new(updated)},
	); callErr != nil {
		t.Fatal(callErr)
	}

	wantStatus(ctx, t, c, "resource-explorer", cfntypes.StackStatusUpdateComplete)

	index, err = ec.GetIndex(ctx, &sdk.GetIndexInput{})
	if err != nil || index.Type != types.IndexTypeAggregator || index.Tags["team"] != "platform" {
		t.Fatalf("CFN update: %+v %v", index, err)
	}

	if _, callErr := c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("resource-explorer")}); callErr != nil {
		t.Fatal(callErr)
	}

	wantStatus(ctx, t, c, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)

	_, err = ec.GetIndex(ctx, &sdk.GetIndexInput{})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = ec.GetView(ctx, &sdk.GetViewInput{ViewArn: def.ViewArn})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}
