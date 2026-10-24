package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	iamsdk "github.com/aws/aws-sdk-go-v2/service/iam"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	r53 "github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	sdk "github.com/aws/aws-sdk-go-v2/service/shield"
	"github.com/aws/aws-sdk-go-v2/service/shield/types"
	"github.com/aws/smithy-go"
)

func shieldClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func shieldError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func shieldEIP(t *testing.T, s *httptest.Server) string {
	t.Helper()
	c := ec2sdk.New(ec2sdk.Options{Region: "us-west-2", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	out, err := c.AllocateAddress(context.Background(), &ec2sdk.AllocateAddressInput{})
	if err != nil {
		t.Fatal(err)
	}

	return "arn:aws:ec2:us-west-2:000000000000:eip-allocation/" + *out.AllocationId
}

func TestShieldLifecycle(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := shieldClient(s)
	state, err := c.GetSubscriptionState(ctx, &sdk.GetSubscriptionStateInput{})
	if err != nil || state.SubscriptionState != types.SubscriptionStateInactive {
		t.Fatalf("inactive: %+v %v", state, err)
	}

	_, err = c.DescribeSubscription(ctx, &sdk.DescribeSubscriptionInput{})
	shieldError(t, err, "ResourceNotFoundException")
	_, err = c.ListProtections(ctx, &sdk.ListProtectionsInput{})
	shieldError(t, err, "ResourceNotFoundException")
	_, err = c.CreateSubscription(ctx, &sdk.CreateSubscriptionInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateSubscription(ctx, &sdk.CreateSubscriptionInput{})
	shieldError(t, err, "ResourceAlreadyExistsException")
	sub, err := c.DescribeSubscription(ctx, &sdk.DescribeSubscriptionInput{})
	if err != nil || sub.Subscription.StartTime == nil || sub.Subscription.EndTime == nil ||
		sub.Subscription.SubscriptionLimits == nil ||
		len(sub.Subscription.Limits) != 6 {
		t.Fatalf("subscription: %+v %v", sub, err)
	}

	if sub.Subscription.EndTime.Sub(*sub.Subscription.StartTime).
		Seconds() !=
		float64(
			sub.Subscription.TimeCommitmentInSeconds,
		) {
		t.Fatal("commitment mismatch")
	}

	_, err = c.UpdateSubscription(ctx, &sdk.UpdateSubscriptionInput{AutoRenew: types.AutoRenewDisabled})
	shieldError(t, err, "LockedSubscriptionException")
	_, err = c.UpdateSubscription(ctx, &sdk.UpdateSubscriptionInput{})
	if err != nil {
		t.Fatal(err)
	}

	//nolint:staticcheck // Verify the documented deprecated operation fails.
	_, err = c.DeleteSubscription(ctx, &sdk.DeleteSubscriptionInput{})
	shieldError(t, err, "LockedSubscriptionException")
	state, err = c.GetSubscriptionState(ctx, &sdk.GetSubscriptionStateInput{})
	if err != nil || state.SubscriptionState != types.SubscriptionStateActive {
		t.Fatalf("active: %+v %v", state, err)
	}

	resources := []string{shieldEIP(t, s), shieldEIP(t, s)}
	ids := []*string{}
	for i, value := range resources {
		out, createErr := c.CreateProtection(
			ctx,
			&sdk.CreateProtectionInput{
				Name:        new(fmt.Sprintf("eip-%d", i)),
				ResourceArn: new(value),
				Tags:        []types.Tag{{Key: new("team"), Value: new("core")}},
			},
		)
		if createErr != nil {
			t.Fatal(createErr)
		}

		ids = append(ids, out.ProtectionId)
	}

	_, err = c.CreateProtection(ctx, &sdk.CreateProtectionInput{Name: new("duplicate"), ResourceArn: new(resources[0])})
	shieldError(t, err, "ResourceAlreadyExistsException")
	_, err = c.CreateProtection(
		ctx,
		&sdk.CreateProtectionInput{
			Name:        new("missing"),
			ResourceArn: new("arn:aws:ec2:us-west-2:000000000000:eip-allocation/eipalloc-missing"),
		},
	)
	shieldError(t, err, "ResourceNotFoundException")
	_, err = c.CreateProtection(
		ctx,
		&sdk.CreateProtectionInput{Name: new("unsupported"), ResourceArn: new("arn:aws:s3:::bucket")},
	)
	shieldError(t, err, "InvalidParameterException")
	described, err := c.DescribeProtection(ctx, &sdk.DescribeProtectionInput{ResourceArn: new(resources[0])})
	if err != nil || *described.Protection.Id != *ids[0] {
		t.Fatalf("describe ARN: %+v %v", described, err)
	}

	_, err = c.DescribeProtection(ctx, &sdk.DescribeProtectionInput{})
	shieldError(t, err, "InvalidParameterException")
	first, err := c.ListProtections(ctx, &sdk.ListProtectionsInput{MaxResults: new(int32(1))})
	if err != nil || len(first.Protections) != 1 || first.NextToken == nil {
		t.Fatalf("page one: %+v %v", first, err)
	}

	second, err := c.ListProtections(
		ctx,
		&sdk.ListProtectionsInput{MaxResults: new(int32(1)), NextToken: first.NextToken},
	)
	if err != nil || len(second.Protections) != 1 || *second.Protections[0].Id == *first.Protections[0].Id ||
		second.NextToken != nil {
		t.Fatalf("page two: %+v %v", second, err)
	}

	filtered, err := c.ListProtections(
		ctx,
		&sdk.ListProtectionsInput{
			InclusionFilters: &types.InclusionProtectionFilters{
				ResourceArns:    resources[:1],
				ProtectionNames: []string{"eip-0"},
				ResourceTypes:   []types.ProtectedResourceType{types.ProtectedResourceTypeElasticIpAllocation},
			},
		},
	)
	if err != nil || len(filtered.Protections) != 1 {
		t.Fatalf("filter: %+v %v", filtered, err)
	}

	_, err = c.ListProtections(ctx, &sdk.ListProtectionsInput{NextToken: new("bogus")})
	shieldError(t, err, "InvalidParameterException")
	protectionARN := described.Protection.ProtectionArn
	tagged, tagErr := taggingClient(
		s,
		"us-east-1",
	).TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{*protectionARN},
		Tags:            map[string]string{"inventory": "yes"},
	})
	if tagErr != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("tagging integration: %+v %v", tagged, tagErr)
	}

	zero, zeroErr := c.ListProtections(ctx, &sdk.ListProtectionsInput{MaxResults: new(int32(0))})
	if zeroErr != nil || len(zero.Protections) != 2 {
		t.Fatalf("zero page size: %+v %v", zero, zeroErr)
	}

	shieldTags(t, c, protectionARN)
	for _, group := range []struct {
		id       string
		pattern  types.ProtectionGroupPattern
		resource types.ProtectedResourceType
		members  []string
	}{
		{"all", types.ProtectionGroupPatternAll, "", nil},
		{"type", types.ProtectionGroupPatternByResourceType, types.ProtectedResourceTypeElasticIpAllocation, nil},
		{"explicit", types.ProtectionGroupPatternArbitrary, "", resources[:1]},
	} {
		_, err = c.CreateProtectionGroup(
			ctx,
			&sdk.CreateProtectionGroupInput{
				ProtectionGroupId: new(group.id),
				Pattern:           group.pattern,
				Aggregation:       types.ProtectionGroupAggregationSum,
				ResourceType:      group.resource,
				Members:           group.members,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	group, err := c.DescribeProtectionGroup(ctx, &sdk.DescribeProtectionGroupInput{ProtectionGroupId: new("all")})
	if err != nil || len(group.ProtectionGroup.Members) != 2 {
		t.Fatalf("all membership: %+v %v", group, err)
	}

	shieldTags(t, c, group.ProtectionGroup.ProtectionGroupArn)
	groups, err := c.ListProtectionGroups(ctx, &sdk.ListProtectionGroupsInput{MaxResults: new(int32(1))})
	if err != nil || groups.NextToken == nil || len(groups.ProtectionGroups) != 1 {
		t.Fatalf("group page: %+v %v", groups, err)
	}

	groups, err = c.ListProtectionGroups(
		ctx,
		&sdk.ListProtectionGroupsInput{
			InclusionFilters: &types.InclusionProtectionGroupFilters{
				Patterns:           []types.ProtectionGroupPattern{types.ProtectionGroupPatternByResourceType},
				Aggregations:       []types.ProtectionGroupAggregation{types.ProtectionGroupAggregationSum},
				ResourceTypes:      []types.ProtectedResourceType{types.ProtectedResourceTypeElasticIpAllocation},
				ProtectionGroupIds: []string{"type"},
			},
		},
	)
	if err != nil || len(groups.ProtectionGroups) != 1 {
		t.Fatalf("group filter: %+v %v", groups, err)
	}

	members, err := c.ListResourcesInProtectionGroup(
		ctx,
		&sdk.ListResourcesInProtectionGroupInput{ProtectionGroupId: new("all"), MaxResults: new(int32(1))},
	)
	if err != nil || members.NextToken == nil || len(members.ResourceArns) != 1 {
		t.Fatalf("member page: %+v %v", members, err)
	}

	_, err = c.UpdateProtectionGroup(
		ctx,
		&sdk.UpdateProtectionGroupInput{
			ProtectionGroupId: new("explicit"),
			Pattern:           types.ProtectionGroupPatternArbitrary,
			Aggregation:       types.ProtectionGroupAggregationMax,
			Members:           resources,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateProtectionGroup(
		ctx,
		&sdk.CreateProtectionGroupInput{
			ProtectionGroupId: new("invalid"),
			Pattern:           types.ProtectionGroupPatternAll,
			Aggregation:       types.ProtectionGroupAggregationSum,
			Members:           resources,
		},
	)
	shieldError(t, err, "InvalidParameterException")
	route := r53.New(r53.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	health, err := route.CreateHealthCheck(
		ctx,
		&r53.CreateHealthCheckInput{
			CallerReference: new("shield"),
			HealthCheckConfig: &r53types.HealthCheckConfig{
				Type:         r53types.HealthCheckTypeHttp,
				IPAddress:    new("192.0.2.10"),
				Port:         new(int32(80)),
				ResourcePath: new("/"),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	healthARN := new("arn:aws:route53:::healthcheck/" + *health.HealthCheck.Id)
	_, err = c.AssociateHealthCheck(
		ctx,
		&sdk.AssociateHealthCheckInput{ProtectionId: ids[0], HealthCheckArn: healthARN},
	)
	if err != nil {
		t.Fatal(err)
	}

	described, err = c.DescribeProtection(ctx, &sdk.DescribeProtectionInput{ProtectionId: ids[0]})
	if err != nil || len(described.Protection.HealthCheckIds) != 1 {
		t.Fatalf("health: %+v %v", described, err)
	}

	_, err = c.DisassociateHealthCheck(
		ctx,
		&sdk.DisassociateHealthCheckInput{ProtectionId: ids[0], HealthCheckArn: healthARN},
	)
	if err != nil {
		t.Fatal(err)
	}

	attacks, err := c.ListAttacks(ctx, &sdk.ListAttacksInput{})
	if err != nil || len(attacks.AttackSummaries) != 0 {
		t.Fatalf("attacks: %+v %v", attacks, err)
	}

	_, err = c.DescribeAttack(ctx, &sdk.DescribeAttackInput{AttackId: new("00000000-0000-0000-0000-000000000000")})
	shieldError(t, err, "ResourceNotFoundException")
	stats, err := c.DescribeAttackStatistics(ctx, &sdk.DescribeAttackStatisticsInput{})
	if err != nil || len(stats.DataItems) != 0 || stats.TimeRange == nil {
		t.Fatalf("statistics: %+v %v", stats, err)
	}

	for _, id := range ids {
		if _, callErr := c.DeleteProtection(ctx, &sdk.DeleteProtectionInput{ProtectionId: id}); callErr != nil {
			t.Fatal(callErr)
		}
	}

	group, err = c.DescribeProtectionGroup(ctx, &sdk.DescribeProtectionGroupInput{ProtectionGroupId: new("all")})
	if err != nil || len(group.ProtectionGroup.Members) != 0 {
		t.Fatalf("dynamic members: %+v %v", group, err)
	}

	for _, id := range []string{"all", "type", "explicit"} {
		if _, callErr := c.DeleteProtectionGroup(
			ctx,
			&sdk.DeleteProtectionGroupInput{ProtectionGroupId: new(id)},
		); callErr != nil {
			t.Fatal(callErr)
		}
	}
}

func shieldTags(t *testing.T, c *sdk.Client, value *string) {
	t.Helper()
	ctx := context.Background()
	_, err := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceARN: value, Tags: []types.Tag{{Key: new("env"), Value: new("dev")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: value})
	if err != nil || len(tags.Tags) == 0 {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceARN: value, TagKeys: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestShieldSettingsAndSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := shieldClient(s)
	_, err := c.CreateSubscription(ctx, &sdk.CreateSubscriptionInput{})
	if err != nil {
		t.Fatal(err)
	}

	contacts := []types.EmergencyContact{
		{EmailAddress: new("srt@example.com"), PhoneNumber: new("+15550100"), ContactNotes: new("Primary")},
	}

	_, err = c.EnableProactiveEngagement(ctx, &sdk.EnableProactiveEngagementInput{})
	shieldError(t, err, "InvalidOperationException")
	_, err = c.AssociateProactiveEngagementDetails(
		ctx,
		&sdk.AssociateProactiveEngagementDetailsInput{EmergencyContactList: contacts},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DisableProactiveEngagement(ctx, &sdk.DisableProactiveEngagementInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateEmergencyContactSettings(
		ctx,
		&sdk.UpdateEmergencyContactSettingsInput{EmergencyContactList: contacts},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.EnableProactiveEngagement(ctx, &sdk.EnableProactiveEngagementInput{})
	if err != nil {
		t.Fatal(err)
	}

	iam := iamsdk.New(iamsdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	role, err := iam.CreateRole(
		ctx,
		&iamsdk.CreateRoleInput{
			RoleName: new("shield-srt"),
			AssumeRolePolicyDocument: new(
				`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Principal":{"Service":"drt.shield.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AssociateDRTRole(ctx, &sdk.AssociateDRTRoleInput{RoleArn: role.Role.Arn})
	if err != nil {
		t.Fatal(err)
	}

	buckets := s3sdk.New(
		s3sdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	for i := range 11 {
		name := fmt.Sprintf("shield-logs-%02d", i)
		if _, callErr := buckets.CreateBucket(ctx, &s3sdk.CreateBucketInput{Bucket: new(name)}); callErr != nil {
			t.Fatal(callErr)
		}

		_, err = c.AssociateDRTLogBucket(ctx, &sdk.AssociateDRTLogBucketInput{LogBucket: new(name)})
		if i == 10 {
			shieldError(t, err, "LimitsExceededException")
		} else if err != nil {
			t.Fatal(err)
		}
	}

	resource := shieldEIP(t, s)
	p, err := c.CreateProtection(ctx, &sdk.CreateProtectionInput{Name: new("persistent"), ResourceArn: new(resource)})
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = shieldClient(s)
	drt, err := c.DescribeDRTAccess(ctx, &sdk.DescribeDRTAccessInput{})
	if err != nil || len(drt.LogBucketList) != 10 || *drt.RoleArn != *role.Role.Arn {
		t.Fatalf("persist DRT: %+v %v", drt, err)
	}

	emergency, err := c.DescribeEmergencyContactSettings(ctx, &sdk.DescribeEmergencyContactSettingsInput{})
	if err != nil || len(emergency.EmergencyContactList) != 1 {
		t.Fatalf("persist contacts: %+v %v", emergency, err)
	}

	global := sdk.New(sdk.Options{Region: "eu-west-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	if _, callErr := global.DescribeProtection(
		ctx,
		&sdk.DescribeProtectionInput{ProtectionId: p.ProtectionId},
	); callErr != nil {
		t.Fatal(callErr)
	}

	_, err = c.DisassociateDRTLogBucket(ctx, &sdk.DisassociateDRTLogBucketInput{LogBucket: new("shield-logs-00")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DisassociateDRTRole(ctx, &sdk.DisassociateDRTRoleInput{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestShieldCloudFormation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := shieldClient(s)
	if _, callErr := c.CreateSubscription(ctx, &sdk.CreateSubscriptionInput{}); callErr != nil {
		t.Fatal(callErr)
	}

	resource := shieldEIP(t, s)
	iam := iamsdk.New(iamsdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	role, err := iam.CreateRole(
		ctx,
		&iamsdk.CreateRoleInput{
			RoleName: new("cf-shield-role"),
			AssumeRolePolicyDocument: new(
				`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Principal":{"Service":"drt.shield.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	cf := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := func(aggregation string) string {
		body, marshalErr := json.Marshal(map[string]any{"Resources": map[string]any{
			"DRT": map[string]any{
				"Type":       "AWS::Shield::DRTAccess",
				"Properties": map[string]any{"RoleArn": *role.Role.Arn},
			},
			"Protection": map[string]any{
				"Type": "AWS::Shield::Protection",
				"Properties": map[string]any{
					"Name":        "cf-eip",
					"ResourceArn": resource,
					"Tags":        map[string]string{"team": aggregation},
				},
			},
			"Group": map[string]any{
				"Type":      "AWS::Shield::ProtectionGroup",
				"DependsOn": "Protection",
				"Properties": map[string]any{
					"ProtectionGroupId": "cf-group",
					"Pattern":           "ALL",
					"Aggregation":       aggregation,
				},
			},
			"Proactive": map[string]any{
				"Type": "AWS::Shield::ProactiveEngagement",
				"Properties": map[string]any{
					"ProactiveEngagementStatus": "ENABLED",
					"EmergencyContactList": []any{
						map[string]any{"EmailAddress": "srt@example.com", "PhoneNumber": "+15550100"},
					},
				},
			},
		}})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}

		return string(body)
	}

	stack, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("shield-stack"), TemplateBody: new(template("SUM"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, "shield-stack", cfntypes.StackStatusCreateComplete)
	before, err := cf.DescribeStackResource(
		ctx,
		&cfn.DescribeStackResourceInput{StackName: new("shield-stack"), LogicalResourceId: new("Protection")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, callErr := cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("shield-stack"), TemplateBody: new(template("MAX"))},
	); callErr != nil {
		t.Fatal(callErr)
	}

	wantStatus(ctx, t, cf, "shield-stack", cfntypes.StackStatusUpdateComplete)
	after, err := cf.DescribeStackResource(
		ctx,
		&cfn.DescribeStackResourceInput{StackName: new("shield-stack"), LogicalResourceId: new("Protection")},
	)
	if err != nil || *after.StackResourceDetail.PhysicalResourceId != *before.StackResourceDetail.PhysicalResourceId {
		t.Fatalf("mutable update replaced protection: %+v %v", after, err)
	}

	group, err := c.DescribeProtectionGroup(ctx, &sdk.DescribeProtectionGroupInput{ProtectionGroupId: new("cf-group")})
	if err != nil || group.ProtectionGroup.Aggregation != types.ProtectionGroupAggregationMax {
		t.Fatalf("group update: %+v %v", group, err)
	}

	if _, callErr := cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("shield-stack")}); callErr != nil {
		t.Fatal(callErr)
	}

	wantStatus(ctx, t, cf, *stack.StackId, cfntypes.StackStatusDeleteComplete)
}

func TestShieldModelValidation(t *testing.T) {
	c := shieldClient(server(t))
	ctx := context.Background()
	_, err := c.ListProtections(ctx, &sdk.ListProtectionsInput{MaxResults: new(int32(-1))})
	shieldError(t, err, "InvalidParameterException")
	_, err = c.AssociateDRTLogBucket(ctx, &sdk.AssociateDRTLogBucketInput{LogBucket: new("INVALID_BUCKET")})
	shieldError(t, err, "InvalidParameterException")
	_, err = c.CreateProtectionGroup(
		ctx,
		&sdk.CreateProtectionGroupInput{
			ProtectionGroupId: new("group"),
			Aggregation:       types.ProtectionGroupAggregation("INVALID"),
			Pattern:           types.ProtectionGroupPatternAll,
		},
	)
	shieldError(t, err, "InvalidParameterException")
}
