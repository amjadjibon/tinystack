package codepipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	buildapi "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) cloudFormation(
	ctx context.Context,
	r *executionRecord,
	a *api.ActionDeclaration,
	c map[string]string,
) (map[string]string, []api.ArtifactDetail, error) {
	params := map[string]any{configStackName: c[configStackName]}
	if c["RoleArn"] != "" {
		params["RoleARN"] = c["RoleArn"]
	}

	mode := c["ActionMode"]
	if mode == "DELETE_ONLY" {
		_, err := s.caller.Call(ctx, "cloudformation", "DeleteStack", params)
		if err != nil {
			return nil, nil, err
		}

		_, err = s.waitStack(ctx, c[configStackName], true)
		return nil, nil, err
	}

	if mode != "CREATE_UPDATE" && mode != "REPLACE_ON_FAILURE" {
		return nil, nil, fmt.Errorf("unsupported CloudFormation ActionMode %s", mode)
	}

	body, err := s.artifactFile(ctx, r, c["TemplatePath"])
	if err != nil {
		return nil, nil, err
	}

	params["TemplateBody"] = string(body)
	if c["Capabilities"] != "" {
		list := []any{}
		for _, v := range strings.Split(c["Capabilities"], ",") {
			list = append(list, strings.TrimSpace(v))
		}

		params["Capabilities"] = list
	}

	if c["ParameterOverrides"] != "" {
		var values map[string]string
		if decodeErr := json.Unmarshal([]byte(c["ParameterOverrides"]), &values); decodeErr != nil {
			return nil, nil, decodeErr
		}

		list := []any{}
		for k, v := range values {
			list = append(list, map[string]any{"ParameterKey": k, "ParameterValue": v})
		}

		params["Parameters"] = list
	}

	described, describeErr := s.caller.Call(
		ctx,
		"cloudformation",
		"DescribeStacks",
		map[string]any{configStackName: c[configStackName]},
	)
	operation := "CreateStack"
	if describeErr == nil {
		operation = "UpdateStack"
		if mode == "REPLACE_ON_FAILURE" {
			stacks, _ := described["Stacks"].([]any)
			if len(stacks) > 0 {
				stack, _ := stacks[0].(map[string]any)
				status, _ := stack["StackStatus"].(string)
				if strings.Contains(status, "FAILED") || status == "ROLLBACK_COMPLETE" {
					if _, err = s.caller.Call(
						ctx,
						"cloudformation",
						"DeleteStack",
						map[string]any{configStackName: c[configStackName]},
					); err != nil {
						return nil, nil, err
					}

					if _, waitErr := s.waitStack(ctx, c[configStackName], true); waitErr != nil {
						return nil, nil, waitErr
					}

					operation = "CreateStack"
				}
			}
		}
	}

	_, err = s.caller.Call(ctx, "cloudformation", operation, params)
	if err != nil && !strings.Contains(err.Error(), "No updates are to be performed") {
		return nil, nil, err
	}

	stack, err := s.waitStack(ctx, c[configStackName], false)
	if err != nil {
		return nil, nil, err
	}

	values := map[string]string{}
	outputs, _ := stack["Outputs"].([]any)
	for _, v := range outputs {
		o, _ := v.(map[string]any)
		key, _ := o["OutputKey"].(string)
		value, _ := o["OutputValue"].(string)
		values[key] = value
	}

	artifacts := []api.ArtifactDetail{}
	if c["OutputFileName"] != "" {
		if len(a.OutputArtifacts) != 1 {
			return nil, nil, fmt.Errorf("outputFileName requires one output artifact")
		}

		var buf bytes.Buffer
		archive := zip.NewWriter(&buf)
		file, e := archive.Create(c["OutputFileName"])
		if e != nil {
			return nil, nil, e
		}

		if _, e = file.Write([]byte(mapJSON(values))); e != nil {
			return nil, nil, e
		}

		if closeErr := archive.Close(); closeErr != nil {
			return nil, nil, closeErr
		}

		artifact, e := s.putArtifact(ctx, r, *a.OutputArtifacts[0].Name, buf.Bytes())
		if e != nil {
			return nil, nil, e
		}

		artifacts = append(artifacts, artifact)
	} else if len(a.OutputArtifacts) > 0 {
		return nil, nil, fmt.Errorf("cloudFormation output artifact requires OutputFileName")
	}

	return values, artifacts, nil
}

