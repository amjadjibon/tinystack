package codepipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func artifactKey(pipeline, artifact string) string {
	return pipeline[:min(100, len(pipeline))] + "/" + artifact[:min(100, len(artifact))] + "/" + common.RandomID(
		7,
		"abcdefghijklmnopqrstuvwxyz0123456789",
	)
}

func (s *Service) object(
	ctx context.Context,
	bucket, key, version string,
) (data []byte, metadata map[string]any, err error) {
	params := map[string]any{"Bucket": bucket, "Key": key}
	if version != "" {
		params["VersionId"] = version
	}

	out, err := s.caller.Base64Blobs().Call(ctx, "s3", "GetObject", params)
	if err != nil {
		return nil, nil, err
	}

	body, _ := out["Body"].(string)
	data, err = base64.StdEncoding.DecodeString(body)
	return data, out, err
}

func (s *Service) putArtifact(
	ctx context.Context,
	r *executionRecord,
	name string,
	data []byte,
) (api.ArtifactDetail, error) {
	store := r.Pipeline.ArtifactStore
	if store == nil {
		v := r.Pipeline.ArtifactStores[r.Scope.Region]
		store = new(v)
	}

	key := artifactKey(*r.Pipeline.Name, name)
	_, err := s.caller.Base64Blobs().
		Call(ctx, "s3", "PutObject", map[string]any{
			"Bucket": *store.Location, "Key": key, "Body": base64.StdEncoding.EncodeToString(data),
		})

	return api.ArtifactDetail{Name: new(name), S3location: &api.S3Location{Bucket: store.Location, Key: new(key)}}, err
}

func (s *Service) artifactFile(ctx context.Context, r *executionRecord, spec string) ([]byte, error) {
	artifact, file, ok := strings.Cut(spec, "::")
	if !ok {
		return nil, fmt.Errorf("expected artifact::path")
	}

	a, ok := r.Artifacts[artifact]
	if !ok || a.S3location == nil {
		return nil, fmt.Errorf("artifact %s missing", artifact)
	}

	data, _, err := s.object(ctx, *a.S3location.Bucket, *a.S3location.Key, "")
	if err != nil {
		return nil, err
	}

	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}

	f, err := archive.Open(file)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()
	// #nosec G110 -- pipeline archives use the same local artifact policy as CodeBuild.
	return io.ReadAll(f)
}

