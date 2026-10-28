package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

const codePipelineRole = "arn:aws:iam::123456789012:role/pipeline"

func codePipelineClient(s *httptest.Server, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{}},
	)
}

func codePipelineAction(
	name string,
	category types.ActionCategory,
	provider, input, output string,
) types.ActionDeclaration {
	a := types.ActionDeclaration{
		Name: new(name),
		ActionTypeId: &types.ActionTypeId{
			Category: category, Owner: types.ActionOwnerAws, Provider: new(provider), Version: new("1"),
		},
		Configuration: map[string]string{"Key": "value"},
	}
	if input != "" {
		a.InputArtifacts = []types.InputArtifact{{Name: new(input)}}
	}

	if output != "" {
		a.OutputArtifacts = []types.OutputArtifact{{Name: new(output)}}
	}

	return a
}

func codePipelineDeclaration(name string) *types.PipelineDeclaration {
	return &types.PipelineDeclaration{
		Name:          new(name),
		RoleArn:       new(codePipelineRole),
		ArtifactStore: &types.ArtifactStore{Type: types.ArtifactStoreTypeS3, Location: new("artifacts")},
		Stages: []types.StageDeclaration{
			{Name: new("Source"), Actions: []types.ActionDeclaration{
				codePipelineAction("Fetch", types.ActionCategorySource, "S3", "", "src"),
			}},
			{Name: new("Build"), Actions: []types.ActionDeclaration{
				codePipelineAction("Compile", types.ActionCategoryBuild, "CodeBuild", "src", "bin"),
			}},
		},
	}
}

func TestCodePipelineSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")

	created, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{
		Pipeline: codePipelineDeclaration("app"),
		Tags:     []types.Tag{{Key: new("env"), Value: new("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToInt32(created.Pipeline.Version) != 1 || created.Pipeline.PipelineType != types.PipelineTypeV1 ||
		created.Pipeline.ExecutionMode != types.ExecutionModeSuperseded ||
		aws.ToInt32(created.Pipeline.Stages[0].Actions[0].RunOrder) != 1 || len(created.Tags) != 1 {
		t.Fatalf("created = %+v", created)
	}

	if _, err = c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: codePipelineDeclaration("app")}); err == nil {
		t.Fatal("expected the duplicate pipeline to fail")
	} else if inUse := (*types.PipelineNameInUseException)(nil); !errors.As(err, &inUse) {
		t.Fatalf("error = %T %v", err, err)
	}

	got, err := c.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("app")})
	if err != nil {
		t.Fatal(err)
	}

	arn := aws.ToString(got.Metadata.PipelineArn)
	if !strings.HasPrefix(arn, "arn:aws:codepipeline:us-east-1:") || !strings.HasSuffix(arn, ":app") ||
		got.Metadata.Created == nil || got.Metadata.Updated == nil ||
		aws.ToString(got.Pipeline.Stages[1].Actions[0].Name) != "Compile" ||
		got.Pipeline.Stages[1].Actions[0].Configuration["Key"] != "value" {
		t.Fatalf("got = %+v", got)
	}

	state, err := c.GetPipelineState(ctx, &sdk.GetPipelineStateInput{Name: new("app")})
	if err != nil || len(state.StageStates) != 2 || aws.ToString(state.StageStates[0].StageName) != "Source" ||
		aws.ToString(state.StageStates[0].ActionStates[0].ActionName) != "Fetch" {
		t.Fatal(state, err)
	}

	// A rejected update changes nothing; a valid one adds a version.
	bad := codePipelineDeclaration("app")
	bad.Stages[1].Actions[0].InputArtifacts = []types.InputArtifact{{Name: new("missing")}}
	if _, err = c.UpdatePipeline(ctx, &sdk.UpdatePipelineInput{Pipeline: bad}); err == nil {
		t.Fatal("expected the unwired artifact to fail")
	} else if invalid := (*types.InvalidActionDeclarationException)(nil); !errors.As(err, &invalid) {
		t.Fatalf("error = %T %v", err, err)
	}

	next := codePipelineDeclaration("app")
	next.ExecutionMode = types.ExecutionModeQueued
	next.PipelineType = types.PipelineTypeV2
	next.Variables = []types.PipelineVariableDeclaration{{Name: new("stage"), DefaultValue: new("dev")}}
	updated, err := c.UpdatePipeline(ctx, &sdk.UpdatePipelineInput{Pipeline: next})
	if err != nil || aws.ToInt32(updated.Pipeline.Version) != 2 {
		t.Fatal(updated, err)
	}

	if _, err = c.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("app"), Version: new(int32(1))}); err == nil {
		t.Fatal("expected the older version to be gone")
	} else if gone := (*types.PipelineVersionNotFoundException)(nil); !errors.As(err, &gone) {
		t.Fatalf("error = %T %v", err, err)
	}

	listed, err := c.ListPipelines(ctx, &sdk.ListPipelinesInput{})
	if err != nil || len(listed.Pipelines) != 1 || listed.Pipelines[0].PipelineType != types.PipelineTypeV2 ||
		listed.Pipelines[0].ExecutionMode != types.ExecutionModeQueued || aws.ToInt32(listed.Pipelines[0].Version) != 2 {
		t.Fatal(listed, err)
	}

	// Tags, directly and through the Resource Groups Tagging API.
	if _, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: new(arn), Tags: []types.Tag{{Key: new("team"), Value: new("platform")}},
	}); err != nil {
		t.Fatal(err)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{arn}, Tags: map[string]string{"rgta": "works"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	if _, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: new(arn), TagKeys: []string{"env"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(arn)})
	if err != nil || len(tags.Tags) != 2 || aws.ToString(tags.Tags[0].Key) != "rgta" ||
		aws.ToString(tags.Tags[1].Key) != "team" {
		t.Fatal(tags, err)
	}

	// Another Region has no such pipeline.
	west := codePipelineClient(s, "us-west-2")
	if _, err = west.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("app")}); err == nil {
		t.Fatal("expected the pipeline to be Regional")
	} else if missing := (*types.PipelineNotFoundException)(nil); !errors.As(err, &missing) {
		t.Fatalf("error = %T %v", err, err)
	}

	if _, err = c.DeletePipeline(ctx, &sdk.DeletePipelineInput{Name: new("app")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("app")}); err == nil {
		t.Fatal("expected the deleted pipeline to be gone")
	}
}

func TestCodePipelineListPages(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	for _, name := range []string{"c", "a", "b"} {
		if _, err := c.CreatePipeline(
			ctx,
			&sdk.CreatePipelineInput{Pipeline: codePipelineDeclaration(name)},
		); err != nil {
			t.Fatal(err)
		}
	}

	var names []string
	pages := sdk.NewListPipelinesPaginator(c, &sdk.ListPipelinesInput{MaxResults: new(int32(2))})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}

		for _, p := range page.Pipelines {
			names = append(names, aws.ToString(p.Name))
		}
	}

	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("names = %v", names)
	}

	_, err := c.ListPipelines(ctx, &sdk.ListPipelinesInput{NextToken: new("!")})
	if invalid := (*types.InvalidNextTokenException)(nil); !errors.As(err, &invalid) {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestCodePipelineRejectsUnsupportedMembers(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := codePipelineDeclaration("triggered")
	p.Triggers = []types.PipelineTriggerDeclaration{
		{ProviderType: types.PipelineTriggerProviderTypeCodeStarSourceConnection},
	}
	if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err == nil {
		t.Fatal("expected triggers to be rejected")
	}

	if _, err := c.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("triggered")}); err == nil {
		t.Fatal("a rejected pipeline was stored")
	}
}
