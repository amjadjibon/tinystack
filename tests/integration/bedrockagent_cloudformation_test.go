package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/bedrockagent"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestBedrockAgentCloudFormation(t *testing.T) {
	t.Parallel()
	s := server(t)
	role := agentRole(t, s)
	secret := agentSecret(t, s)
	ctx := context.Background()
	if _, checkErr1 := s3Client(
		s,
		"test",
		"us-east-1",
	).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("agent-stack-documents")}); checkErr1 != nil {
		t.Fatal(checkErr1)
	}

	template := map[string]any{"Resources": map[string]any{
		"Knowledge": map[string]any{"Type": "AWS::Bedrock::KnowledgeBase", "Properties": map[string]any{
			"Name":    "stack-knowledge",
			"RoleArn": *role,
			"Tags":    map[string]string{"team": "core"},
			"KnowledgeBaseConfiguration": map[string]any{
				"Type": "VECTOR",
				"VectorKnowledgeBaseConfiguration": map[string]any{
					"EmbeddingModelArn": "arn:aws:bedrock:us-east-1::foundation-model/amazon.titan-embed-text-v2:0",
				},
			},
			"StorageConfiguration": map[string]any{
				"Type": "PINECONE",
				"PineconeConfiguration": map[string]any{
					"ConnectionString":     "https://index.example.test",
					"CredentialsSecretArn": *secret,
					"FieldMapping":         map[string]any{"TextField": "text", "MetadataField": "metadata"},
				},
			},
		}},
		"Source": map[string]any{
			"Type": "AWS::Bedrock::DataSource",
			"Properties": map[string]any{
				"KnowledgeBaseId": map[string]any{"Ref": "Knowledge"},
				"Name":            "stack-documents",
				"DataSourceConfiguration": map[string]any{
					"Type":            "S3",
					"S3Configuration": map[string]any{"BucketArn": "arn:aws:s3:::agent-stack-documents"},
				},
			},
		},
		"Agent": map[string]any{"Type": "AWS::Bedrock::Agent", "Properties": map[string]any{
			"AgentName":            "stack-agent",
			"AgentResourceRoleArn": *role,
			"FoundationModel":      bedrockClaude,
			"AutoPrepare":          true,
			"Instruction":          "Follow the user supplied fixtures for deterministic local execution.",
			"Tags":                 map[string]string{"team": "core"},
			"ActionGroups": []any{
				map[string]any{"ActionGroupName": "input", "ParentActionGroupSignature": "AMAZON.UserInput"},
			},
			"KnowledgeBases": []any{
				map[string]any{
					"KnowledgeBaseId": map[string]any{"Ref": "Knowledge"},
					"Description":     "stack fixture metadata",
				},
			},
		}},
		"Alias": map[string]any{
			"Type": "AWS::Bedrock::AgentAlias",
			"Properties": map[string]any{
				"AgentId":        map[string]any{"Ref": "Agent"},
				"AgentAliasName": "stack-alias",
				"Tags":           map[string]string{"team": "core"},
			},
		},
	}}

	stacks := cfnClient(s)
	body, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}

	created, err := stacks.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("agent-stack"), TemplateBody: new(string(body))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, stacks, "agent-stack", cfntypes.StackStatusCreateComplete)
	resources, err := stacks.DescribeStackResources(
		ctx,
		&cfn.DescribeStackResourcesInput{StackName: new("agent-stack")},
	)
	if err != nil {
		t.Fatal(err)
	}

	ids := map[string]string{}

	for _, r := range resources.StackResources {
		ids[*r.LogicalResourceId] = *r.PhysicalResourceId
	}

	properties := template["Resources"].(map[string]any)["Agent"].(map[string]any)["Properties"].(map[string]any)
	properties["AgentName"] = "stack-agent-updated"
	properties["ActionGroups"] = []any{
		map[string]any{"ActionGroupName": "code", "ParentActionGroupSignature": "AMAZON.CodeInterpreter"},
	}

	properties["KnowledgeBases"] = []any{}

	properties["Tags"] = map[string]string{"team": "platform"}

	body, err = json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}

	if _, checkErr3 := stacks.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("agent-stack"), TemplateBody: new(string(body))},
	); checkErr3 != nil {
		t.Fatal(checkErr3)
	}

	wantStatus(ctx, t, stacks, "agent-stack", cfntypes.StackStatusUpdateComplete)
	c, _ := agentClients(s)
	agent, err := c.GetAgent(ctx, &sdk.GetAgentInput{AgentId: new(ids["Agent"])})
	if err != nil || *agent.Agent.AgentName != "stack-agent-updated" {
		t.Fatalf("updated agent: %+v %v", agent, err)
	}

	groups, err := c.ListAgentActionGroups(
		ctx,
		&sdk.ListAgentActionGroupsInput{AgentId: new(ids["Agent"]), AgentVersion: new("DRAFT")},
	)
	if err != nil || len(groups.ActionGroupSummaries) != 1 ||
		*groups.ActionGroupSummaries[0].ActionGroupName != "code" {
		t.Fatalf("updated groups: %+v %v", groups, err)
	}

	if _, checkErr4 := stacks.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("agent-stack")}); checkErr4 != nil {
		t.Fatal(checkErr4)
	}

	wantStatus(ctx, t, stacks, *created.StackId, cfntypes.StackStatusDeleteComplete)
}
