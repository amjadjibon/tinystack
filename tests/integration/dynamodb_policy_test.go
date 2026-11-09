package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestDynamoDBResourcePolicies(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	table, err := create(ctx, c, "policies")
	mustAuthorize(t, err)
	_, err = c.UpdateTable(
		ctx,
		&sdk.UpdateTableInput{
			TableName: new("policies"),
			StreamSpecification: &types.StreamSpecification{
				StreamEnabled:  new(true),
				StreamViewType: types.StreamViewTypeNewImage,
			},
		},
	)
	mustAuthorize(t, err)
	described, err := c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: new("policies")})
	mustAuthorize(t, err)
	for _, arn := range []string{*table.TableDescription.TableArn, *described.Table.LatestStreamArn} {
		doc := fmt.Sprintf(
			`{"Statement":{"Effect":"Allow","Principal":{"AWS":"111111111111"},"Action":"dynamodb:GetItem","Resource":%q}}`,
			arn,
		)
		put, putErr := c.PutResourcePolicy(
			ctx,
			&sdk.PutResourcePolicyInput{
				ResourceArn:                     &arn,
				Policy:                          &doc,
				ExpectedRevisionId:              new("NO_POLICY"),
				ConfirmRemoveSelfResourceAccess: true,
			},
		)
		mustAuthorize(t, putErr)
		got, getErr := c.GetResourcePolicy(ctx, &sdk.GetResourcePolicyInput{ResourceArn: &arn})
		mustAuthorize(t, getErr)
		if *got.Policy != doc || *got.RevisionId != *put.RevisionId {
			t.Fatal(got)
		}
		_, err = c.PutResourcePolicy(
			ctx,
			&sdk.PutResourcePolicyInput{ResourceArn: &arn, Policy: &doc, ExpectedRevisionId: new("wrong")},
		)
		wantCode(t, err, "PolicyNotFoundException")
		_, err = c.DeleteResourcePolicy(
			ctx,
			&sdk.DeleteResourcePolicyInput{ResourceArn: &arn, ExpectedRevisionId: new("wrong")},
		)
		wantCode(t, err, "PolicyNotFoundException")
		_, err = c.DeleteResourcePolicy(
			ctx,
			&sdk.DeleteResourcePolicyInput{ResourceArn: &arn, ExpectedRevisionId: put.RevisionId},
		)
		mustAuthorize(t, err)
		_, err = c.GetResourcePolicy(ctx, &sdk.GetResourcePolicyInput{ResourceArn: &arn})
		wantCode(t, err, "PolicyNotFoundException")
	}
}

func TestDynamoDBCrossAccountPolicy(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test", "other": "test"}
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	owner := client(s, "other", "us-east-1")
	table, err := create(ctx, owner, "shared")
	mustAuthorize(t, err)
	arn := *table.TableDescription.TableArn
	key := authorizationUser(t, s)
	authorizationPolicy(
		t,
		s,
		fmt.Sprintf(`{"Statement":{"Effect":"Allow","Action":"dynamodb:GetItem","Resource":%q}}`, arn),
	)
	user := sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  authorizationProvider(key),
			Retryer:      aws.NopRetryer{},
		},
	)
	input := &sdk.GetItemInput{
		TableName: &arn,
		Key:       map[string]types.AttributeValue{"PK": &types.AttributeValueMemberS{Value: "item"}},
	}
	_, err = user.GetItem(ctx, input)
	wantCode(t, err, "AccessDeniedException")
	for _, effect := range []string{"Allow", "Deny"} {
		doc := fmt.Sprintf(
			`{"Statement":{"Effect":%q,"Principal":{"AWS":%q},"Action":"dynamodb:GetItem","Resource":%q}}`,
			effect,
			cfg.Defaults.Account,
			arn,
		)
		_, err = owner.PutResourcePolicy(ctx, &sdk.PutResourcePolicyInput{ResourceArn: &arn, Policy: &doc})
		mustAuthorize(t, err)
		_, err = user.GetItem(ctx, input)
		if effect == "Allow" {
			mustAuthorize(t, err)
		} else {
			wantCode(t, err, "AccessDeniedException")
		}
	}
}

func TestDynamoDBCreateTableByARN(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	probe, err := create(ctx, c, "probe")
	mustAuthorize(t, err)
	own := strings.Replace(*probe.TableDescription.TableArn, "table/probe", "table/by-arn", 1)
	table, err := create(ctx, c, own)
	mustAuthorize(t, err)
	if got := *table.TableDescription.TableName; got != "by-arn" {
		t.Fatalf("TableName = %q, want by-arn", got)
	}

	if got := *table.TableDescription.TableArn; got != own {
		t.Fatalf("TableArn = %q, want %q", got, own)
	}

	foreign := strings.Replace(own, ":"+strings.Split(own, ":")[4]+":", ":222222222222:", 1)
	_, err = create(ctx, c, foreign)
	code(t, err, "ResourceNotFoundException")
	_, err = c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: new(foreign)})
	code(t, err, "ResourceNotFoundException")
}