func (s *Service) run(ctx context.Context, c *claimedAction) {
	r := &c.Record
	a := new(r.Pipeline.Stages[c.Action.Stage].Actions[c.Action.Action])
	m, _ := request.FromContext(ctx)
	m.Scope = r.Scope
	if a.Region != nil {
		m.Region = *a.Region
	}

	ctx = request.WithMetadata(ctx, m)
	id := *c.Action.Detail.ActionExecutionId
	config := map[string]string{}
	var err error
	for k, v := range a.Configuration {
		config[k], err = resolve(v, r.Values)
		if err != nil {
			break
		}
	}

	store := r.Pipeline.ArtifactStore
	if store == nil {
		v := r.Pipeline.ArtifactStores[r.Scope.Region]
		store = new(v)
	}

	if store.EncryptionKey != nil {
		err = fmt.Errorf("artifact-store encryption keys are not implemented")
	}

	if err == nil {
		err = configurationOK(a, config)
	}

	variables := map[string]string{}
	artifacts := []api.ArtifactDetail{}
	if err == nil {
		s.emit(
			ctx,
			r,
			"Action",
			"STARTED",
			map[string]any{eventStage: *c.Action.Detail.StageName, eventAction: *a.Name},
		)
		switch *a.ActionTypeId.Category + "/" + *a.ActionTypeId.Provider {
		case actionCodeCommit:
			var data []byte
			data, variables, err = s.commitSource(ctx, r, a, config)
			if err == nil {
				artifacts, err = s.outputs(ctx, r, a, data)
			}
		case actionS3:
			override := r.Revisions[*a.Name]
			var data []byte
			var meta map[string]any
			data, meta, err = s.object(
				ctx,
				config["S3Bucket"],
				config["S3ObjectKey"],
				common.Value(override.RevisionValue),
			)
			if err == nil {
				version, _ := meta["VersionId"].(string)
				etag, _ := meta["ETag"].(string)
				variables = map[string]string{
					"BucketName": config["S3Bucket"],
					"ObjectKey":  config["S3ObjectKey"],
					"VersionId":  version,
					"ETag":       etag,
				}

				artifacts, err = s.outputs(ctx, r, a, data)
			}
		case "Approval/Manual":
			if config["NotificationArn"] != "" {
				_, err = s.caller.Call(
					ctx,
					"sns",
					"Publish",
					map[string]any{
						"TopicArn": config["NotificationArn"],
						"Message": mapJSON(
							map[string]any{
								"approval": map[string]any{
									"pipelineName":       *r.Pipeline.Name,
									"stageName":          *c.Action.Detail.StageName,
									"actionName":         *a.Name,
									"token":              c.Action.Token,
									"customData":         config["CustomData"],
									"externalEntityLink": config["ExternalEntityLink"],
								},
							},
						),
					},
				)
			}
		case actionCloudFormation:
			variables, artifacts, err = s.cloudFormation(ctx, r, a, config)
		case actionECS:
			err = s.ecs(ctx, r, a, config)
		case actionLambda:
			inputs := []any{}
			for _, input := range a.InputArtifacts {
				v := r.Artifacts[*input.Name]
				inputs = append(
					inputs,
					map[string]any{
						"name": *input.Name,
						"location": map[string]any{
							"type": "S3",
							"s3Location": map[string]any{
								"bucketName": *v.S3location.Bucket,
								"objectKey":  *v.S3location.Key,
							},
						},
					},
				)
			}

			payload := map[string]any{
				"CodePipeline.job": map[string]any{
					"id":        c.Action.Job,
					"accountId": r.Scope.AccountID,
					"data": map[string]any{
						"actionConfiguration": map[string]any{
							"configuration": map[string]any{
								configFunction:   config[configFunction],
								"UserParameters": config["UserParameters"],
							},
						},
						"inputArtifacts":  inputs,
						"outputArtifacts": []any{},
						"pipelineContext": map[string]any{
							"pipelineName":        *r.Pipeline.Name,
							"pipelineExecutionId": *r.Execution.PipelineExecutionId,
						},
					},
				},
			}

			_, err = s.caller.Call(
				ctx,
				"lambda",
				categoryInvoke,
				map[string]any{configFunction: config[configFunction], "InvocationType": "Event", "Payload": payload},
			)
		case actionCodeBuild:
			err = s.build(ctx, c, a, config)
		default:
			err = fmt.Errorf("unsupported action %s/%s", *a.ActionTypeId.Category, *a.ActionTypeId.Provider)
		}
	}

	if ctx.Err() != nil {
		return
	}

	updateErr := s.completeAction(ctx, c.Key, id, func(r *executionRecord, rec *actionRecord) {
		rec.Detail.Input.ResolvedConfiguration = config
		for _, input := range a.InputArtifacts {
			rec.Detail.Input.InputArtifacts = append(rec.Detail.Input.InputArtifacts, r.Artifacts[*input.Name])
		}

		if err != nil {
			actionDone(rec, statusFailed, err.Error())
			return
		}

		if c.Action.Waiting || (*a.ActionTypeId.Provider == "CodeBuild" && rec.Waiting) {
			return
		}

		actionDone(rec, statusSucceeded, "Action succeeded")
		rec.Detail.Output.OutputArtifacts = artifacts
		rec.Detail.Output.OutputVariables = variables
		for _, artifact := range artifacts {
			r.Artifacts[*artifact.Name] = artifact
		}

		if *a.ActionTypeId.Category == categorySource {
			revision := variables["CommitId"]
			if revision == "" {
				revision = variables["VersionId"]
			}

			if revision == "" {
				revision = variables["ETag"]
			}

			summary := variables["CommitMessage"]
			r.Sources = append(
				r.Sources,
				api.SourceRevision{ActionName: a.Name, RevisionId: new(revision), RevisionSummary: new(summary)},
			)
			for _, artifact := range artifacts {
				r.Execution.ArtifactRevisions = append(
					r.Execution.ArtifactRevisions,
					api.ArtifactRevision{
						Name:            artifact.Name,
						RevisionId:      new(revision),
						RevisionSummary: new(summary),
						Created:         rec.Detail.StartTime,
					},
				)
			}
		}

		if a.Namespace != nil {
			for k, v := range variables {
				r.Values[*a.Namespace+"."+k] = v
			}
		}
	})

	_ = updateErr // A concurrent stop or reset can remove a claimed action.
}