func (s *Service) ecs(ctx context.Context, r *executionRecord, a *api.ActionDeclaration, c map[string]string) error {
	if c["DeploymentTimeout"] != "" {
		return fmt.Errorf("deploymentTimeout is not implemented")
	}

	if len(a.InputArtifacts) != 1 {
		return fmt.Errorf("ECS requires one input artifact")
	}

	file := c["FileName"]
	if file == "" {
		file = "imagedefinitions.json"
	}

	data, err := s.artifactFile(ctx, r, *a.InputArtifacts[0].Name+"::"+file)
	if err != nil {
		return err
	}

	var images []struct {
		Name     string `json:"name"`
		ImageURI string `json:"imageUri"`
	}

	if decodeErr := json.Unmarshal(data, &images); decodeErr != nil {
		return decodeErr
	}

	out, err := s.caller.Call(
		ctx,
		"ecs",
		"DescribeServices",
		map[string]any{"Cluster": c["ClusterName"], "Services": []any{c["ServiceName"]}},
	)
	if err != nil {
		return err
	}

	services, _ := out["Services"].([]any)
	if len(services) != 1 {
		return fmt.Errorf("ECS service not found")
	}

	service, _ := services[0].(map[string]any)
	out, err = s.caller.Call(
		ctx,
		"ecs",
		"DescribeTaskDefinition",
		map[string]any{"TaskDefinition": service["TaskDefinition"]},
	)
	if err != nil {
		return err
	}

	task, _ := out["TaskDefinition"].(map[string]any)
	params := map[string]any{}
	for _, key := range []string{
		"Family", "TaskRoleArn", "ExecutionRoleArn", "NetworkMode", "ContainerDefinitions", "Volumes",
		"PlacementConstraints", "RequiresCompatibilities", "Cpu", "Memory", "RuntimePlatform", "EphemeralStorage",
	} {
		if v, ok := task[key]; ok {
			params[key] = v
		}
	}

	containers, _ := params["ContainerDefinitions"].([]any)
	for _, image := range images {
		found := false
		for _, v := range containers {
			container, _ := v.(map[string]any)
			if container["Name"] == image.Name {
				container["Image"] = image.ImageURI
				found = true
			}
		}

		if !found || image.ImageURI == "" {
			return fmt.Errorf("invalid image definition for %s", image.Name)
		}
	}

	out, err = s.caller.Call(ctx, "ecs", "RegisterTaskDefinition", params)
	if err != nil {
		return err
	}

	task, _ = out["TaskDefinition"].(map[string]any)
	_, err = s.caller.Call(
		ctx,
		"ecs",
		"UpdateService",
		map[string]any{
			"Cluster":        c["ClusterName"],
			"Service":        c["ServiceName"],
			"TaskDefinition": task["TaskDefinitionArn"],
		},
	)
	return err
}

