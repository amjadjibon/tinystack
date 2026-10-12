package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/codepipeline"
)

func codePipelineTemplateAction(name, category, provider string, in, out []any) map[string]any {
	a := map[string]any{
		"Name":          name,
		"ActionTypeId":  map[string]any{"Category": category, "Owner": "AWS", "Provider": provider, "Version": "1"},
		"Configuration": map[string]any{"PollForSourceChanges": false, "Key": "value"},
	}
	if in != nil {
		a["InputArtifacts"] = in
	}

	if out != nil {
		a["OutputArtifacts"] = out
	}

	return a
}

// codePipelineTemplate declares one pipeline with extra resource properties merged over the defaults.
func codePipelineTemplate(extra map[string]any) string {
	props := map[string]any{
		"Name":          "cfn-pipeline",
		"RoleArn":       "arn:aws:iam::123456789012:role/pipeline",
		"ArtifactStore": map[string]any{"Type": "S3", "Location": "artifacts"},
		"Stages": []any{
			map[string]any{"Name": "Source", "Actions": []any{
				codePipelineTemplateAction("Fetch", "Source", "S3", nil, []any{map[string]any{"Name": "src"}}),
			}},
			map[string]any{"Name": "Build", "Actions": []any{
				codePipelineTemplateAction("Compile", "Build", "CodeBuild", []any{map[string]any{"Name": "src"}}, nil),
			}},
		},
		"Tags": []any{map[string]any{"Key": "Env", "Value": "Dev"}},
	}
	for k, v := range extra {
		props[k] = v
	}

	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"Pipeline": map[string]any{"Type": "AWS::CodePipeline::Pipeline", "Properties": props},
		},
		"Outputs": map[string]any{
			"Ref":     map[string]any{"Value": map[string]any{"Ref": "Pipeline"}},
			"Version": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Pipeline", "Version"}}},
		},
	})

	return string(raw)
}

func TestCloudFormationCodePipelineLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	pipelines := codePipelineClient(s, "us-east-1")
	stack := aws.String("codepipeline-stack")

	if _, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: stack, TemplateBody: aws.String(codePipelineTemplate(nil)),
	}); err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, cf, *stack, cfntypes.StackStatusCreateComplete)
	if out := outputsOf(&st); out["Ref"] != "cfn-pipeline" || out["Version"] != "1" {
		t.Fatalf("outputs = %v", out)
	}

	got, err := pipelines.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("cfn-pipeline")})
	if err != nil || len(got.Pipeline.Stages) != 2 ||
		got.Pipeline.Stages[0].Actions[0].Configuration["PollForSourceChanges"] != "false" {
		t.Fatal(got, err)
	}

	tags, err := pipelines.ListTagsForResource(
		ctx,
		&sdk.ListTagsForResourceInput{ResourceArn: got.Metadata.PipelineArn},
	)
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "Dev" {
		t.Fatal(tags, err)
	}

	// Changing the declaration updates the pipeline in place and adds a version.
	update := codePipelineTemplate(map[string]any{
		"ExecutionMode": "QUEUED", "PipelineType": "V2", "RestartExecutionOnUpdate": false,
		"Variables": []any{map[string]any{"Name": "stage", "DefaultValue": "dev"}},
		"Tags":      []any{map[string]any{"Key": "Env", "Value": "Prod"}},
	})
	if _, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: stack, TemplateBody: aws.String(update)},
	); err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, cf, *stack, cfntypes.StackStatusUpdateComplete)
	if out := outputsOf(&st); out["Ref"] != "cfn-pipeline" || out["Version"] != "2" {
		t.Fatalf("outputs = %v", out)
	}

	got, err = pipelines.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("cfn-pipeline")})
	if err != nil || aws.ToInt32(got.Pipeline.Version) != 2 || string(got.Pipeline.ExecutionMode) != "QUEUED" {
		t.Fatal(got, err)
	}

	tags, err = pipelines.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: got.Metadata.PipelineArn})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "Prod" {
		t.Fatal(tags, err)
	}

	// Properties TinyStack cannot store fail the update, which rolls back and leaves the pipeline as it was.
	for _, extra := range []map[string]any{
		{
			"ExecutionMode": "QUEUED", "PipelineType": "V2",
			"Triggers": []any{map[string]any{"ProviderType": "CodeStarSourceConnection"}},
		},
		{"DisableInboundStageTransitions": []any{map[string]any{"StageName": "Build", "Reason": "hold"}}},
		{"RestartExecutionOnUpdate": true},
	} {
		if _, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
			StackName: stack, TemplateBody: aws.String(codePipelineTemplate(extra)),
		}); err != nil {
			t.Fatal(err)
		}

		wantStatus(ctx, t, cf, *stack, cfntypes.StackStatusUpdateRollbackComplete)
	}

	got, err = pipelines.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("cfn-pipeline")})
	if err != nil || aws.ToInt32(got.Pipeline.Version) != 2 {
		t.Fatal(got, err)
	}

	// Renaming the pipeline replaces it.
	rename := codePipelineTemplate(map[string]any{"Name": "cfn-renamed"})
	if _, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: stack, TemplateBody: aws.String(rename)},
	); err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, cf, *stack, cfntypes.StackStatusUpdateComplete)
	if out := outputsOf(&st); out["Ref"] != "cfn-renamed" || out["Version"] != "1" {
		t.Fatalf("outputs = %v", out)
	}

	if _, err = pipelines.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("cfn-pipeline")}); err == nil {
		t.Fatal("the replaced pipeline is still there")
	}

	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: stack}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	if _, err = pipelines.GetPipeline(ctx, &sdk.GetPipelineInput{Name: new("cfn-renamed")}); err == nil {
		t.Fatal("the pipeline survived the stack")
	}
}
