package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/amplify"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

func amplifyTemplate(team string) string {
	return fmt.Sprintf(`{
 "Resources":{
 "App":{"Type":"AWS::Amplify::App","Properties":{"Name":"hosting","BuildSpec":"version: 1",
 "EnvironmentVariables":[{"Name":"TEAM","Value":%q}],"Tags":[{"Key":"team","Value":%q}],
 "BasicAuthConfig":{"EnableBasicAuth":true,"Username":"user","Password":"secret"}}},
 "Branch":{"Type":"AWS::Amplify::Branch","Properties":{"AppId":{"Fn::GetAtt":["App","AppId"]},
 "BranchName":"main","Stage":"DEVELOPMENT","EnvironmentVariables":[{"Name":"TEAM","Value":%q}]}},
 "Domain":{"Type":"AWS::Amplify::Domain","Properties":{"AppId":{"Fn::GetAtt":["App","AppId"]},
 "DomainName":"example.com","SubDomainSettings":[{"Prefix":"www","BranchName":{"Fn::GetAtt":["Branch","BranchName"]}}]}}
 },
 "Outputs":{"AppId":{"Value":{"Fn::GetAtt":["App","AppId"]}},
 "DomainState":{"Value":{"Fn::GetAtt":["Domain","DomainStatus"]}},
 "DefaultDomain":{"Value":{"Fn::GetAtt":["App","DefaultDomain"]}}}
 }`, team, team, team)
}

func TestCloudFormationAmplify(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := amplifyClient(s, "test", "us-east-1")
	_, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("hosting"), TemplateBody: new(amplifyTemplate("core"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, cf, "hosting", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&stack)
	id := new(outputs["AppId"])
	if outputs["DomainState"] != "PENDING_VERIFICATION" || outputs["DefaultDomain"] != *id+".amplifyapp.com" {
		t.Fatal(outputs)
	}

	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("hosting"), TemplateBody: new(amplifyTemplate("platform"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, "hosting", cfntypes.StackStatusUpdateComplete)
	app, err := c.GetApp(ctx, &sdk.GetAppInput{AppId: id})
	if err != nil || app.App.EnvironmentVariables["TEAM"] != "platform" || app.App.Tags["team"] != "platform" ||
		!aws.ToBool(app.App.EnableBasicAuth) || app.App.BasicAuthCredentials != nil {
		t.Fatal(app, err)
	}

	b, err := c.GetBranch(ctx, &sdk.GetBranchInput{AppId: id, BranchName: new("main")})
	if err != nil || b.Branch.EnvironmentVariables["TEAM"] != "platform" {
		t.Fatal(b, err)
	}

	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("hosting")})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *stack.StackId, cfntypes.StackStatusDeleteComplete)
	_, err = c.GetApp(ctx, &sdk.GetAppInput{AppId: id})
	amplifyError(t, err, "NotFoundException")
}