func (s *Service) build(
	ctx context.Context,
	c *claimedAction,
	a *api.ActionDeclaration,
	config map[string]string,
) error {
	if len(a.InputArtifacts) != 1 || len(a.OutputArtifacts) > 1 {
		return fmt.Errorf("codeBuild supports one input and at most one output artifact")
	}

	input := c.Record.Artifacts[*a.InputArtifacts[0].Name]
	if input.S3location == nil {
		return fmt.Errorf("codeBuild input artifact missing")
	}

	if config["PrimarySource"] != "" && config["PrimarySource"] != *a.InputArtifacts[0].Name {
		return fmt.Errorf("primarySource does not name an input artifact")
	}

	vars := []buildapi.EnvironmentVariable{}
	if config["EnvironmentVariables"] != "" {
		var env []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
			Type  string `json:"type"`
		}

		if err := json.Unmarshal([]byte(config["EnvironmentVariables"]), &env); err != nil {
			return err
		}

		for _, v := range env {
			typ := v.Type
			if typ == "" {
				typ = "PLAINTEXT"
			}

			vars = append(vars, buildapi.EnvironmentVariable{Name: new(v.Name), Value: new(v.Value), Type: new(typ)})
		}
	}

	output := &buildapi.ProjectArtifacts{Type: new("NO_ARTIFACTS")}
	var artifact api.ArtifactDetail
	if len(a.OutputArtifacts) == 1 {
		store := c.Record.Pipeline.ArtifactStore
		if store == nil {
			v := c.Record.Pipeline.ArtifactStores[c.Record.Scope.Region]
			store = new(v)
		}

		key := artifactKey(*c.Record.Pipeline.Name, *a.OutputArtifacts[0].Name)
		prefix, name, _ := strings.Cut(key, "/")
		// Name may contain the remaining artifact/id path.
		output = &buildapi.ProjectArtifacts{
			Type:          new("S3"),
			Location:      store.Location,
			Path:          new(prefix),
			Name:          new(name),
			Packaging:     new("ZIP"),
			NamespaceType: new("NONE"),
		}

		artifact = api.ArtifactDetail{
			Name:       a.OutputArtifacts[0].Name,
			S3location: &api.S3Location{Bucket: store.Location, Key: new(key)},
		}
	}

	started, err := s.builds.StartPipelineBuild(
		ctx,
		&buildapi.StartBuildInput{
			ProjectName:                  new(config["ProjectName"]),
			EnvironmentVariablesOverride: vars,
			ArtifactsOverride:            output,
		},
		*input.S3location.Bucket+"/"+*input.S3location.Key,
	)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		out, e := s.builds.BatchGetBuilds(ctx, &buildapi.BatchGetBuildsInput{Ids: []string{*started.Build.Id}})
		if e != nil {
			return e
		}

		if len(out.Builds) != 1 {
			return fmt.Errorf("codeBuild build disappeared")
		}

		b := out.Builds[0]
		if !common.Value(b.BuildComplete) {
			continue
		}

		if common.Value(b.BuildStatus) != eventSucceeded {
			return fmt.Errorf("codeBuild finished %s", common.Value(b.BuildStatus))
		}

		return s.completeAction(
			ctx,
			c.Key,
			*c.Action.Detail.ActionExecutionId,
			func(r *executionRecord, rec *actionRecord) {
				actionDone(rec, statusSucceeded, "CodeBuild succeeded")
				rec.Detail.Output.ExecutionResult.ExternalExecutionId = b.Id
				if artifact.Name != nil {
					r.Artifacts[*artifact.Name] = artifact
					rec.Detail.Output.OutputArtifacts = []api.ArtifactDetail{artifact}
				}
			},
		)
	}
}

func (s *Service) waitStack(ctx context.Context, name string, deleting bool) (map[string]any, error) {
	deadline := time.NewTimer(72 * time.Hour)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := s.caller.Call(ctx, "cloudformation", "DescribeStacks", map[string]any{configStackName: name})
		if err != nil {
			if deleting && strings.Contains(err.Error(), "does not exist") {
				return map[string]any{}, nil
			}

			return nil, err
		}

		stacks, _ := out["Stacks"].([]any)
		if len(stacks) == 0 {
			if deleting {
				return map[string]any{}, nil
			}

			return nil, fmt.Errorf("stack missing")
		}

		stack, _ := stacks[0].(map[string]any)
		status, _ := stack["StackStatus"].(string)
		if !deleting && (status == "CREATE_COMPLETE" || status == "UPDATE_COMPLETE") {
			return stack, nil
		}

		if !strings.HasSuffix(status, "_IN_PROGRESS") {
			return nil, fmt.Errorf("stack status %s", status)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("cloudformation action timed out")
		case <-ticker.C:
		}
	}
}
