package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/bedrockagent"
	agenttypes "github.com/aws/aws-sdk-go-v2/service/bedrockagent/types"
	runtime "github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime"
	rt "github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func agentClients(s *httptest.Server) (control *sdk.Client, inference *runtime.Client) {
	return sdk.New(
			sdk.Options{
				Region:       "us-east-1",
				BaseEndpoint: new(s.URL),
				Credentials:  creds("test"),
				Retryer:      aws.NopRetryer{},
			},
		), runtime.New(
			runtime.Options{
				Region:       "us-east-1",
				BaseEndpoint: new(s.URL),
				Credentials:  creds("test"),
				Retryer:      aws.NopRetryer{},
			},
		)
}

func agentRole(t *testing.T, s *httptest.Server) *string {
	t.Helper()
	role, err := iamClient(
		s,
		"test",
		"us-east-1",
	).CreateRole(context.Background(), &iam.CreateRoleInput{
		RoleName: new("agent-role"),
		AssumeRolePolicyDocument: new(
			`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"bedroc` +
				`k.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	return role.Role.Arn
}

func agentStatus(t *testing.T, c *sdk.Client, id *string, want agenttypes.AgentStatus) {
	t.Helper()
	out, err := c.GetAgent(context.Background(), &sdk.GetAgentInput{AgentId: id})
	if err != nil || out.Agent.AgentStatus != want {
		t.Fatalf("status want %s: %+v %v", want, out, err)
	}
}

func preparedAgent(t *testing.T, s *httptest.Server) (control *sdk.Client, inference *runtime.Client, agentID *string) {
	t.Helper()
	c, r := agentClients(s)
	out, err := c.CreateAgent(
		context.Background(),
		&sdk.CreateAgentInput{
			AgentName:            new("assistant"),
			AgentResourceRoleArn: agentRole(t, s),
			FoundationModel:      new(bedrockClaude),
			Instruction:          new("Follow the supplied fixture and return only its configured response."),
			MemoryConfiguration: &agenttypes.MemoryConfiguration{
				EnabledMemoryTypes: []agenttypes.MemoryType{agenttypes.MemoryTypeSessionSummary},
				StorageDays:        new(int32(7)),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// AWS always returns the prompt configuration, which the Terraform provider dereferences; TinyStack has no
	// default prompt templates, so it is empty.
	if prompts := out.Agent.PromptOverrideConfiguration; prompts == nil || len(prompts.PromptConfigurations) != 0 {
		t.Fatalf("prompt override configuration: %+v", prompts)
	}

	id := out.Agent.AgentId
	replay, replayErr := c.CreateAgent(
		context.Background(),
		&sdk.CreateAgentInput{
			AgentName:            new("assistant"),
			AgentResourceRoleArn: out.Agent.AgentResourceRoleArn,
			FoundationModel:      new(bedrockClaude),
			ClientToken:          out.Agent.ClientToken,
		},
	)
	if replayErr != nil || *replay.Agent.AgentId != *id {
		t.Fatalf("idempotent create: %+v %v", replay, replayErr)
	}

	agentStatus(t, c, id, agenttypes.AgentStatusCreating)
	agentStatus(t, c, id, agenttypes.AgentStatusNotPrepared)
	if _, checkErr1 := c.PrepareAgent(context.Background(), &sdk.PrepareAgentInput{AgentId: id}); checkErr1 != nil {
		t.Fatal(checkErr1)
	}

	agentStatus(t, c, id, agenttypes.AgentStatusPreparing)
	agentStatus(t, c, id, agenttypes.AgentStatusPrepared)
	return c, r, id
}

func agentFixture(t *testing.T, s *httptest.Server, name string, fixture any) {
	t.Helper()
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(
		http.MethodPut,
		s.URL+"/_tinystack/bedrock-agent/fixtures/"+name,
		strings.NewReader(string(data)),
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("fixture: %d %s", resp.StatusCode, body)
	}
}

func TestBedrockAgentControlPlane(t *testing.T) {
	t.Parallel()
	s := server(t)
	c, r, id := preparedAgent(t, s)
	ctx := context.Background()
	got, err := c.GetAgentAlias(ctx, &sdk.GetAgentAliasInput{AgentId: id, AgentAliasId: new("TSTALIASID")})
	if err != nil || *got.AgentAlias.RoutingConfiguration[0].AgentVersion != "DRAFT" {
		t.Fatalf("test alias: %+v %v", got, err)
	}

	aliases, err := c.ListAgentAliases(ctx, &sdk.ListAgentAliasesInput{AgentId: id})
	if err != nil || len(aliases.AgentAliasSummaries) != 1 {
		t.Fatalf("aliases: %+v %v", aliases, err)
	}

	_, err = r.InvokeAgent(
		ctx,
		&runtime.InvokeAgentInput{
			AgentId:      id,
			AgentAliasId: new("TSTALIASID"),
			SessionId:    new("missing-fixture-session"),
			InputText:    new("hello"),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "/_tinystack/bedrock-agent/fixtures") {
		t.Fatalf("missing fixture: %v", err)
	}

	group, err := c.CreateAgentActionGroup(
		ctx,
		&sdk.CreateAgentActionGroupInput{
			AgentId:             id,
			AgentVersion:        new("DRAFT"),
			ActionGroupName:     new("lookup"),
			ActionGroupExecutor: &agenttypes.ActionGroupExecutorMemberCustomControl{Value: "RETURN_CONTROL"},
			FunctionSchema: &agenttypes.FunctionSchemaMemberFunctions{
				Value: []agenttypes.Function{{Name: new("lookup")}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	gid := group.AgentActionGroup.ActionGroupId
	if _, checkErr2 := c.GetAgentActionGroup(
		ctx,
		&sdk.GetAgentActionGroupInput{AgentId: id, AgentVersion: new("DRAFT"), ActionGroupId: gid},
	); checkErr2 != nil {
		t.Fatal(checkErr2)
	}

	groups, err := c.ListAgentActionGroups(
		ctx,
		&sdk.ListAgentActionGroupsInput{AgentId: id, AgentVersion: new("DRAFT")},
	)
	if err != nil || len(groups.ActionGroupSummaries) != 1 {
		t.Fatalf("groups: %+v %v", groups, err)
	}

	_, err = c.UpdateAgentActionGroup(
		ctx,
		&sdk.UpdateAgentActionGroupInput{
			AgentId:             id,
			AgentVersion:        new("DRAFT"),
			ActionGroupId:       gid,
			ActionGroupName:     new("lookup"),
			ActionGroupExecutor: &agenttypes.ActionGroupExecutorMemberCustomControl{Value: "RETURN_CONTROL"},
			ActionGroupState:    agenttypes.ActionGroupStateEnabled,
			FunctionSchema: &agenttypes.FunctionSchemaMemberFunctions{
				Value: []agenttypes.Function{{Name: new("lookup")}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	alias, err := c.CreateAgentAlias(
		ctx,
		&sdk.CreateAgentAliasInput{
			AgentId:        id,
			AgentAliasName: new("production"),
			Tags:           map[string]string{"team": "core"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	aid := alias.AgentAlias.AgentAliasId
	for range 2 {
		if _, checkErr3 := c.GetAgentAlias(
			ctx,
			&sdk.GetAgentAliasInput{AgentId: id, AgentAliasId: aid},
		); checkErr3 != nil {
			t.Fatal(checkErr3)
		}
	}

	version := alias.AgentAlias.RoutingConfiguration[0].AgentVersion
	v, err := c.GetAgentVersion(ctx, &sdk.GetAgentVersionInput{AgentId: id, AgentVersion: version})
	if err != nil || *v.AgentVersion.Version != "1" {
		t.Fatalf("version: %+v %v", v, err)
	}

	versions, err := c.ListAgentVersions(ctx, &sdk.ListAgentVersionsInput{AgentId: id, MaxResults: new(int32(1))})
	if err != nil || len(versions.AgentVersionSummaries) == 0 {
		t.Fatalf("versions: %+v %v", versions, err)
	}

	_, err = c.UpdateAgentAlias(
		ctx,
		&sdk.UpdateAgentAliasInput{
			AgentId:              id,
			AgentAliasId:         aid,
			AgentAliasName:       new("renamed"),
			RoutingConfiguration: []agenttypes.AgentAliasRoutingConfigurationListItem{{AgentVersion: version}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if _, checkErr4 := c.GetAgentAlias(
			ctx,
			&sdk.GetAgentAliasInput{AgentId: id, AgentAliasId: aid},
		); checkErr4 != nil {
			t.Fatal(checkErr4)
		}
	}

	for _, arn := range []*string{alias.AgentAlias.AgentAliasArn, v.AgentVersion.AgentArn} {
		if _, checkErr5 := c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: arn, Tags: map[string]string{"team": "platform"}},
		); checkErr5 != nil {
			t.Fatal(checkErr5)
		}

		tags, tagsErr := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		if tagsErr != nil || tags.Tags["team"] != "platform" {
			t.Fatalf("tags: %+v %v", tags, tagsErr)
		}

		if _, checkErr6 := c.UntagResource(
			ctx,
			&sdk.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}},
		); checkErr6 != nil {
			t.Fatal(checkErr6)
		}
	}

	// Numbered snapshots keep their original action group even after DRAFT is removed.
	if _, checkErr7 := c.DeleteAgentActionGroup(
		ctx,
		&sdk.DeleteAgentActionGroupInput{
			AgentId:                id,
			AgentVersion:           new("DRAFT"),
			ActionGroupId:          gid,
			SkipResourceInUseCheck: true,
		},
	); checkErr7 != nil {
		t.Fatal(checkErr7)
	}

	if _, checkErr8 := c.GetAgentActionGroup(
		ctx,
		&sdk.GetAgentActionGroupInput{AgentId: id, AgentVersion: version, ActionGroupId: gid},
	); checkErr8 != nil {
		t.Fatal(checkErr8)
	}

	_, err = c.DeleteAgentVersion(ctx, &sdk.DeleteAgentVersionInput{AgentId: id, AgentVersion: version})
	bedrockError(t, err, "ConflictException")
	_, err = c.DeleteAgent(ctx, &sdk.DeleteAgentInput{AgentId: id})
	bedrockError(t, err, "ConflictException")
	if _, checkErr9 := c.DeleteAgentAlias(
		ctx,
		&sdk.DeleteAgentAliasInput{AgentId: id, AgentAliasId: aid},
	); checkErr9 != nil {
		t.Fatal(checkErr9)
	}

	if _, checkErr10 := c.DeleteAgentVersion(
		ctx,
		&sdk.DeleteAgentVersionInput{AgentId: id, AgentVersion: version},
	); checkErr10 != nil {
		t.Fatal(checkErr10)
	}

	_, err = c.UpdateAgent(
		ctx,
		&sdk.UpdateAgentInput{
			AgentId:              id,
			AgentName:            new("updated"),
			FoundationModel:      new(bedrockClaude),
			AgentResourceRoleArn: v.AgentVersion.AgentResourceRoleArn,
			Instruction:          new("Updated fixture instruction with sufficient context for execution."),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	agentStatus(t, c, id, agenttypes.AgentStatusUpdating)
	agentStatus(t, c, id, agenttypes.AgentStatusNotPrepared)
	_, err = r.InvokeAgent(
		ctx,
		&runtime.InvokeAgentInput{
			AgentId:      id,
			AgentAliasId: new("TSTALIASID"),
			SessionId:    new("unprepared-session"),
			InputText:    new("hello"),
		},
	)
	bedrockError(t, err, "ConflictException")
	list, err := c.ListAgents(ctx, &sdk.ListAgentsInput{MaxResults: new(int32(1))})
	if err != nil || len(list.AgentSummaries) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}

	if _, checkErr11 := c.DeleteAgent(ctx, &sdk.DeleteAgentInput{AgentId: id}); checkErr11 != nil {
		t.Fatal(checkErr11)
	}

	agentStatus(t, c, id, agenttypes.AgentStatusDeleting)
	_, err = c.GetAgent(ctx, &sdk.GetAgentInput{AgentId: id})
	bedrockError(t, err, "ResourceNotFoundException")
}

func TestBedrockAgentKnowledgeBases(t *testing.T) {
	t.Parallel()
	s := server(t)
	c, _, id := preparedAgent(t, s)
	ctx := context.Background()
	agent, err := c.GetAgent(ctx, &sdk.GetAgentInput{AgentId: id})
	if err != nil {
		t.Fatal(err)
	}

	config := &agenttypes.KnowledgeBaseConfiguration{
		Type: agenttypes.KnowledgeBaseTypeVector,
		VectorKnowledgeBaseConfiguration: &agenttypes.VectorKnowledgeBaseConfiguration{
			EmbeddingModelArn: new("arn:aws:bedrock:us-east-1::foundation-model/amazon.titan-embed-text-v2:0"),
		},
	}

	secret := agentSecret(t, s)
	storageConfig := &agenttypes.StorageConfiguration{
		Type: agenttypes.KnowledgeBaseStorageTypePinecone,
		PineconeConfiguration: &agenttypes.PineconeConfiguration{
			ConnectionString:     new("https://index.example.test"),
			CredentialsSecretArn: secret,
			FieldMapping: &agenttypes.PineconeFieldMapping{
				TextField:     new("text"),
				MetadataField: new("metadata"),
			},
		},
	}

	kb, err := c.CreateKnowledgeBase(
		ctx,
		&sdk.CreateKnowledgeBaseInput{
			Name:                       new("knowledge"),
			RoleArn:                    agent.Agent.AgentResourceRoleArn,
			KnowledgeBaseConfiguration: config,
			StorageConfiguration:       storageConfig,
			Tags:                       map[string]string{"team": "core"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	kid := kb.KnowledgeBase.KnowledgeBaseId
	changedConfig := &agenttypes.KnowledgeBaseConfiguration{
		Type: agenttypes.KnowledgeBaseTypeVector,
		VectorKnowledgeBaseConfiguration: &agenttypes.VectorKnowledgeBaseConfiguration{
			EmbeddingModelArn: new("arn:aws:bedrock:us-east-1::foundation-model/cohere.embed-english-v3"),
		},
	}

	_, changeErr := c.UpdateKnowledgeBase(
		ctx,
		&sdk.UpdateKnowledgeBaseInput{
			KnowledgeBaseId:            kid,
			Name:                       new("knowledge"),
			RoleArn:                    agent.Agent.AgentResourceRoleArn,
			KnowledgeBaseConfiguration: changedConfig,
			StorageConfiguration:       storageConfig,
		},
	)
	bedrockError(t, changeErr, "ValidationException")

	if _, checkErr12 := c.GetKnowledgeBase(ctx, &sdk.GetKnowledgeBaseInput{KnowledgeBaseId: kid}); checkErr12 != nil {
		t.Fatal(checkErr12)
	}

	if _, checkErr13 := c.UpdateKnowledgeBase(
		ctx,
		&sdk.UpdateKnowledgeBaseInput{
			KnowledgeBaseId:            kid,
			Name:                       new("knowledge-updated"),
			RoleArn:                    agent.Agent.AgentResourceRoleArn,
			KnowledgeBaseConfiguration: config,
			StorageConfiguration:       storageConfig,
		},
	); checkErr13 != nil {
		t.Fatal(checkErr13)
	}

	if list, checkErr14 := c.ListKnowledgeBases(
		ctx,
		&sdk.ListKnowledgeBasesInput{},
	); checkErr14 != nil ||
		len(list.KnowledgeBaseSummaries) != 1 {
		t.Fatalf("knowledge list: %+v %v", list, checkErr14)
	}

	if _, checkErr15 := c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceArn: kb.KnowledgeBase.KnowledgeBaseArn,
			Tags:        map[string]string{"team": "platform"},
		},
	); checkErr15 != nil {
		t.Fatal(checkErr15)
	}

	if _, checkErr16 := c.AssociateAgentKnowledgeBase(
		ctx,
		&sdk.AssociateAgentKnowledgeBaseInput{
			AgentId:         id,
			AgentVersion:    new("DRAFT"),
			KnowledgeBaseId: kid,
			Description:     new("fixture metadata"),
		},
	); checkErr16 != nil {
		t.Fatal(checkErr16)
	}

	if _, checkErr17 := c.GetAgentKnowledgeBase(
		ctx,
		&sdk.GetAgentKnowledgeBaseInput{AgentId: id, AgentVersion: new("DRAFT"), KnowledgeBaseId: kid},
	); checkErr17 != nil {
		t.Fatal(checkErr17)
	}

	if _, checkErr18 := c.UpdateAgentKnowledgeBase(
		ctx,
		&sdk.UpdateAgentKnowledgeBaseInput{
			AgentId:            id,
			AgentVersion:       new("DRAFT"),
			KnowledgeBaseId:    kid,
			Description:        new("updated metadata"),
			KnowledgeBaseState: agenttypes.KnowledgeBaseStateDisabled,
		},
	); checkErr18 != nil {
		t.Fatal(checkErr18)
	}

	if list, checkErr19 := c.ListAgentKnowledgeBases(
		ctx,
		&sdk.ListAgentKnowledgeBasesInput{AgentId: id, AgentVersion: new("DRAFT")},
	); checkErr19 != nil ||
		len(list.AgentKnowledgeBaseSummaries) != 1 {
		t.Fatalf("associations: %+v %v", list, checkErr19)
	}

	if _, checkErr20 := c.DisassociateAgentKnowledgeBase(
		ctx,
		&sdk.DisassociateAgentKnowledgeBaseInput{AgentId: id, AgentVersion: new("DRAFT"), KnowledgeBaseId: kid},
	); checkErr20 != nil {
		t.Fatal(checkErr20)
	}

	bucket := s3Client(s, "test", "us-east-1")
	if _, checkErr21 := bucket.CreateBucket(
		ctx,
		&s3.CreateBucketInput{Bucket: new("agent-documents")},
	); checkErr21 != nil {
		t.Fatal(checkErr21)
	}

	dsconfig := &agenttypes.DataSourceConfiguration{
		Type:            agenttypes.DataSourceTypeS3,
		S3Configuration: &agenttypes.S3DataSourceConfiguration{BucketArn: new("arn:aws:s3:::agent-documents")},
	}

	ds, err := c.CreateDataSource(
		ctx,
		&sdk.CreateDataSourceInput{KnowledgeBaseId: kid, Name: new("documents"), DataSourceConfiguration: dsconfig},
	)
	if err != nil {
		t.Fatal(err)
	}

	did := ds.DataSource.DataSourceId
	if _, checkErr22 := c.GetDataSource(
		ctx,
		&sdk.GetDataSourceInput{KnowledgeBaseId: kid, DataSourceId: did},
	); checkErr22 != nil {
		t.Fatal(checkErr22)
	}

	if _, checkErr23 := c.UpdateDataSource(
		ctx,
		&sdk.UpdateDataSourceInput{
			KnowledgeBaseId:         kid,
			DataSourceId:            did,
			Name:                    new("updated-documents"),
			DataSourceConfiguration: dsconfig,
			DataDeletionPolicy:      agenttypes.DataDeletionPolicyRetain,
		},
	); checkErr23 != nil {
		t.Fatal(checkErr23)
	}

	if list, checkErr24 := c.ListDataSources(
		ctx,
		&sdk.ListDataSourcesInput{KnowledgeBaseId: kid},
	); checkErr24 != nil ||
		len(list.DataSourceSummaries) != 1 {
		t.Fatalf("sources: %+v %v", list, checkErr24)
	}

	if _, checkErr25 := c.DeleteDataSource(
		ctx,
		&sdk.DeleteDataSourceInput{KnowledgeBaseId: kid, DataSourceId: did},
	); checkErr25 != nil {
		t.Fatal(checkErr25)
	}

	if _, checkErr26 := c.DeleteKnowledgeBase(
		ctx,
		&sdk.DeleteKnowledgeBaseInput{KnowledgeBaseId: kid},
	); checkErr26 != nil {
		t.Fatal(checkErr26)
	}
}

func TestBedrockAgentSessions(t *testing.T) {
	t.Parallel()
	s := server(t)
	_, r := agentClients(s)
	ctx := context.Background()
	first, err := r.CreateSession(ctx, &runtime.CreateSessionInput{SessionMetadata: map[string]string{"team": "core"}})
	if err != nil {
		t.Fatal(err)
	}

	second, err := r.CreateSession(ctx, &runtime.CreateSessionInput{})
	if err != nil {
		t.Fatal(err)
	}

	list, err := r.ListSessions(ctx, &runtime.ListSessionsInput{MaxResults: new(int32(1))})
	if err != nil || len(list.SessionSummaries) != 1 || list.NextToken == nil {
		t.Fatalf("page: %+v %v", list, err)
	}

	tail, err := r.ListSessions(
		ctx,
		&runtime.ListSessionsInput{MaxResults: new(int32(1)), NextToken: list.NextToken},
	)
	if err != nil || len(tail.SessionSummaries) != 1 ||
		*tail.SessionSummaries[0].SessionId == *list.SessionSummaries[0].SessionId {
		t.Fatalf("tail: %+v %v", tail, err)
	}

	if _, checkErr27 := r.UpdateSession(
		ctx,
		&runtime.UpdateSessionInput{
			SessionIdentifier: first.SessionArn,
			SessionMetadata:   map[string]string{"team": "platform"},
		},
	); checkErr27 != nil {
		t.Fatal(checkErr27)
	}

	live, err := r.GetSession(ctx, &runtime.GetSessionInput{SessionIdentifier: first.SessionId})
	if err != nil || live.SessionMetadata["team"] != "platform" {
		t.Fatalf("session: %+v %v", live, err)
	}

	invocation, err := r.CreateInvocation(
		ctx,
		&runtime.CreateInvocationInput{SessionIdentifier: first.SessionId, Description: new("checkpoint")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if list, checkErr28 := r.ListInvocations(
		ctx,
		&runtime.ListInvocationsInput{SessionIdentifier: first.SessionId},
	); checkErr28 != nil ||
		len(list.InvocationSummaries) != 1 {
		t.Fatalf("invocations: %+v %v", list, checkErr28)
	}

	now := time.Now().UTC().Truncate(time.Second)
	step, err := r.PutInvocationStep(
		ctx,
		&runtime.PutInvocationStepInput{
			SessionIdentifier:    first.SessionId,
			InvocationIdentifier: invocation.InvocationId,
			InvocationStepTime:   new(now),
			Payload: &rt.InvocationStepPayloadMemberContentBlocks{
				Value: []rt.BedrockSessionContentBlock{
					&rt.BedrockSessionContentBlockMemberText{Value: "checkpoint text"},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	found, err := r.GetInvocationStep(
		ctx,
		&runtime.GetInvocationStepInput{
			SessionIdentifier:    first.SessionId,
			InvocationIdentifier: invocation.InvocationId,
			InvocationStepId:     step.InvocationStepId,
		},
	)
	if err != nil || !found.InvocationStep.InvocationStepTime.Equal(now) {
		t.Fatalf("step: %+v %v", found, err)
	}

	if list, checkErr29 := r.ListInvocationSteps(
		ctx,
		&runtime.ListInvocationStepsInput{
			SessionIdentifier:    first.SessionId,
			InvocationIdentifier: invocation.InvocationId,
		},
	); checkErr29 != nil ||
		len(list.InvocationStepSummaries) != 1 {
		t.Fatalf("steps: %+v %v", list, checkErr29)
	}

	if _, checkErr30 := r.EndSession(
		ctx,
		&runtime.EndSessionInput{SessionIdentifier: first.SessionId},
	); checkErr30 != nil {
		t.Fatal(checkErr30)
	}

	_, err = r.CreateInvocation(ctx, &runtime.CreateInvocationInput{SessionIdentifier: first.SessionId})
	bedrockError(t, err, "ConflictException")
	for _, sid := range []*string{first.SessionId, second.SessionId} {
		if _, checkErr31 := r.DeleteSession(
			ctx,
			&runtime.DeleteSessionInput{SessionIdentifier: sid},
		); checkErr31 != nil {
			t.Fatal(checkErr31)
		}
	}

	_, err = r.GetSession(ctx, &runtime.GetSessionInput{SessionIdentifier: first.SessionId})
	bedrockError(t, err, "ResourceNotFoundException")
}

func TestBedrockAgentReturnControl(t *testing.T) {
	t.Parallel()
	s := server(t)
	c, r, id := preparedAgent(t, s)
	ctx := context.Background()
	_, err := c.CreateAgentActionGroup(
		ctx,
		&sdk.CreateAgentActionGroupInput{
			AgentId:             id,
			AgentVersion:        new("DRAFT"),
			ActionGroupName:     new("lookup"),
			ActionGroupExecutor: &agenttypes.ActionGroupExecutorMemberCustomControl{Value: "RETURN_CONTROL"},
			FunctionSchema: &agenttypes.FunctionSchemaMemberFunctions{
				Value: []agenttypes.Function{{Name: new("lookup")}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	agentFixture(
		t,
		s,
		"return-control",
		map[string]any{
			"agentId":       *id,
			"contains":      "hello",
			"steps":         []any{map[string]any{"actionGroup": "lookup", "function": "lookup"}},
			"text":          "fixture answer",
			"memorySummary": "User supplied summary",
		},
	)
	turn, err := r.InvokeAgent(
		ctx,
		&runtime.InvokeAgentInput{
			AgentId:      id,
			AgentAliasId: new("TSTALIASID"),
			SessionId:    new("return-control-session"),
			InputText:    new("hello"),
			MemoryId:     new("user-memory"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	stream := turn.GetStream()
	var invocationID *string
	for event := range stream.Events() {
		if value, ok := event.(*rt.ResponseStreamMemberReturnControl); ok {
			invocationID = value.Value.InvocationId
		}
	}

	if checkErr32 := stream.Err(); checkErr32 != nil || invocationID == nil {
		t.Fatalf("return-control: %v %v", invocationID, checkErr32)
	}

	stream.Close()
	turn, err = r.InvokeAgent(
		ctx,
		&runtime.InvokeAgentInput{
			AgentId:      id,
			AgentAliasId: new("TSTALIASID"),
			SessionId:    new("return-control-session"),
			EndSession:   new(true),
			MemoryId:     new("user-memory"),
			SessionState: &rt.SessionState{
				InvocationId: invocationID,
				ReturnControlInvocationResults: []rt.InvocationResultMember{
					&rt.InvocationResultMemberMemberFunctionResult{
						Value: rt.FunctionResult{
							ActionGroup:  new("lookup"),
							Function:     new("lookup"),
							ResponseBody: map[string]rt.ContentBody{"TEXT": {Body: new("result")}},
						},
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	stream = turn.GetStream()
	text := ""
	for event := range stream.Events() {
		if chunk, ok := event.(*rt.ResponseStreamMemberChunk); ok {
			text += string(chunk.Value.Bytes)
		}
	}

	if checkErr33 := stream.Err(); checkErr33 != nil || text != "fixture answer" {
		t.Fatalf("answer: %q %v", text, checkErr33)
	}

	stream.Close()
	memory, err := r.GetAgentMemory(
		ctx,
		&runtime.GetAgentMemoryInput{
			AgentId:      id,
			AgentAliasId: new("TSTALIASID"),
			MemoryId:     new("user-memory"),
			MemoryType:   rt.MemoryTypeSessionSummary,
		},
	)
	if err != nil || len(memory.MemoryContents) != 1 {
		t.Fatalf("memory: %+v %v", memory, err)
	}

	if _, checkErr34 := r.DeleteAgentMemory(
		ctx,
		&runtime.DeleteAgentMemoryInput{AgentId: id, AgentAliasId: new("TSTALIASID"), MemoryId: new("user-memory")},
	); checkErr34 != nil {
		t.Fatal(checkErr34)
	}
}

func TestBedrockAgentPersistentRestart(t *testing.T) {
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c, r, id := preparedAgent(t, s)
	_ = c
	session, err := r.CreateSession(
		context.Background(),
		&runtime.CreateSessionInput{SessionMetadata: map[string]string{"persist": "yes"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	agentFixture(t, s, "persist", map[string]any{"agentId": *id, "contains": "persist", "text": "durable answer"})
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c, r = agentClients(s)
	agentStatus(t, c, id, agenttypes.AgentStatusPrepared)
	live, err := r.GetSession(
		context.Background(),
		&runtime.GetSessionInput{SessionIdentifier: session.SessionId},
	)
	if err != nil || live.SessionMetadata["persist"] != "yes" {
		t.Fatalf("restart: %+v %v", live, err)
	}

	out, err := r.InvokeAgent(
		context.Background(),
		&runtime.InvokeAgentInput{
			AgentId:      id,
			AgentAliasId: new("TSTALIASID"),
			SessionId:    new("persistent-conversation"),
			InputText:    new("persist"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	stream := out.GetStream()
	var text string
	for event := range stream.Events() {
		if chunk, ok := event.(*rt.ResponseStreamMemberChunk); ok {
			text += string(chunk.Value.Bytes)
		}
	}

	if checkErr35 := stream.Err(); checkErr35 != nil || text != "durable answer" {
		t.Fatal(fmt.Errorf("restart answer %q: %w", text, checkErr35))
	}

	stream.Close()
}

func TestBedrockAgentValidationAndGaps(t *testing.T) {
	t.Parallel()
	s := server(t)
	c, r := agentClients(s)
	ctx := context.Background()
	role := agentRole(t, s)
	for _, in := range []*sdk.CreateAgentInput{
		{
			AgentName:            new("missing-role"),
			AgentResourceRoleArn: new("arn:aws:iam::000000000000:role/missing"),
			FoundationModel:      new(bedrockClaude),
		},
		{AgentName: new("missing-model"), AgentResourceRoleArn: role, FoundationModel: new("unknown-model")},
		{
			AgentName:            new("supervisor"),
			AgentResourceRoleArn: role,
			FoundationModel:      new(bedrockClaude),
			AgentCollaboration:   agenttypes.AgentCollaborationSupervisor,
		},
	} {
		_, err := c.CreateAgent(ctx, in)
		bedrockError(t, err, "ValidationException")
	}

	_, err := c.CreateKnowledgeBase(
		ctx,
		&sdk.CreateKnowledgeBaseInput{
			Name:    new("managed-gap"),
			RoleArn: role,
			KnowledgeBaseConfiguration: &agenttypes.KnowledgeBaseConfiguration{
				Type: agenttypes.KnowledgeBaseTypeVector,
				VectorKnowledgeBaseConfiguration: &agenttypes.VectorKnowledgeBaseConfiguration{
					EmbeddingModelArn: new("arn:aws:bedrock:us-east-1::foundation-model/amazon.titan-embed-text-v2:0"),
				},
			},
			StorageConfiguration: &agenttypes.StorageConfiguration{
				Type: agenttypes.KnowledgeBaseStorageTypeOpensearchManagedCluster,
				OpensearchManagedClusterConfiguration: &agenttypes.OpenSearchManagedClusterConfiguration{
					DomainArn:       new("arn:aws:es:us-east-1:000000000000:domain/test"),
					DomainEndpoint:  new("https://domain.example.test"),
					VectorIndexName: new("vectors"),
					FieldMapping: &agenttypes.OpenSearchManagedClusterFieldMapping{
						VectorField:   new("vector"),
						TextField:     new("text"),
						MetadataField: new("metadata"),
					},
				},
			},
		},
	)
	bedrockError(t, err, "NotImplemented")
	_, err = r.CreateSession(ctx, &runtime.CreateSessionInput{Tags: map[string]string{"unsupported": "true"}})
	bedrockError(t, err, "NotImplemented")
	created, err := c.CreateAgent(
		ctx,
		&sdk.CreateAgentInput{
			AgentName:            new("error-agent"),
			AgentResourceRoleArn: role,
			FoundationModel:      new(bedrockClaude),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateAgentActionGroup(
		ctx,
		&sdk.CreateAgentActionGroupInput{
			AgentId:         created.Agent.AgentId,
			AgentVersion:    new("DRAFT"),
			ActionGroupName: new("missing-lambda"),
			ActionGroupExecutor: &agenttypes.ActionGroupExecutorMemberLambda{
				Value: "arn:aws:lambda:us-east-1:000000000000:function:missing",
			},
			FunctionSchema: &agenttypes.FunctionSchemaMemberFunctions{
				Value: []agenttypes.Function{{Name: new("lookup")}},
			},
		},
	)
	bedrockError(t, err, "ValidationException")
}

func agentSecret(t *testing.T, s *httptest.Server) *string {
	t.Helper()
	client := secretsmanager.New(
		secretsmanager.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	out, err := client.CreateSecret(
		context.Background(),
		&secretsmanager.CreateSecretInput{Name: new("agent-vector-credentials"), SecretString: new("local-test")},
	)
	if err != nil {
		t.Fatal(err)
	}

	return out.ARN
}