func mapJSON(v any) string { data, _ := json.Marshal(v); return string(data) }

func configurationOK(a *api.ActionDeclaration, c map[string]string) error {
	if *a.ActionTypeId.Owner != ownerAWS {
		return fmt.Errorf("only AWS action types are supported")
	}

	allowed := map[string]string{
		actionCodeCommit:     "RepositoryName BranchName PollForSourceChanges OutputArtifactFormat",
		actionS3:             "S3Bucket S3ObjectKey PollForSourceChanges AllowOverrideForS3ObjectKey",
		"Approval/Manual":    "NotificationArn CustomData ExternalEntityLink",
		actionCodeBuild:      "ProjectName PrimarySource EnvironmentVariables",
		actionCloudFormation: "ActionMode TemplatePath ParameterOverrides Capabilities RoleArn StackName OutputFileName",
		actionECS:            "ClusterName ServiceName FileName DeploymentTimeout",
		actionLambda:         "FunctionName UserParameters",
	}

	if value := c["PollForSourceChanges"]; value != "" && value != "true" && value != "false" {
		return fmt.Errorf("invalid PollForSourceChanges")
	}

	if c["AllowOverrideForS3ObjectKey"] != "" {
		return fmt.Errorf("AllowOverrideForS3ObjectKey is not implemented")
	}

	if *a.ActionTypeId.Category == categoryInvoke && len(a.OutputArtifacts) > 0 {
		return fmt.Errorf("lambda output artifacts are not implemented")
	}

	if *a.ActionTypeId.Category == categoryApproval && (len(a.InputArtifacts) > 0 || len(a.OutputArtifacts) > 0) {
		return fmt.Errorf("manual approval does not accept artifacts")
	}

	required := map[string][]string{
		actionCodeCommit: {
			configRepository,
			"BranchName",
		},
		actionS3:             {"S3Bucket", "S3ObjectKey"},
		actionCodeBuild:      {"ProjectName"},
		actionCloudFormation: {"ActionMode", configStackName},
		actionECS:            {"ServiceName"},
		actionLambda:         {configFunction},
	}

	for _, key := range required[*a.ActionTypeId.Category+"/"+*a.ActionTypeId.Provider] {
		if c[key] == "" {
			return fmt.Errorf("missing action configuration %s", key)
		}
	}

	keys, ok := allowed[*a.ActionTypeId.Category+"/"+*a.ActionTypeId.Provider]
	if !ok {
		return fmt.Errorf("unsupported action %s/%s", *a.ActionTypeId.Category, *a.ActionTypeId.Provider)
	}

	for k := range c {
		if !strings.Contains(" "+keys+" ", " "+k+" ") {
			return fmt.Errorf("unsupported configuration %s", k)
		}
	}

	if a.Region != nil || a.RoleArn != nil {
		return fmt.Errorf("cross-Region and action role overrides are not implemented")
	}

	return nil
}

