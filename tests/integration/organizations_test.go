package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestOrganizationsLifecycle(t *testing.T) {
	cfg := config.Default()
	cfg.Identity.Accounts = map[string]string{"team-b": "111111111111"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	c := organizations.New(
		organizations.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Retryer:      aws.NopRetryer{},
			Credentials:  creds("test"),
		},
	)
	_, err := c.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	code(t, err, "AWSOrganizationsNotInUseException")
	org, err := c.CreateOrganization(ctx, &organizations.CreateOrganizationInput{})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(org.Organization.MasterAccountId) != "000000000000" ||
		string(org.Organization.FeatureSet) != "ALL" {
		t.Fatalf("organization: %+v", org)
	}

	_, err = c.CreateOrganization(ctx, &organizations.CreateOrganizationInput{})
	code(t, err, "AlreadyInOrganizationException")
	roots, err := c.ListRoots(ctx, &organizations.ListRootsInput{})
	if err != nil || len(roots.Roots) != 1 {
		t.Fatalf("roots %v %+v", err, roots)
	}

	root := roots.Roots[0].Id
	policies, err := c.ListPoliciesForTarget(
		ctx,
		&organizations.ListPoliciesForTargetInput{TargetId: root, Filter: types.PolicyTypeServiceControlPolicy},
	)
	if err != nil || len(policies.Policies) != 1 || !policies.Policies[0].AwsManaged {
		t.Fatalf("default policy: %v %+v", err, policies)
	}

	account, err := c.CreateAccount(
		ctx,
		&organizations.CreateAccountInput{
			AccountName: new("Member"),
			Email:       new("member@example.com"),
			Tags:        []types.Tag{{Key: new("team"), Value: new("dev")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := account.CreateAccountStatus.AccountId
	if account.CreateAccountStatus.State != types.CreateAccountStateSucceeded || len(aws.ToString(id)) != 12 {
		t.Fatalf("account %+v", account)
	}

	status, err := c.DescribeCreateAccountStatus(
		ctx,
		&organizations.DescribeCreateAccountStatusInput{CreateAccountRequestId: account.CreateAccountStatus.Id},
	)
	if err != nil || aws.ToString(status.CreateAccountStatus.AccountId) != aws.ToString(id) {
		t.Fatalf("status %v", err)
	}

	_, err = c.CreateAccount(
		ctx,
		&organizations.CreateAccountInput{AccountName: new("Dup"), Email: new("member@example.com")},
	)
	code(t, err, "ConstraintViolationException")
	described, err := c.DescribeAccount(ctx, &organizations.DescribeAccountInput{AccountId: id})
	if err != nil || aws.ToString(described.Account.Name) != "Member" {
		t.Fatalf("describe %v", err)
	}

	_, err = c.DescribeAccount(ctx, &organizations.DescribeAccountInput{AccountId: new("123456789012")})
	code(t, err, "AccountNotFoundException")
	accounts, err := c.ListAccounts(ctx, &organizations.ListAccountsInput{MaxResults: new(int32(1))})
	if err != nil || len(accounts.Accounts) != 1 || accounts.NextToken == nil {
		t.Fatalf("page %v %+v", err, accounts)
	}

	second, err := c.ListAccounts(
		ctx,
		&organizations.ListAccountsInput{MaxResults: new(int32(1)), NextToken: accounts.NextToken},
	)
	if err != nil || len(second.Accounts) != 1 || second.NextToken != nil {
		t.Fatalf("second page %v %+v", err, second)
	}

	_, err = c.ListAccounts(ctx, &organizations.ListAccountsInput{NextToken: new("!")})
	code(t, err, "InvalidInputException")
	ou, err := c.CreateOrganizationalUnit(
		ctx,
		&organizations.CreateOrganizationalUnitInput{ParentId: root, Name: new("Engineering")},
	)
	if err != nil {
		t.Fatal(err)
	}

	ouID := ou.OrganizationalUnit.Id
	_, err = c.CreateOrganizationalUnit(
		ctx,
		&organizations.CreateOrganizationalUnitInput{ParentId: root, Name: new("Engineering")},
	)
	code(t, err, "DuplicateOrganizationalUnitException")
	_, err = c.UpdateOrganizationalUnit(
		ctx,
		&organizations.UpdateOrganizationalUnitInput{OrganizationalUnitId: ouID, Name: new("Platform")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeOrganizationalUnit(
		ctx,
		&organizations.DescribeOrganizationalUnitInput{OrganizationalUnitId: ouID},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.MoveAccount(
		ctx,
		&organizations.MoveAccountInput{AccountId: id, SourceParentId: root, DestinationParentId: ouID},
	)
	if err != nil {
		t.Fatal(err)
	}

	parents, err := c.ListParents(ctx, &organizations.ListParentsInput{ChildId: id})
	if err != nil || len(parents.Parents) != 1 || aws.ToString(parents.Parents[0].Id) != aws.ToString(ouID) {
		t.Fatalf("parents %v %+v", err, parents)
	}

	children, err := c.ListChildren(
		ctx,
		&organizations.ListChildrenInput{ParentId: ouID, ChildType: types.ChildTypeAccount},
	)
	if err != nil || len(children.Children) != 1 {
		t.Fatalf("children %v", err)
	}

	byParent, err := c.ListAccountsForParent(ctx, &organizations.ListAccountsForParentInput{ParentId: ouID})
	if err != nil || len(byParent.Accounts) != 1 {
		t.Fatalf("accounts for parent %v", err)
	}

	units, err := c.ListOrganizationalUnitsForParent(
		ctx,
		&organizations.ListOrganizationalUnitsForParentInput{ParentId: root},
	)
	if err != nil || len(units.OrganizationalUnits) != 1 {
		t.Fatalf("units %v", err)
	}

	_, err = c.DeleteOrganizationalUnit(ctx, &organizations.DeleteOrganizationalUnitInput{OrganizationalUnitId: ouID})
	code(t, err, "OrganizationalUnitNotEmptyException")
	_, err = c.DeleteOrganization(ctx, &organizations.DeleteOrganizationInput{})
	code(t, err, "OrganizationNotEmptyException")
	// Configured SigV4 credentials resolve a newly created member account.
	cfg.Identity.Accounts["member"] = *id
	member := organizations.New(organizations.Options{
		Region: "eu-west-1", BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{}, Credentials: creds("member"),
	})
	visible, err := member.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if err != nil || aws.ToString(visible.Organization.Id) != aws.ToString(org.Organization.Id) {
		t.Fatalf("member describe %v %+v", err, visible)
	}

	_, err = member.ListAccounts(ctx, &organizations.ListAccountsInput{})
	code(t, err, "AccessDeniedException")
	unrelated := organizations.New(
		organizations.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Retryer:      aws.NopRetryer{},
			Credentials:  creds("team-b"),
		},
	)
	_, err = unrelated.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	code(t, err, "AWSOrganizationsNotInUseException")
	policy, err := c.CreatePolicy(
		ctx,
		&organizations.CreatePolicyInput{
			Name:        new("Guardrail"),
			Description: new("record"),
			Type:        types.PolicyTypeServiceControlPolicy,
			Content:     new(`{"Version":"2012-10-17","Statement":[]}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	pid := policy.Policy.PolicySummary.Id
	_, err = c.AttachPolicy(ctx, &organizations.AttachPolicyInput{PolicyId: pid, TargetId: root})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AttachPolicy(ctx, &organizations.AttachPolicyInput{PolicyId: pid, TargetId: root})
	code(t, err, "DuplicatePolicyAttachmentException")
	_, err = c.DeletePolicy(ctx, &organizations.DeletePolicyInput{PolicyId: pid})
	code(t, err, "PolicyInUseException")
	targets, err := c.ListTargetsForPolicy(ctx, &organizations.ListTargetsForPolicyInput{PolicyId: pid})
	if err != nil || len(targets.Targets) != 1 {
		t.Fatalf("targets %v", err)
	}

	_, err = c.DescribePolicy(ctx, &organizations.DescribePolicyInput{PolicyId: pid})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdatePolicy(ctx, &organizations.UpdatePolicyInput{PolicyId: pid, Description: new("updated")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TagResource(
		ctx,
		&organizations.TagResourceInput{ResourceId: pid, Tags: []types.Tag{{Key: new("env"), Value: new("test")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tagout, err := c.ListTagsForResource(ctx, &organizations.ListTagsForResourceInput{ResourceId: pid})
	if err != nil || len(tagout.Tags) != 1 {
		t.Fatalf("tags %v %+v", err, tagout)
	}

	_, err = c.UntagResource(ctx, &organizations.UntagResourceInput{ResourceId: pid, TagKeys: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DetachPolicy(ctx, &organizations.DetachPolicyInput{PolicyId: pid, TargetId: root})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeletePolicy(ctx, &organizations.DeletePolicyInput{PolicyId: pid})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DisablePolicyType(
		ctx,
		&organizations.DisablePolicyTypeInput{RootId: root, PolicyType: types.PolicyTypeServiceControlPolicy},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.EnablePolicyType(
		ctx,
		&organizations.EnablePolicyTypeInput{RootId: root, PolicyType: types.PolicyTypeServiceControlPolicy},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ListPolicies(ctx, &organizations.ListPoliciesInput{Filter: types.PolicyTypeTagPolicy})
	code(t, err, "NotImplemented")
	_, err = c.CreateAccount(
		ctx,
		&organizations.CreateAccountInput{
			AccountName: new("unsupported"),
			Email:       new("other@example.com"),
			RoleName:    new("Custom"),
		},
	)
	code(t, err, "ValidationException")
	_, err = c.RemoveAccountFromOrganization(ctx, &organizations.RemoveAccountFromOrganizationInput{AccountId: id})
	if err != nil {
		t.Fatal(err)
	}

	_, err = member.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	code(t, err, "AWSOrganizationsNotInUseException")
	_, err = c.DeleteOrganizationalUnit(ctx, &organizations.DeleteOrganizationalUnitInput{OrganizationalUnitId: ouID})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteOrganization(ctx, &organizations.DeleteOrganizationInput{})
	if err != nil {
		t.Fatal(err)
	}
}
