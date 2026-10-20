package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/opensearchserverless"
	"github.com/aws/aws-sdk-go-v2/service/opensearchserverless/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func aossClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantAOSSError(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

const (
	aossEncryption = `{"Rules":[{"ResourceType":"collection","Resource":["collection/analytics*"]}],"AWSOwnedKey":true}`
	aossNetwork    = `[{
"Rules":[{"ResourceType":"collection","Resource":["collection/analytics*"]},
{"ResourceType":"dashboard","Resource":["collection/analytics*"]}],
"AllowFromPublic":true}]`
	aossAccess = `[{
"Rules":[{"ResourceType":"collection","Resource":["collection/analytics*"],"Permission":["aoss:*"]},
{"ResourceType":"index","Resource":["index/analytics*/*"],"Permission":["aoss:ReadDocument"]}],
"Principal":["arn:aws:iam::000000000000:root"]}]`
)

func TestOpenSearchServerlessCollections(t *testing.T) {
	s := server(t)
	c := aossClient(s, "test", "us-east-1")
	ctx := context.Background()
	input := &sdk.CreateCollectionInput{
		Name: new("analytics"), Type: types.CollectionTypeVectorsearch,
		StandbyReplicas: types.StandbyReplicasDisabled, ClientToken: new("collection-token"),
		Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
	}

	_, err := c.CreateCollection(ctx, input)
	wantAOSSError(t, err, "ValidationException")
	_, err = c.CreateSecurityPolicy(
		ctx,
		&sdk.CreateSecurityPolicyInput{
			Name:   new("encrypt"),
			Type:   types.SecurityPolicyTypeEncryption,
			Policy: new(aossEncryption),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	created, err := c.CreateCollection(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	d := created.CreateCollectionDetail
	if d.Status != types.CollectionStatusCreating || d.Type != types.CollectionTypeVectorsearch ||
		d.StandbyReplicas != types.StandbyReplicasDisabled {
		t.Fatal(d)
	}

	again, err := c.CreateCollection(ctx, input)
	if err != nil || aws.ToString(again.CreateCollectionDetail.Id) != aws.ToString(d.Id) {
		t.Fatal(again, err)
	}

	input.Description = new("different")
	_, err = c.CreateCollection(ctx, input)
	wantAOSSError(t, err, "ConflictException")
	get := &sdk.BatchGetCollectionInput{Ids: []string{*d.Id}}
	first, err := c.BatchGetCollection(ctx, get)
	if err != nil || len(first.CollectionDetails) != 1 ||
		first.CollectionDetails[0].Status != types.CollectionStatusCreating {
		t.Fatal(first, err)
	}

	active, err := c.BatchGetCollection(ctx, get)
	if err != nil || active.CollectionDetails[0].Status != types.CollectionStatusActive {
		t.Fatal(active, err)
	}

	if got := aws.ToString(
		active.CollectionDetails[0].CollectionEndpoint,
	); got != "https://"+*d.Id+".us-east-1.aoss.amazonaws.com" {
		t.Fatal(got)
	}

	if !strings.HasSuffix(aws.ToString(active.CollectionDetails[0].DashboardEndpoint), "/_dashboards") {
		t.Fatal(active)
	}

	updated, err := c.UpdateCollection(
		ctx,
		&sdk.UpdateCollectionInput{Id: d.Id, Description: new("updated"), ClientToken: new("update")},
	)
	if err != nil || aws.ToString(updated.UpdateCollectionDetail.Description) != "updated" {
		t.Fatal(updated, err)
	}

	list, err := c.ListCollections(
		ctx,
		&sdk.ListCollectionsInput{
			CollectionFilters: &types.CollectionFilters{Name: new("analytics"), Status: types.CollectionStatusActive},
		},
	)
	if err != nil || len(list.CollectionSummaries) != 1 {
		t.Fatal(list, err)
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: d.Arn, Tags: []types.Tag{{Key: new("stage"), Value: new("dev")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: d.Arn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	tagger := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagger.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*d.Arn}, Tags: map[string]string{"via": "rgta"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("%+v %v", tagged.FailedResourcesMap, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: d.Arn, TagKeys: []string{"stage"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, other := range []*sdk.Client{aossClient(s, "team-b", "us-east-1"), aossClient(s, "test", "eu-west-1")} {
		result, e := other.BatchGetCollection(ctx, get)
		if e != nil || len(result.CollectionDetails) != 0 || len(result.CollectionErrorDetails) != 1 {
			t.Fatal(result, e)
		}

		_, e = other.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: d.Arn})
		wantAOSSError(t, e, "ResourceNotFoundException")
		_, e = other.GetSecurityPolicy(
			ctx,
			&sdk.GetSecurityPolicyInput{Name: new("encrypt"), Type: types.SecurityPolicyTypeEncryption},
		)
		wantAOSSError(t, e, "ResourceNotFoundException")
	}

	deleted, err := c.DeleteCollection(ctx, &sdk.DeleteCollectionInput{Id: d.Id, ClientToken: new("delete")})
	if err != nil || deleted.DeleteCollectionDetail.Status != types.CollectionStatusDeleting {
		t.Fatal(deleted, err)
	}

	deleting, err := c.BatchGetCollection(ctx, get)
	if err != nil || deleting.CollectionDetails[0].Status != types.CollectionStatusDeleting {
		t.Fatal(deleting, err)
	}

	gone, err := c.BatchGetCollection(ctx, get)
	if err != nil || len(gone.CollectionDetails) != 0 {
		t.Fatal(gone, err)
	}

	replay, err := c.CreateCollection(ctx, &sdk.CreateCollectionInput{
		Name:            new("analytics"),
		Type:            types.CollectionTypeVectorsearch,
		StandbyReplicas: types.StandbyReplicasDisabled,
		ClientToken:     new("collection-token"),
		Tags:            []types.Tag{{Key: new("team"), Value: new("core")}},
	})
	if err != nil || aws.ToString(replay.CreateCollectionDetail.Id) != *d.Id {
		t.Fatal(replay, err)
	}
}

func TestOpenSearchServerlessPolicies(t *testing.T) {
	s := server(t)
	c := aossClient(s, "test", "us-east-1")
	ctx := context.Background()
	for _, kind := range []types.SecurityPolicyType{types.SecurityPolicyTypeEncryption, types.SecurityPolicyTypeNetwork} {
		text := aossEncryption
		if kind == types.SecurityPolicyTypeNetwork {
			text = aossNetwork
		}

		in := &sdk.CreateSecurityPolicyInput{
			Name:        new("policy"),
			Type:        kind,
			Policy:      new(text),
			ClientToken: new("create-" + string(kind)),
		}

		created, err := c.CreateSecurityPolicy(ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		var document any
		if docErr := created.SecurityPolicyDetail.Policy.UnmarshalSmithyDocument(&document); docErr != nil {
			t.Fatal(docErr)
		}

		if document == nil {
			t.Fatal("missing document")
		}

		_, err = c.CreateSecurityPolicy(ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetSecurityPolicy(ctx, &sdk.GetSecurityPolicyInput{Name: in.Name, Type: kind})
		if err != nil {
			t.Fatal(err)
		}

		update := &sdk.UpdateSecurityPolicyInput{
			Name:          in.Name,
			Type:          kind,
			PolicyVersion: got.SecurityPolicyDetail.PolicyVersion,
			Description:   new("changed"),
			Policy:        new(text),
			ClientToken:   new("update-" + string(kind)),
		}

		changed, err := c.UpdateSecurityPolicy(ctx, update)
		if err != nil || *changed.SecurityPolicyDetail.PolicyVersion == *got.SecurityPolicyDetail.PolicyVersion {
			t.Fatal(changed, err)
		}

		update.ClientToken = new("stale-" + string(kind))
		_, err = c.UpdateSecurityPolicy(ctx, update)
		wantAOSSError(t, err, "ConflictException")
		list, err := c.ListSecurityPolicies(
			ctx,
			&sdk.ListSecurityPoliciesInput{Type: kind, Resource: []string{"collection/analytics"}},
		)
		if err != nil || len(list.SecurityPolicySummaries) != 1 {
			t.Fatal(list, err)
		}
	}

	in := &sdk.CreateAccessPolicyInput{
		Name:        new("data-policy"),
		Type:        types.AccessPolicyTypeData,
		Policy:      new(aossAccess),
		ClientToken: new("access"),
	}

	created, err := c.CreateAccessPolicy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetAccessPolicy(ctx, &sdk.GetAccessPolicyInput{Name: in.Name, Type: in.Type})
	if err != nil || *got.AccessPolicyDetail.PolicyVersion != *created.AccessPolicyDetail.PolicyVersion {
		t.Fatal(got, err)
	}

	changed, err := c.UpdateAccessPolicy(
		ctx,
		&sdk.UpdateAccessPolicyInput{
			Name:          in.Name,
			Type:          in.Type,
			PolicyVersion: got.AccessPolicyDetail.PolicyVersion,
			Description:   new("updated"),
		},
	)
	if err != nil || aws.ToString(changed.AccessPolicyDetail.Description) != "updated" {
		t.Fatal(changed, err)
	}

	list, err := c.ListAccessPolicies(ctx, &sdk.ListAccessPoliciesInput{Type: in.Type})
	if err != nil || len(list.AccessPolicySummaries) != 1 {
		t.Fatal(list, err)
	}

	stats, err := c.GetPoliciesStats(ctx, &sdk.GetPoliciesStatsInput{})
	if err != nil || aws.ToInt64(stats.TotalPolicyCount) != 3 {
		t.Fatal(stats, err)
	}

	_, err = c.DeleteAccessPolicy(ctx, &sdk.DeleteAccessPolicyInput{Name: in.Name, Type: in.Type})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetAccessPolicy(ctx, &sdk.GetAccessPolicyInput{Name: in.Name, Type: in.Type})
	wantAOSSError(t, err, "ResourceNotFoundException")
	for _, kind := range []types.SecurityPolicyType{types.SecurityPolicyTypeEncryption, types.SecurityPolicyTypeNetwork} {
		_, err = c.DeleteSecurityPolicy(ctx, &sdk.DeleteSecurityPolicyInput{Name: new("policy"), Type: kind})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenSearchServerlessValidationAndPagination(t *testing.T) {
	s := server(t)
	c := aossClient(s, "test", "us-east-1")
	ctx := context.Background()
	for _, bad := range []string{
		`{}`, `null`, `{"Rules":[]}`,
		`{"Rules":[{"ResourceType":"index","Resource":["index/x/*"]}],"AWSOwnedKey":true}`, aossEncryption + `{}`,
	} {
		_, err := c.CreateSecurityPolicy(
			ctx,
			&sdk.CreateSecurityPolicyInput{
				Name:   new("invalid"),
				Type:   types.SecurityPolicyTypeEncryption,
				Policy: new(bad),
			},
		)
		wantAOSSError(t, err, "ValidationException")
	}

	for _, bad := range []string{
		`{}`, `[]`, `[{"Rules":[],
"Principal":[]}]`, strings.ReplaceAll(aossAccess, "aoss:ReadDocument", "aoss:Invalid"),
	} {
		_, err := c.CreateAccessPolicy(
			ctx,
			&sdk.CreateAccessPolicyInput{Name: new("invalid"), Type: types.AccessPolicyTypeData, Policy: new(bad)},
		)
		wantAOSSError(t, err, "ValidationException")
	}

	for _, name := range []string{"policy-one", "policy-two"} {
		_, err := c.CreateSecurityPolicy(
			ctx,
			&sdk.CreateSecurityPolicyInput{
				Name:   new(name),
				Type:   types.SecurityPolicyTypeEncryption,
				Policy: new(aossEncryption),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	page, err := c.ListSecurityPolicies(
		ctx,
		&sdk.ListSecurityPoliciesInput{Type: types.SecurityPolicyTypeEncryption, MaxResults: new(int32(1))},
	)
	if err != nil || len(page.SecurityPolicySummaries) != 1 || page.NextToken == nil {
		t.Fatal(page, err)
	}

	next, err := c.ListSecurityPolicies(
		ctx,
		&sdk.ListSecurityPoliciesInput{
			Type:       types.SecurityPolicyTypeEncryption,
			MaxResults: new(int32(1)),
			NextToken:  page.NextToken,
		},
	)
	if err != nil || len(next.SecurityPolicySummaries) != 1 || next.NextToken != nil {
		t.Fatal(next, err)
	}

	_, err = c.ListCollections(ctx, &sdk.ListCollectionsInput{NextToken: new("!invalid")})
	wantAOSSError(t, err, "ValidationException")
	for _, kind := range []types.CollectionType{types.CollectionTypeSearch, types.CollectionTypeTimeseries} {
		_, err = c.CreateCollection(
			ctx,
			&sdk.CreateCollectionInput{Name: new("analytics-" + strings.ToLower(string(kind))), Type: kind},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateCollection(ctx, &sdk.CreateCollectionInput{Name: new("Bad_Name")})
	wantAOSSError(t, err, "ValidationException")
	_, err = c.CreateCollection(
		ctx,
		&sdk.CreateCollectionInput{Name: new("analytics-bad"), Type: types.CollectionType("bad")},
	)
	wantAOSSError(t, err, "ValidationException")
	_, err = c.CreateCollection(
		ctx,
		&sdk.CreateCollectionInput{
			Name: new("analytics-bad"),
			Tags: []types.Tag{{Key: new("aws:bad"), Value: new("x")}},
		},
	)
	wantAOSSError(t, err, "ValidationException")
	settings, err := c.UpdateAccountSettings(
		ctx,
		&sdk.UpdateAccountSettingsInput{CapacityLimits: &types.CapacityLimits{MaxSearchCapacityInOCU: new(int32(20))}},
	)
	if err != nil || aws.ToInt32(settings.AccountSettingsDetail.CapacityLimits.MaxSearchCapacityInOCU) != 20 {
		t.Fatal(settings, err)
	}

	settingsGet, err := c.GetAccountSettings(ctx, &sdk.GetAccountSettingsInput{})
	if err != nil || aws.ToInt32(settingsGet.AccountSettingsDetail.CapacityLimits.MaxSearchCapacityInOCU) != 20 {
		t.Fatal(settingsGet, err)
	}

	raw, _ := json.Marshal(settingsGet.AccountSettingsDetail)
	if !strings.Contains(string(raw), "20") {
		t.Fatal(string(raw))
	}
}