func (s *Service) commitSource(
	ctx context.Context,
	r *executionRecord,
	a *api.ActionDeclaration,
	c map[string]string,
) (data []byte, variables map[string]string, err error) {
	if c["OutputArtifactFormat"] != "" && c["OutputArtifactFormat"] != "CODE_ZIP" {
		return nil, nil, fmt.Errorf("only CODE_ZIP is supported")
	}

	commit := common.Value(r.Revisions[*a.Name].RevisionValue)
	if commit == "" {
		commit, err = s.commits.Branch(ctx, r.Scope, c[configRepository], c["BranchName"])
		if err != nil {
			return nil, nil, err
		}
	}

	var buffer bytes.Buffer
	if archiveErr := s.commits.Archive(ctx, r.Scope, c[configRepository], commit, &buffer); archiveErr != nil {
		return nil, nil, archiveErr
	}

	out, err := s.caller.Call(
		ctx,
		"codecommit",
		"GetCommit",
		map[string]any{configRepository: c[configRepository], "CommitId": commit},
	)
	if err != nil {
		return nil, nil, err
	}

	meta, _ := out["Commit"].(map[string]any)
	author, _ := meta["Author"].(map[string]any)
	committer, _ := meta["Committer"].(map[string]any)
	authorDate, dateErr := commitDate(author["Date"])
	if dateErr != nil {
		return nil, nil, dateErr
	}

	committerDate, dateErr := commitDate(committer["Date"])
	if dateErr != nil {
		return nil, nil, dateErr
	}

	return buffer.Bytes(), map[string]string{
		"CommitId":       commit,
		configRepository: c[configRepository],
		"BranchName":     c["BranchName"],
		"CommitMessage":  fmt.Sprint(meta["Message"]),
		"AuthorDate":     authorDate,
		"CommitterDate":  committerDate,
	}, nil
}

func (s *Service) outputs(
	ctx context.Context,
	r *executionRecord,
	a *api.ActionDeclaration,
	data []byte,
) ([]api.ArtifactDetail, error) {
	if len(a.OutputArtifacts) != 1 {
		return nil, fmt.Errorf("source requires exactly one output artifact")
	}

	artifact, err := s.putArtifact(ctx, r, *a.OutputArtifacts[0].Name, data)
	return []api.ArtifactDetail{artifact}, err
}

// SourceChanged is called after a committed CodeCommit branch update, outside storage transactions.
func (s *Service) SourceChanged(ctx context.Context, scope request.Scope, repo, branch, commit string) {
	if commit == "" {
		return
	}

	var pipelines []*record
	err := s.store.View(ctx, func(tx storage.ReadTx) (e error) {
		pipelines, e = storage.Records[record](tx, scope.Prefix(serviceName, pipelineKind))
		return e
	})
	if err != nil {
		return
	}

	for _, p := range pipelines {
		for _, a := range p.Pipeline.Stages[0].Actions {
			c := a.Configuration
			if *a.ActionTypeId.Provider != providerCodeCommit || c[configRepository] != repo ||
				c["BranchName"] != branch ||
				c["PollForSourceChanges"] == "false" {
				continue
			}

			input := &api.StartPipelineExecutionInput{Name: p.Pipeline.Name}
			if common.Value(p.Pipeline.ExecutionMode) != modeParallel {
				input.SourceRevisions = []api.SourceRevisionOverride{
					{ActionName: a.Name, RevisionType: new("COMMIT_ID"), RevisionValue: new(commit)},
				}
			}

			// Source changes are advisory; an invalid pipeline is reported by its execution summary.
			_, _ = s.start(ctx, input, "CloudWatchEvent")
			break
		}
	}
}

// CodeCommit returns git's epoch-and-offset dates; pipeline variables use ISO 8601 UTC.
func commitDate(value any) (string, error) {
	raw, _ := value.(string)
	epoch, _, _ := strings.Cut(raw, " ")
	seconds, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid commit date: %w", err)
	}

	return time.Unix(seconds, 0).UTC().Format(time.RFC3339), nil
}
