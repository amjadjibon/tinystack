package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/keyspaces"
)

const keyspacesTemplate = `
Resources:
  App:
    Type: AWS::Cassandra::Keyspace
    Properties:
      KeyspaceName: app
      Tags: [{Key: stage, Value: first}]
  Events:
    Type: AWS::Cassandra::Table
    Properties:
      KeyspaceName: !Ref App
      TableName: events
      PartitionKeyColumns: [{ColumnName: id, ColumnType: text}]
      RegularColumns: [{ColumnName: payload, ColumnType: text}]
      DefaultTimeToLive: 60
Outputs:
  TableArn:
    Value: !GetAtt Events.Arn
  TableName:
    Value: !Ref Events
`

func TestCloudFormationKeyspaces(t *testing.T) {
	s := server(t)
	cf := cfnClient(s)
	c := keyspacesClient(s, "test", "us-east-1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const stack = "keyspaces"
	if _, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new(stack), TemplateBody: new(keyspacesTemplate)},
	); err != nil {
		t.Fatal(err)
	}

	created := settled(ctx, t, cf, stack)
	if created.StackStatus != cfntypes.StackStatusCreateComplete {
		t.Fatalf("stack status %s: %s", created.StackStatus, aws.ToString(created.StackStatusReason))
	}

	outputs := map[string]string{}
	for _, entry := range created.Outputs {
		outputs[aws.ToString(entry.OutputKey)] = aws.ToString(entry.OutputValue)
	}

	arn := "arn:aws:cassandra:us-east-1:000000000000:/keyspace/app/table/events"
	if outputs["TableArn"] != arn || outputs["TableName"] != "events" {
		t.Fatal(outputs)
	}

	update := strings.Replace(keyspacesTemplate, "{Key: stage, Value: first}", "{Key: owner, Value: local}", 1)
	update = strings.Replace(update, "ColumnType: text}]\n      DefaultTimeToLive: 60",
		"ColumnType: text}, {ColumnName: extra, ColumnType: int}]", 1)
	if _, err := cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new(stack), TemplateBody: new(update)},
	); err != nil {
		t.Fatal(err)
	}

	updated := settled(ctx, t, cf, stack)
	if updated.StackStatus != cfntypes.StackStatusUpdateComplete {
		t.Fatalf("stack status %s: %s", updated.StackStatus, aws.ToString(updated.StackStatusReason))
	}

	table := getKeyspacesTable(ctx, t, c, "app", "events")
	if aws.ToInt32(table.DefaultTimeToLive) != 0 || len(table.SchemaDefinition.AllColumns) != 3 {
		t.Fatalf("table properties: %+v", table)
	}

	tags, err := c.ListTagsForResource(ctx, &keyspaces.ListTagsForResourceInput{
		ResourceArn: new(strings.TrimSuffix(arn, "table/events")),
	})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "owner" {
		t.Fatalf("updated tags: %+v %v", tags, err)
	}

	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new(stack)}); err != nil {
		t.Fatal(err)
	}

	deleted := settled(ctx, t, cf, aws.ToString(created.StackId))
	if deleted.StackStatus != cfntypes.StackStatusDeleteComplete {
		t.Fatalf("stack status %s: %s", deleted.StackStatus, aws.ToString(deleted.StackStatusReason))
	}

	_, err = c.GetKeyspace(ctx, &keyspaces.GetKeyspaceInput{KeyspaceName: new("app")})
	wantCode(t, err, "ResourceNotFoundException")
}
