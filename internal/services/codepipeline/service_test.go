package codepipeline_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/codepipeline"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

const roleARN = "arn:aws:iam::123456789012:role/pipeline"

func pipelineContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func action(name, category, in, out string) api.ActionDeclaration {
	a := api.ActionDeclaration{
		Name: new(name),
		ActionTypeId: &api.ActionTypeId{
			Category: new(category), Owner: new("AWS"), Provider: new("Provider"), Version: new("1"),
		},
	}
	if in != "" {
		a.InputArtifacts = []api.InputArtifact{{Name: new(in)}}
	}

	if out != "" {
		a.OutputArtifacts = []api.OutputArtifact{{Name: new(out)}}
	}

	return a
}

// declaration is a valid three-stage pipeline: a source, a build that consumes it and a deploy that consumes the build.
func declaration(name string) *api.PipelineDeclaration {
	return &api.PipelineDeclaration{
		Name:          new(name),
		RoleArn:       new(roleARN),
		ArtifactStore: &api.ArtifactStore{Type: new("S3"), Location: new("artifacts")},
		Stages: []api.StageDeclaration{
			{Name: new("Source"), Actions: []api.ActionDeclaration{action("Fetch", "Source", "", "src")}},
			{Name: new("Build"), Actions: []api.ActionDeclaration{action("Compile", "Build", "src", "bin")}},
			{Name: new("Deploy"), Actions: []api.ActionDeclaration{action("Release", "Deploy", "bin", "")}},
		},
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()

	if e, ok := errors.AsType[*awserror.Error](err); !ok || e.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestPipelineLifecycle(t *testing.T) {
	ctx := pipelineContext("123456789012", "us-east-1")
	s := codepipeline.New(memory.New(nil, nil))
	created, err := s.CreatePipeline(ctx, &api.CreatePipelineInput{
		Pipeline: declaration("app"),
		Tags:     []api.Tag{{Key: new("env"), Value: new("test")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	p := created.Pipeline
	if *p.Version != 1 || *p.PipelineType != "V1" || *p.ExecutionMode != "SUPERSEDED" ||
		*p.Stages[0].Actions[0].RunOrder != 1 || len(created.Tags) != 1 {
		t.Fatalf("created = %+v", created)
	}

	if _, err = s.CreatePipeline(ctx, &api.CreatePipelineInput{Pipeline: declaration("app")}); err == nil {
		t.Fatal("duplicate accepted")
	}

	wantCode(t, err, "PipelineNameInUseException")
	got, err := s.GetPipeline(ctx, &api.GetPipelineInput{Name: new("app"), Version: new(int64(1))})
	if err != nil || *got.Metadata.PipelineArn != "arn:aws:codepipeline:us-east-1:123456789012:app" ||
		*got.Metadata.Created != *got.Metadata.Updated || *got.Pipeline.Name != "app" {
		t.Fatal(got, err)
	}

	state, err := s.GetPipelineState(ctx, &api.GetPipelineStateInput{Name: new("app")})
	if err != nil || len(state.StageStates) != 3 || *state.StageStates[1].ActionStates[0].ActionName != "Compile" ||
		*state.PipelineVersion != 1 {
		t.Fatal(state, err)
	}

	update := declaration("app")
	update.Stages = update.Stages[:2]
	update.Stages[1].Actions[0].OutputArtifacts = nil
	updated, err := s.UpdatePipeline(ctx, &api.UpdatePipelineInput{Pipeline: update})
	if err != nil || *updated.Pipeline.Version != 2 {
		t.Fatal(updated, err)
	}

	got, err = s.GetPipeline(ctx, &api.GetPipelineInput{Name: new("app")})
	if err != nil || *got.Pipeline.Version != 2 || len(got.Pipeline.Stages) != 2 ||
		*got.Metadata.Created > *got.Metadata.Updated {
		t.Fatal(got, err)
	}

	if _, err = s.GetPipeline(ctx, &api.GetPipelineInput{Name: new("app"), Version: new(int64(1))}); err == nil {
		t.Fatal("an older version was served")
	}

	wantCode(t, err, "PipelineVersionNotFoundException")
	if _, err = s.DeletePipeline(ctx, &api.DeletePipelineInput{Name: new("app")}); err != nil {
		t.Fatal(err)
	}

	_, err = s.GetPipeline(ctx, &api.GetPipelineInput{Name: new("app")})
	wantCode(t, err, "PipelineNotFoundException")
	_, err = s.UpdatePipeline(ctx, &api.UpdatePipelineInput{Pipeline: declaration("app")})
	wantCode(t, err, "PipelineNotFoundException")
	_, err = s.GetPipelineState(ctx, &api.GetPipelineStateInput{Name: new("app")})
	wantCode(t, err, "PipelineNotFoundException")

	// Deleting a pipeline that does not exist succeeds: the operation models no not-found error.
	if _, err = s.DeletePipeline(ctx, &api.DeletePipelineInput{Name: new("app")}); err != nil {
		t.Fatal(err)
	}
}

func TestDeclarationRules(t *testing.T) {
	ctx := pipelineContext("123456789012", "us-east-1")
	s := codepipeline.New(memory.New(nil, nil))
	tests := []struct {
		modify func(p *api.PipelineDeclaration)
		name   string
		code   string
	}{
		{func(p *api.PipelineDeclaration) { p.Stages = p.Stages[:1] }, "one stage", "InvalidStructureException"},
		{func(p *api.PipelineDeclaration) { p.ArtifactStore = nil }, "no artifact store", "InvalidStructureException"},
		{func(p *api.PipelineDeclaration) {
			p.ArtifactStores = map[string]api.ArtifactStore{"us-east-1": *p.ArtifactStore}
		}, "both artifact stores", "InvalidStructureException"},
		{func(p *api.PipelineDeclaration) {
			p.ArtifactStores = map[string]api.ArtifactStore{"eu-west-1": *p.ArtifactStore}
			p.ArtifactStore = nil
		}, "artifact stores without the pipeline region", "InvalidStructureException"},
		{func(p *api.PipelineDeclaration) {
			p.PipelineType = new("V1")
			p.Variables = []api.PipelineVariableDeclaration{{Name: new("v")}}
		}, "variables on V1", "InvalidStructureException"},
		{func(p *api.PipelineDeclaration) {
			p.PipelineType = new("V1")
			p.ExecutionMode = new("QUEUED")
		}, "queued on V1", "InvalidStructureException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[0].Actions = append(p.Stages[0].Actions, action("Extra", "Build", "", ""))
		}, "first stage without a source", "InvalidStageDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[2].Actions = append(p.Stages[2].Actions, action("Late", "Source", "", "late"))
		}, "source in a later stage", "InvalidStageDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[2].Name = new("Build")
		}, "duplicate stage", "InvalidStageDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[2].Actions = nil
		}, "empty stage", "InvalidStageDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[2].Actions = append(p.Stages[2].Actions, action("Release", "Deploy", "bin", ""))
		}, "duplicate action", "InvalidActionDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[2].Actions[0].InputArtifacts = []api.InputArtifact{{Name: new("nothing")}}
		}, "unknown input artifact", "InvalidActionDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[1].Actions[0].InputArtifacts = []api.InputArtifact{{Name: new("bin")}}
		}, "input from a later stage", "InvalidActionDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[2].Actions = append(p.Stages[2].Actions, action("Peer", "Deploy", "", "peer"))
			p.Stages[2].Actions[0].InputArtifacts = []api.InputArtifact{{Name: new("peer")}}
		}, "input from the same run order", "InvalidActionDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[1].Actions[0].OutputArtifacts = []api.OutputArtifact{{Name: new("src")}}
		}, "duplicate output artifact", "InvalidActionDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[2].Actions[0].Region = new("eu-west-1")
		}, "action region without a store", "InvalidActionDeclarationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[2].Actions[0].ActionTypeId.Category = new("Cook")
		}, "bad category", "ValidationException"},
		{
			func(p *api.PipelineDeclaration) { p.ExecutionMode = new("FAST") },
			"bad execution mode",
			"ValidationException",
		},
		{func(p *api.PipelineDeclaration) { p.Name = new("a b") }, "bad name", "ValidationException"},
		{func(p *api.PipelineDeclaration) { p.RoleArn = new("role") }, "bad role", "ValidationException"},
		{func(p *api.PipelineDeclaration) {
			p.Stages[1].Actions[0].RunOrder = new(int64(1000))
		}, "bad run order", "ValidationException"},
		{func(p *api.PipelineDeclaration) { p.ArtifactStore.Location = new("a") }, "bad bucket", "ValidationException"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := declaration("rules")
			tt.modify(p)
			_, err := s.CreatePipeline(ctx, &api.CreatePipelineInput{Pipeline: p})
			wantCode(t, err, tt.code)
		})
	}

	// Parallel actions, same-stage hand-offs and cross-region artifact stores are valid.
	p := declaration("valid")
	p.PipelineType = nil
	p.ExecutionMode = new("PARALLEL")
	p.Variables = []api.PipelineVariableDeclaration{{Name: new("stage"), DefaultValue: new("dev")}}
	p.ArtifactStore = nil
	p.ArtifactStores = map[string]api.ArtifactStore{
		"us-east-1": {Type: new("S3"), Location: new("east-artifacts")},
		"eu-west-1": {
			Type: new("S3"), Location: new("west-artifacts"),
			EncryptionKey: &api.EncryptionKey{Id: new("alias/key"), Type: new("KMS")},
		},
	}
	p.Stages[2].Actions[0].Region = new("eu-west-1")
	p.Stages[2].Actions[0].RunOrder = new(int64(2))
	p.Stages[2].Actions = append(p.Stages[2].Actions, action("Prepare", "Build", "bin", "ready"))
	p.Stages[2].Actions[0].InputArtifacts = []api.InputArtifact{{Name: new("ready")}}
	created, err := s.CreatePipeline(ctx, &api.CreatePipelineInput{Pipeline: p})
	if err != nil || *created.Pipeline.PipelineType != "V2" {
		t.Fatal(created, err)
	}

	if _, err = s.CreatePipeline(ctx, &api.CreatePipelineInput{
		Pipeline: declaration(
			"tags",
		),
		Tags: []api.Tag{{Key: new("a"), Value: new("1")}, {Key: new("a"), Value: new("2")}},
	}); err == nil {
		t.Fatal("duplicate tag keys accepted")
	}
}

