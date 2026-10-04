package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func TestStepFunctionsVersionsAndAliases(t *testing.T) {
	c := sfnClient(server(t))
	ctx := context.Background()
	pass := func(result string) string {
		return `{"StartAt": "A", "States": {"A": {"Type": "Pass", "Result": "` + result + `", "End": true}}}`
	}

	created, err := c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
		Name: aws.String("versioned"), Definition: aws.String(pass("one")), RoleArn: aws.String(sfnRole),
		Publish: true, VersionDescription: aws.String("first"),
	})
	if err != nil || !strings.HasSuffix(aws.ToString(created.StateMachineVersionArn), ":versioned:1") {
		t.Fatalf("%+v %v", created, err)
	}

	machine := created.StateMachineArn
	v1 := created.StateMachineVersionArn

	// Publishing an unchanged revision returns the latest version.
	same, err := c.PublishStateMachineVersion(ctx, &sfn.PublishStateMachineVersionInput{StateMachineArn: machine})
	if err != nil || aws.ToString(same.StateMachineVersionArn) != aws.ToString(v1) {
		t.Fatalf("%+v %v", same, err)
	}

	updated, err := c.UpdateStateMachine(ctx, &sfn.UpdateStateMachineInput{
		StateMachineArn: machine, Definition: aws.String(pass("two")), Publish: true,
	})
	if err != nil || !strings.HasSuffix(aws.ToString(updated.StateMachineVersionArn), ":versioned:2") {
		t.Fatalf("%+v %v", updated, err)
	}

	v2 := updated.StateMachineVersionArn
	_, err = c.PublishStateMachineVersion(ctx, &sfn.PublishStateMachineVersionInput{
		StateMachineArn: machine, RevisionId: aws.String("stale"),
	})
	code(t, err, "ConflictException")

	described, err := c.DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{StateMachineArn: v1})
	if err != nil || !strings.Contains(aws.ToString(described.Definition), "one") ||
		aws.ToString(described.Description) != "first" || aws.ToString(described.StateMachineArn) != aws.ToString(v1) {
		t.Fatalf("%+v %v", described, err)
	}

	versions, err := c.ListStateMachineVersions(ctx, &sfn.ListStateMachineVersionsInput{StateMachineArn: machine})
	if err != nil || len(versions.StateMachineVersions) != 2 ||
		aws.ToString(versions.StateMachineVersions[0].StateMachineVersionArn) != aws.ToString(v2) {
		t.Fatalf("%+v %v", versions, err)
	}

	// An alias routes executions to its versions.
	_, err = c.CreateStateMachineAlias(ctx, &sfn.CreateStateMachineAliasInput{
		Name: aws.String("bad"), RoutingConfiguration: []sfntypes.RoutingConfigurationListItem{
			{StateMachineVersionArn: v1, Weight: 60}, {StateMachineVersionArn: v2, Weight: 30},
		},
	})
	code(t, err, "ValidationException")

	aliased, err := c.CreateStateMachineAlias(ctx, &sfn.CreateStateMachineAliasInput{
		Name: aws.String("live"), Description: aws.String("production"),
		RoutingConfiguration: []sfntypes.RoutingConfigurationListItem{{StateMachineVersionArn: v1, Weight: 100}},
	})
	if err != nil || !strings.HasSuffix(aws.ToString(aliased.StateMachineAliasArn), ":versioned:live") {
		t.Fatalf("%+v %v", aliased, err)
	}

	live := aliased.StateMachineAliasArn
	for target, want := range map[*string]string{live: `"one"`, v2: `"two"`, machine: `"two"`} {
		exec := start(t, c, target, `{}`)
		out := finish(t, c, exec)
		if out.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(out.Output) != want {
			t.Fatalf("%s: %+v", aws.ToString(target), out)
		}

		switch target {
		case live:
			if aws.ToString(out.StateMachineAliasArn) != aws.ToString(live) ||
				aws.ToString(out.StateMachineVersionArn) != aws.ToString(v1) {
				t.Fatalf("alias execution %+v", out)
			}
		case machine:
			if out.StateMachineVersionArn != nil {
				t.Fatalf("unqualified execution %+v", out)
			}
		}
	}

	byAlias, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{StateMachineArn: live})
	if err != nil || len(byAlias.Executions) != 1 {
		t.Fatalf("%+v %v", byAlias, err)
	}

	_, err = c.DeleteStateMachineVersion(ctx, &sfn.DeleteStateMachineVersionInput{StateMachineVersionArn: v1})
	code(t, err, "ConflictException")

	if _, err = c.UpdateStateMachineAlias(ctx, &sfn.UpdateStateMachineAliasInput{
		StateMachineAliasArn: live,
		RoutingConfiguration: []sfntypes.RoutingConfigurationListItem{
			{StateMachineVersionArn: v1, Weight: 50}, {StateMachineVersionArn: v2, Weight: 50},
		},
	}); err != nil {
		t.Fatal(err)
	}

	alias, err := c.DescribeStateMachineAlias(ctx, &sfn.DescribeStateMachineAliasInput{StateMachineAliasArn: live})
	if err != nil || len(alias.RoutingConfiguration) != 2 || aws.ToString(alias.Description) != "production" {
		t.Fatalf("%+v %v", alias, err)
	}

	aliases, err := c.ListStateMachineAliases(ctx, &sfn.ListStateMachineAliasesInput{StateMachineArn: machine})
	if err != nil || len(aliases.StateMachineAliases) != 1 {
		t.Fatalf("%+v %v", aliases, err)
	}

	if _, err = c.DeleteStateMachineAlias(
		ctx,
		&sfn.DeleteStateMachineAliasInput{StateMachineAliasArn: live},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteStateMachineVersion(ctx, &sfn.DeleteStateMachineVersionInput{
		StateMachineVersionArn: v1,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{StateMachineArn: v1})
	code(t, err, "ResourceNotFound")
}
