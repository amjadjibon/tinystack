package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroups"
	rgtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroups/types"
	tagging "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func groupsClient(s *httptest.Server, key, region string) *resourcegroups.Client {
	return resourcegroups.New(resourcegroups.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: key}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// tagQuery is a TAG_FILTERS_1_0 resource query.
func tagQuery(query string) *rgtypes.ResourceQuery {
	return &rgtypes.ResourceQuery{Type: rgtypes.QueryTypeTagFilters10, Query: aws.String(query)}
}

// memberItems returns the ARNs of ListGroupResources items.
func memberItems(items []rgtypes.ListGroupResourcesItem) []string {
	ids := make([]rgtypes.ResourceIdentifier, 0, len(items))
	for _, item := range items {
		ids = append(ids, *item.Identifier)
	}

	return memberARNs(ids)
}

// memberARNs returns the ARNs of resource identifiers, sorted.
func memberARNs(ids []rgtypes.ResourceIdentifier) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, aws.ToString(id.ResourceArn))
	}

	slices.Sort(out)

	return out
}

func TestResourceGroupsQueries(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	r := createTaggedResources(t, s)
	g := groupsClient(s, "test", "us-east-1")

	const prod = `{"ResourceTypeFilters":["AWS::AllSupported"],"TagFilters":[{"Key":"env","Values":["prod"]}]}`
	created, err := g.CreateGroup(ctx, &resourcegroups.CreateGroupInput{
		Name: aws.String("prod"), Description: aws.String("production"), ResourceQuery: tagQuery(prod),
		Tags: map[string]string{"owner": "me"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(created.Group.GroupArn) != "arn:aws:resource-groups:us-east-1:000000000000:group/prod" ||
		created.Tags["owner"] != "me" || aws.ToString(created.ResourceQuery.Query) != prod {
		t.Fatalf("created group: %+v", created)
	}

	members, err := g.ListGroupResources(ctx, &resourcegroups.ListGroupResourcesInput{Group: aws.String("prod")})
	if err != nil {
		t.Fatal(err)
	}

	// The Region's tagged prod resources: the other Region's table is not here.
	want := []string{r.bucket, r.role, r.table}
	slices.Sort(want)
	if got := memberItems(members.Resources); !slices.Equal(got, want) {
		t.Fatalf("members = %v, want %v", got, want)
	}

	if len(members.Resources) != len(want) || len(members.QueryErrors) != 0 {
		t.Fatalf("members: %+v", members)
	}

	types := map[string]string{}
	for _, item := range members.Resources {
		id := item.Identifier
		types[aws.ToString(id.ResourceArn)] = aws.ToString(id.ResourceType)
	}

	if types[r.bucket] != "AWS::S3::Bucket" || types[r.table] != "AWS::DynamoDB::Table" ||
		types[r.role] != "AWS::IAM::Role" {
		t.Fatalf("types = %v", types)
	}

	// A request filter narrows the group's resources by type.
	filtered, err := g.ListGroupResources(ctx, &resourcegroups.ListGroupResourcesInput{
		Group: aws.String("prod"),
		Filters: []rgtypes.ResourceFilter{
			{Name: rgtypes.ResourceFilterNameResourceType, Values: []string{"AWS::S3::Bucket"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := memberItems(filtered.Resources); !slices.Equal(got, []string{r.bucket}) {
		t.Fatalf("filtered members = %v", got)
	}

	// Searching runs the same query without a group, and pages.
	found, err := g.SearchResources(ctx, &resourcegroups.SearchResourcesInput{
		ResourceQuery: tagQuery(`{"ResourceTypeFilters":["AWS::SQS::Queue"],"TagFilters":[{"Key":"env"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := memberARNs(found.ResourceIdentifiers); !slices.Equal(got, []string{r.queue}) {
		t.Fatalf("search = %v", got)
	}

	var paged []string
	var token *string
	for {
		page, pageErr := g.SearchResources(ctx, &resourcegroups.SearchResourcesInput{
			ResourceQuery: tagQuery(prod), MaxResults: aws.Int32(3), NextToken: token,
		})
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		if len(page.ResourceIdentifiers) > 3 {
			t.Fatalf("page of %d", len(page.ResourceIdentifiers))
		}

		paged = append(paged, memberARNs(page.ResourceIdentifiers)...)
		if token = page.NextToken; token == nil {
			break
		}
	}

	slices.Sort(paged)
	if !slices.Equal(paged, want) {
		t.Fatalf("paged = %v, want %v", paged, want)
	}

	// A type no service implements is reported, not failed.
	unknown, err := g.SearchResources(ctx, &resourcegroups.SearchResourcesInput{
		ResourceQuery: tagQuery(`{"ResourceTypeFilters":["AWS::Nope::Thing"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(unknown.QueryErrors) != 1 ||
		unknown.QueryErrors[0].ErrorCode != rgtypes.QueryErrorCodeResourceTypeNotSupported {
		t.Fatalf("query errors: %+v", unknown.QueryErrors)
	}

	// Another Region and another account see none of it.
	for name, client := range map[string]*resourcegroups.Client{
		"region":  groupsClient(s, "test", "eu-west-1"),
		"account": groupsClient(s, "team-b", "us-east-1"),
	} {
		other, searchErr := client.SearchResources(
			ctx,
			&resourcegroups.SearchResourcesInput{ResourceQuery: tagQuery(prod)},
		)
		if searchErr != nil {
			t.Fatal(searchErr)
		}

		if name == "region" {
			// The table created in eu-west-1 is the only prod resource there.
			if len(other.ResourceIdentifiers) != 1 ||
				aws.ToString(other.ResourceIdentifiers[0].ResourceType) != "AWS::DynamoDB::Table" {
				t.Fatalf("%s sees %v", name, other.ResourceIdentifiers)
			}

			continue
		}

		if len(other.ResourceIdentifiers) != 0 {
			t.Fatalf("%s sees %v", name, other.ResourceIdentifiers)
		}

		_, getErr := client.GetGroup(ctx, &resourcegroups.GetGroupInput{Group: aws.String("prod")})
		var missing *rgtypes.NotFoundException
		if !errors.As(getErr, &missing) {
			t.Fatalf("other account's GetGroup: %v", getErr)
		}
	}
}

func TestResourceGroupsLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	g := groupsClient(s, "test", "us-east-1")

	const query = `{"ResourceTypeFilters":["AWS::EC2::Instance"],"TagFilters":[{"Key":"stage","Values":["test"]}]}`
	if _, err := g.CreateGroup(ctx, &resourcegroups.CreateGroupInput{
		Name: aws.String("alpha"), ResourceQuery: tagQuery(query), Owner: aws.String("team-a"),
		Criticality: aws.Int32(3),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := g.CreateGroup(ctx, &resourcegroups.CreateGroupInput{
		Name: aws.String(
			"beta",
		),
		Configuration: []rgtypes.GroupConfigurationItem{{Type: aws.String("AWS::ResourceGroups::Generic")}},
	}); err != nil {
		t.Fatal(err)
	}

	// Duplicates, bad names, and groups with neither a query nor a configuration fail.
	for name, in := range map[string]*resourcegroups.CreateGroupInput{
		"duplicate":  {Name: aws.String("alpha"), ResourceQuery: tagQuery(query)},
		"aws prefix": {Name: aws.String("aws-thing"), ResourceQuery: tagQuery(query)},
		"characters": {Name: aws.String("a b"), ResourceQuery: tagQuery(query)},
		"empty":      {Name: aws.String("gamma")},
		"bad query":  {Name: aws.String("gamma"), ResourceQuery: tagQuery(`{"Nope":1}`)},
		"bad type": {
			Name:          aws.String("gamma"),
			ResourceQuery: &rgtypes.ResourceQuery{Type: "OTHER", Query: aws.String("{}")},
		},
	} {
		_, err := g.CreateGroup(ctx, in)
		var bad *rgtypes.BadRequestException
		if !errors.As(err, &bad) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}

	list, err := g.ListGroups(ctx, &resourcegroups.ListGroupsInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(list.GroupIdentifiers) != 2 || aws.ToString(list.GroupIdentifiers[0].GroupName) != "alpha" {
		t.Fatalf("groups: %+v", list.GroupIdentifiers)
	}

	for filter, values := range map[string][]string{
		"resource-type":      {"AWS::EC2::Instance"},
		"owner":              {"team-a"},
		"criticality":        {"3"},
		"configuration-type": {"AWS::ResourceGroups::Generic"},
	} {
		got, listErr := g.ListGroups(ctx, &resourcegroups.ListGroupsInput{
			Filters: []rgtypes.GroupFilter{{Name: rgtypes.GroupFilterName(filter), Values: values}},
		})
		if listErr != nil {
			t.Fatal(listErr)
		}

		wantName := "alpha"
		if filter == "configuration-type" {
			wantName = "beta"
		}

		if len(got.GroupIdentifiers) != 1 || aws.ToString(got.GroupIdentifiers[0].GroupName) != wantName {
			t.Fatalf("filter %s: %+v", filter, got.GroupIdentifiers)
		}
	}

	page, err := g.ListGroups(ctx, &resourcegroups.ListGroupsInput{MaxResults: aws.Int32(1)})
	if err != nil {
		t.Fatal(err)
	}

	if len(page.GroupIdentifiers) != 1 || page.NextToken == nil {
		t.Fatalf("first page: %+v", page)
	}

	rest, err := g.ListGroups(ctx, &resourcegroups.ListGroupsInput{MaxResults: aws.Int32(1), NextToken: page.NextToken})
	if err != nil {
		t.Fatal(err)
	}

	if len(rest.GroupIdentifiers) != 1 || aws.ToString(rest.GroupIdentifiers[0].GroupName) != "beta" ||
		rest.NextToken != nil {
		t.Fatalf("second page: %+v", rest)
	}

	// Update the group, its query, and its configuration.
	updated, err := g.UpdateGroup(ctx, &resourcegroups.UpdateGroupInput{
		Group:       aws.String("arn:aws:resource-groups:us-east-1:000000000000:group/alpha"),
		Description: aws.String("tests"), DisplayName: aws.String("Alpha group"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(updated.Group.Description) != "tests" || aws.ToString(updated.Group.Owner) != "team-a" {
		t.Fatalf("updated: %+v", updated.Group)
	}

	if _, err = g.UpdateGroupQuery(ctx, &resourcegroups.UpdateGroupQueryInput{
		Group: aws.String("alpha"), ResourceQuery: tagQuery(`{"ResourceTypeFilters":["AWS::SQS::Queue"]}`),
	}); err != nil {
		t.Fatal(err)
	}

	gq, err := g.GetGroupQuery(ctx, &resourcegroups.GetGroupQueryInput{Group: aws.String("alpha")})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(gq.GroupQuery.ResourceQuery.Query) != `{"ResourceTypeFilters":["AWS::SQS::Queue"]}` {
		t.Fatalf("query: %+v", gq.GroupQuery)
	}

	if _, err = g.PutGroupConfiguration(ctx, &resourcegroups.PutGroupConfigurationInput{
		Group: aws.String("beta"),
		Configuration: []rgtypes.GroupConfigurationItem{{
			Type: aws.String("AWS::ResourceGroups::Generic"),
			Parameters: []rgtypes.GroupConfigurationParameter{
				{Name: aws.String("allowed-resource-types"), Values: []string{"AWS::EC2::Instance"}},
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	conf, err := g.GetGroupConfiguration(ctx, &resourcegroups.GetGroupConfigurationInput{Group: aws.String("beta")})
	if err != nil {
		t.Fatal(err)
	}

	if len(conf.GroupConfiguration.Configuration) != 1 ||
		len(conf.GroupConfiguration.Configuration[0].Parameters) != 1 {
		t.Fatalf("configuration: %+v", conf.GroupConfiguration)
	}

	// A configuration group has no query and no members here.
	if _, err = g.GetGroupQuery(ctx, &resourcegroups.GetGroupQueryInput{Group: aws.String("beta")}); err == nil {
		t.Fatal("GetGroupQuery on a configuration group succeeded")
	}

	members, err := g.ListGroupResources(ctx, &resourcegroups.ListGroupResourcesInput{Group: aws.String("beta")})
	if err != nil || len(members.Resources) != 0 {
		t.Fatalf("members = %+v, err = %v", members, err)
	}

	statuses, err := g.ListGroupingStatuses(ctx, &resourcegroups.ListGroupingStatusesInput{Group: aws.String("beta")})
	if err != nil || len(statuses.GroupingStatuses) != 0 {
		t.Fatalf("statuses = %+v, err = %v", statuses, err)
	}

	// Account settings settle at once.
	before, err := g.GetAccountSettings(ctx, &resourcegroups.GetAccountSettingsInput{})
	if err != nil || before.AccountSettings.GroupLifecycleEventsStatus != rgtypes.GroupLifecycleEventsStatusInactive {
		t.Fatalf("settings = %+v, err = %v", before, err)
	}

	after, err := g.UpdateAccountSettings(ctx, &resourcegroups.UpdateAccountSettingsInput{
		GroupLifecycleEventsDesiredStatus: rgtypes.GroupLifecycleEventsDesiredStatusActive,
	})
	if err != nil || after.AccountSettings.GroupLifecycleEventsStatus != rgtypes.GroupLifecycleEventsStatusActive {
		t.Fatalf("settings = %+v, err = %v", after, err)
	}

	// Delete.
	deleted, err := g.DeleteGroup(ctx, &resourcegroups.DeleteGroupInput{Group: aws.String("alpha")})
	if err != nil || aws.ToString(deleted.Group.Name) != "alpha" {
		t.Fatalf("deleted = %+v, err = %v", deleted, err)
	}

	_, err = g.GetGroup(ctx, &resourcegroups.GetGroupInput{Group: aws.String("alpha")})
	var missing *rgtypes.NotFoundException
	if !errors.As(err, &missing) {
		t.Fatalf("GetGroup after delete: %v", err)
	}

	// The name is free again.
	if _, err = g.CreateGroup(ctx, &resourcegroups.CreateGroupInput{
		Name: aws.String("alpha"), ResourceQuery: tagQuery(`{"ResourceTypeFilters":["AWS::AllSupported"]}`),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResourceGroupsTags(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	g := groupsClient(s, "test", "us-east-1")

	created, err := g.CreateGroup(ctx, &resourcegroups.CreateGroupInput{
		Name: aws.String("tagged"), ResourceQuery: tagQuery(`{"ResourceTypeFilters":["AWS::AllSupported"]}`),
		Tags: map[string]string{"a": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := created.Group.GroupArn
	if _, err = g.Tag(
		ctx,
		&resourcegroups.TagInput{Arn: arn, Tags: map[string]string{"b": "2", "c": "3"}},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = g.Untag(ctx, &resourcegroups.UntagInput{Arn: arn, Keys: []string{"c"}}); err != nil {
		t.Fatal(err)
	}

	got, err := g.GetTags(ctx, &resourcegroups.GetTagsInput{Arn: arn})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Tags) != 2 || got.Tags["a"] != "1" || got.Tags["b"] != "2" {
		t.Fatalf("tags = %v", got.Tags)
	}

	// The Tagging API sees the group, and tags it through the group's own operations.
	tg := taggingClient(s, "us-east-1")
	listed, err := tg.GetResources(
		ctx,
		&tagging.GetResourcesInput{ResourceTypeFilters: []string{"resource-groups:group"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if got := arns(listed.ResourceTagMappingList); !slices.Equal(got, []string{*arn}) {
		t.Fatalf("tagged groups = %v", got)
	}

	out, err := tg.TagResources(ctx, &tagging.TagResourcesInput{
		ResourceARNList: []string{*arn}, Tags: map[string]string{"via": "tagging"},
	})
	if err != nil || len(out.FailedResourcesMap) != 0 {
		t.Fatalf("TagResources = %+v, err = %v", out, err)
	}

	untagged, err := tg.UntagResources(
		ctx,
		&tagging.UntagResourcesInput{ResourceARNList: []string{*arn}, TagKeys: []string{"a"}},
	)
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatalf("UntagResources = %+v, err = %v", untagged, err)
	}

	got, err = g.GetTags(ctx, &resourcegroups.GetTagsInput{Arn: arn})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Tags) != 2 || got.Tags["via"] != "tagging" || got.Tags["b"] != "2" {
		t.Fatalf("tags after the Tagging API = %v", got.Tags)
	}

	// A group whose query selects tagged groups finds this one.
	found, err := g.SearchResources(ctx, &resourcegroups.SearchResourcesInput{
		ResourceQuery: tagQuery(`{"ResourceTypeFilters":["AWS::ResourceGroups::Group"],"TagFilters":[{"Key":"via"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := memberARNs(found.ResourceIdentifiers); !slices.Equal(got, []string{*arn}) {
		t.Fatalf("search = %v", got)
	}
}

func TestResourceGroupsCloudFormationQuery(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	g := groupsClient(s, "test", "us-east-1")

	const template = `{"Resources":{"Jobs":{"Type":"AWS::SQS::Queue","Properties":{"QueueName":"stack-jobs"}}}}`
	if _, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("members"), TemplateBody: aws.String(template),
	}); err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, c, "members", cfntypes.StackStatusCreateComplete)

	// A queue outside the stack is not a member.
	if _, err := sqsClient(
		s,
		"test",
	).CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("outside")}); err != nil {
		t.Fatal(err)
	}

	query := func(stack string) *rgtypes.ResourceQuery {
		return &rgtypes.ResourceQuery{
			Type:  rgtypes.QueryTypeCloudformationStack10,
			Query: aws.String(`{"StackIdentifier":"` + stack + `"}`),
		}
	}

	if _, err := g.CreateGroup(ctx, &resourcegroups.CreateGroupInput{
		Name: aws.String("stack"), ResourceQuery: query(aws.ToString(st.StackId)),
	}); err != nil {
		t.Fatal(err)
	}

	members, err := g.ListGroupResources(ctx, &resourcegroups.ListGroupResourcesInput{Group: aws.String("stack")})
	if err != nil {
		t.Fatal(err)
	}

	want := "arn:aws:sqs:us-east-1:000000000000:stack-jobs"
	if got := memberItems(members.Resources); !slices.Equal(got, []string{want}) {
		t.Fatalf("members = %v, want %v", got, want)
	}

	// A stack that does not exist is a query error, not a failure.
	missing, err := g.SearchResources(ctx, &resourcegroups.SearchResourcesInput{
		ResourceQuery: query("arn:aws:cloudformation:us-east-1:000000000000:stack/nope/0000"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(missing.ResourceIdentifiers) != 0 || len(missing.QueryErrors) != 1 ||
		missing.QueryErrors[0].ErrorCode != rgtypes.QueryErrorCodeCloudformationStackNotExisting {
		t.Fatalf("missing stack: %+v", missing)
	}
}