func TestRejectedUpdateKeepsPipeline(t *testing.T) {
	ctx := pipelineContext("123456789012", "us-east-1")
	s := codepipeline.New(memory.New(nil, nil))
	if _, err := s.CreatePipeline(ctx, &api.CreatePipelineInput{Pipeline: declaration("keep")}); err != nil {
		t.Fatal(err)
	}

	bad := declaration("keep")
	bad.Stages = bad.Stages[:1]
	if _, err := s.UpdatePipeline(ctx, &api.UpdatePipelineInput{Pipeline: bad}); err == nil {
		t.Fatal("invalid update accepted")
	}

	got, err := s.GetPipeline(ctx, &api.GetPipelineInput{Name: new("keep")})
	if err != nil || *got.Pipeline.Version != 1 || len(got.Pipeline.Stages) != 3 {
		t.Fatal(got, err)
	}
}

func TestListPipelinesPages(t *testing.T) {
	ctx := pipelineContext("123456789012", "us-east-1")
	s := codepipeline.New(memory.New(nil, nil))
	for _, name := range []string{"c", "a", "d", "b"} {
		if _, err := s.CreatePipeline(ctx, &api.CreatePipelineInput{Pipeline: declaration(name)}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.ListPipelines(ctx, &api.ListPipelinesInput{MaxResults: new(int64(2))})
	if err != nil || len(first.Pipelines) != 2 || *first.Pipelines[0].Name != "a" || first.NextToken == nil {
		t.Fatal(first, err)
	}

	// The cursor pipeline can be deleted before the next page is read.
	if _, err = s.DeletePipeline(ctx, &api.DeletePipelineInput{Name: new("b")}); err != nil {
		t.Fatal(err)
	}

	second, err := s.ListPipelines(ctx, &api.ListPipelinesInput{MaxResults: new(int64(2)), NextToken: first.NextToken})
	if err != nil || len(second.Pipelines) != 2 || *second.Pipelines[0].Name != "c" || second.NextToken != nil {
		t.Fatal(second, err)
	}

	if *second.Pipelines[0].PipelineType != "V1" || *second.Pipelines[0].ExecutionMode != "SUPERSEDED" {
		t.Fatal(second.Pipelines[0])
	}

	_, err = s.ListPipelines(ctx, &api.ListPipelinesInput{NextToken: new("!")})
	wantCode(t, err, "InvalidNextTokenException")
	_, err = s.ListPipelines(ctx, &api.ListPipelinesInput{MaxResults: new(int64(1001))})
	wantCode(t, err, "ValidationException")
}

func TestTagsAndScopes(t *testing.T) {
	ctx := pipelineContext("123456789012", "us-east-1")
	s := codepipeline.New(memory.New(nil, nil))
	if _, err := s.CreatePipeline(ctx, &api.CreatePipelineInput{Pipeline: declaration("tagged")}); err != nil {
		t.Fatal(err)
	}

	arn := new("arn:aws:codepipeline:us-east-1:123456789012:tagged")
	if _, err := s.TagResource(ctx, &api.TagResourceInput{
		ResourceArn: arn, Tags: []api.Tag{{Key: new("env"), Value: new("dev")}, {Key: new("team"), Value: new("a")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UntagResource(
		ctx,
		&api.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := s.ListTagsForResource(ctx, &api.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "env" {
		t.Fatal(tags, err)
	}

	many := make([]api.Tag, 51)
	for i := range many {
		many[i] = api.Tag{Key: new(fmt.Sprintf("k%d", i)), Value: new("v")}
	}

	_, err = s.TagResource(ctx, &api.TagResourceInput{ResourceArn: arn, Tags: many})
	wantCode(t, err, "TooManyTagsException")
	_, err = s.TagResource(ctx, &api.TagResourceInput{
		ResourceArn: arn, Tags: []api.Tag{{Key: new("aws:reserved"), Value: new("v")}},
	})
	wantCode(t, err, "InvalidTagsException")
	_, err = s.ListTagsForResource(ctx, &api.ListTagsForResourceInput{ResourceArn: new("tagged")})
	wantCode(t, err, "InvalidArnException")
	_, err = s.ListTagsForResource(ctx, &api.ListTagsForResourceInput{
		ResourceArn: new("arn:aws:codepipeline:us-east-1:123456789012:missing"),
	})
	wantCode(t, err, "ResourceNotFoundException")

	// Another account or Region sees neither the pipeline nor its tags.
	for _, other := range []context.Context{
		pipelineContext("210987654321", "us-east-1"), pipelineContext("123456789012", "us-west-2"),
	} {
		_, err = s.GetPipeline(other, &api.GetPipelineInput{Name: new("tagged")})
		wantCode(t, err, "PipelineNotFoundException")
		_, err = s.ListTagsForResource(other, &api.ListTagsForResourceInput{ResourceArn: arn})
		wantCode(t, err, "ResourceNotFoundException")
		listed, listErr := s.ListPipelines(other, &api.ListPipelinesInput{})
		if listErr != nil || len(listed.Pipelines) != 0 {
			t.Fatal(listed, listErr)
		}
	}
}
