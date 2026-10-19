package integration_test

import (
	"context"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

func TestIAMSimulateCustomPolicy(t *testing.T) {
	c := iamClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	const resource = "arn:aws:s3:::simulation/key"
	const caller = "arn:aws:iam::000000000000:user/alice"
	deny := `{"Statement":{"Effect":"Deny","Action":"s3:DeleteObject","Resource":"*"}}`
	out, err := c.SimulateCustomPolicy(ctx, &iam.SimulateCustomPolicyInput{
		PolicyInputList: []string{s3ReadPolicy, deny},
		ActionNames:     []string{"s3:GetObject", "s3:DeleteObject", "s3:PutObject"},
		ResourceArns:    []string{resource},
	})
	if err != nil {
		t.Fatal(err)
	}

	for i, want := range []string{"allowed", "explicitDeny", "implicitDeny"} {
		result := out.EvaluationResults[i]
		if string(result.EvalDecision) != want || len(result.ResourceSpecificResults) != 1 ||
			string(result.ResourceSpecificResults[0].EvalResourceDecision) != want {
			t.Fatalf("result %d: %+v", i, result)
		}
	}

	if len(out.EvaluationResults[0].MatchedStatements) != 1 ||
		*out.EvaluationResults[0].MatchedStatements[0].SourcePolicyId != "PolicyInputList.1" ||
		string(out.EvaluationResults[0].MatchedStatements[0].SourcePolicyType) != "user" {
		t.Fatalf("missing statement provenance: %+v", out)
	}

	t.Run("boundary", func(t *testing.T) {
		out, err := c.SimulateCustomPolicy(ctx, &iam.SimulateCustomPolicyInput{
			PolicyInputList: []string{boundaryBroad}, PermissionsBoundaryPolicyInputList: []string{s3ReadPolicy},
			ActionNames: []string{"s3:GetObject", "s3:PutObject"},
		})
		if err != nil {
			t.Fatal(err)
		}

		if string(out.EvaluationResults[0].EvalDecision) != "allowed" ||
			!out.EvaluationResults[0].PermissionsBoundaryDecisionDetail.AllowedByPermissionsBoundary ||
			string(out.EvaluationResults[1].EvalDecision) != "implicitDeny" ||
			out.EvaluationResults[1].PermissionsBoundaryDecisionDetail.AllowedByPermissionsBoundary {
			t.Fatalf("boundary: %+v", out)
		}
	})

	t.Run("resource policy", func(t *testing.T) {
		grant := `{"Statement":{"Effect":"Allow","Principal":{"AWS":"` + caller + `"},` +
			`"Action":"s3:PutObject","Resource":"*"}}`
		for _, owner := range []string{"000000000000", "222222222222"} {
			out, err := c.SimulateCustomPolicy(ctx, &iam.SimulateCustomPolicyInput{
				PolicyInputList: []string{s3ReadPolicy}, ActionNames: []string{"s3:PutObject"},
				ResourceArns: []string{resource}, ResourcePolicy: &grant, CallerArn: new(caller), ResourceOwner: &owner,
			})
			if err != nil {
				t.Fatal(err)
			}

			want := "allowed"
			if owner == "222222222222" {
				want = "implicitDeny"
			}

			if string(out.EvaluationResults[0].EvalDecision) != want {
				t.Fatalf("owner %s: %+v", owner, out)
			}
		}
	})

	t.Run("deny precedence", func(t *testing.T) {
		grant := `{"Statement":{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"*"}}`
		deny := `{"Statement":{"Effect":"Deny","Action":"s3:GetObject","Resource":"*"}}`
		for _, layer := range []string{"identity", "boundary", "resource"} {
			input := &iam.SimulateCustomPolicyInput{
				PolicyInputList: []string{s3ReadPolicy}, ActionNames: []string{"s3:GetObject"},
				CallerArn: new(caller), ResourcePolicy: &grant,
			}

			switch layer {
			case "identity":
				input.PolicyInputList = append(input.PolicyInputList, deny)
			case "boundary":
				input.PermissionsBoundaryPolicyInputList = []string{deny}
			case "resource":
				input.ResourcePolicy = new(`{"Statement":{"Effect":"Deny","Principal":"*",` +
					`"Action":"s3:GetObject","Resource":"*"}}`)
			}

			out, err := c.SimulateCustomPolicy(ctx, input)
			if err != nil {
				t.Fatal(err)
			}

			result := out.EvaluationResults[0]
			if string(result.EvalDecision) != "explicitDeny" || len(result.MatchedStatements) != 1 {
				t.Fatalf("%s deny: %+v", layer, result)
			}
		}

		out, err := c.SimulateCustomPolicy(ctx, &iam.SimulateCustomPolicyInput{
			PolicyInputList: []string{s3ReadPolicy}, ActionNames: []string{"s3:GetObject"},
			CallerArn: new(caller), ResourcePolicy: &grant, PermissionsBoundaryPolicyInputList: []string{boundaryOther},
		})
		if err != nil {
			t.Fatal(err)
		}

		if string(out.EvaluationResults[0].EvalDecision) != "allowed" ||
			out.EvaluationResults[0].PermissionsBoundaryDecisionDetail.AllowedByPermissionsBoundary {
			t.Fatalf("direct resource grant must bypass implicit boundary deny: %+v", out)
		}
	})

	t.Run("missing username variable", func(t *testing.T) {
		input := &iam.SimulateCustomPolicyInput{
			PolicyInputList: []string{`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
				`"Action":"s3:GetObject","Resource":"arn:aws:s3:::simulation/${aws:username}"}}`},
			ActionNames: []string{"s3:GetObject"}, ResourceArns: []string{"arn:aws:s3:::simulation/alice"},
		}

		out, err := c.SimulateCustomPolicy(ctx, input)
		if err != nil {
			t.Fatal(err)
		}

		if string(out.EvaluationResults[0].EvalDecision) != "implicitDeny" ||
			!slices.Contains(out.EvaluationResults[0].MissingContextValues, "aws:username") {
			t.Fatalf("absent username variable: %+v", out)
		}

		input.ContextEntries = []iamtypes.ContextEntry{{
			ContextKeyName: new("aws:username"), ContextKeyType: "string", ContextKeyValues: []string{"alice"},
		}}
		out, err = c.SimulateCustomPolicy(ctx, input)
		if err != nil {
			t.Fatal(err)
		}

		if string(out.EvaluationResults[0].EvalDecision) != "allowed" {
			t.Fatalf("username variable: %+v", out)
		}
	})

	t.Run("context", func(t *testing.T) {
		doc := `{"Statement":{"Effect":"Allow","Action":"s3:GetObject","Resource":"*",` +
			`"Condition":{"StringEquals":{"s3:prefix":"public"},"Bool":{"aws:SecureTransport":"true"}}}}`
		input := &iam.SimulateCustomPolicyInput{
			PolicyInputList: []string{doc}, ActionNames: []string{"s3:GetObject"}, ResourceArns: []string{resource},
		}

		out, err := c.SimulateCustomPolicy(ctx, input)
		if err != nil {
			t.Fatal(err)
		}

		result := out.EvaluationResults[0]
		if string(result.EvalDecision) != "implicitDeny" ||
			!slices.Contains(result.ResourceSpecificResults[0].MissingContextValues, "s3:prefix") ||
			!slices.Contains(result.MissingContextValues, "aws:SecureTransport") {
			t.Fatalf("missing context: %+v", result)
		}

		input.ContextEntries = []iamtypes.ContextEntry{
			{ContextKeyName: new("S3:Prefix"), ContextKeyValues: []string{"public"}, ContextKeyType: "string"},
			{ContextKeyName: new("aws:SecureTransport"), ContextKeyValues: []string{"true"}, ContextKeyType: "boolean"},
		}

		out, err = c.SimulateCustomPolicy(ctx, input)
		if err != nil {
			t.Fatal(err)
		}

		if string(out.EvaluationResults[0].EvalDecision) != "allowed" ||
			len(out.EvaluationResults[0].MissingContextValues) != 0 {
			t.Fatalf("supplied context: %+v", out)
		}

		input.ContextEntries[1].ContextKeyType = "invalid"
		_, err = c.SimulateCustomPolicy(ctx, input)
		wantCode(t, err, "InvalidInput")
	})

	t.Run("pagination per action and resource", func(t *testing.T) {
		input := &iam.SimulateCustomPolicyInput{
			PolicyInputList: []string{`{"Statement":{"Effect":"Allow","Action":"s3:GetObject",` +
				`"Resource":"arn:aws:s3:::simulation/key"}}`},
			ActionNames:  []string{"s3:GetObject", "s3:PutObject"},
			ResourceArns: []string{resource, "arn:aws:s3:::other/key"}, MaxItems: new(int32(3)),
		}

		first, err := c.SimulateCustomPolicy(ctx, input)
		if err != nil {
			t.Fatal(err)
		}

		want := []string{"allowed", "implicitDeny", "implicitDeny"}
		if !first.IsTruncated || first.Marker == nil || len(first.EvaluationResults) != 3 {
			t.Fatalf("first page: %+v", first)
		}

		for i, result := range first.EvaluationResults {
			if string(result.EvalDecision) != want[i] || *result.EvalResourceName != input.ResourceArns[i%2] ||
				len(result.ResourceSpecificResults) != 1 {
				t.Fatalf("result %d: %+v", i, result)
			}
		}

		input.Marker = first.Marker
		last, err := c.SimulateCustomPolicy(ctx, input)
		if err != nil {
			t.Fatal(err)
		}

		if last.IsTruncated || len(last.EvaluationResults) != 1 ||
			*last.EvaluationResults[0].EvalActionName != "s3:PutObject" ||
			*last.EvaluationResults[0].EvalResourceName != "arn:aws:s3:::other/key" {
			t.Fatalf("last page: %+v", last)
		}
	})

	t.Run("absent variable leaves other resources", func(t *testing.T) {
		out, err := c.SimulateCustomPolicy(ctx, &iam.SimulateCustomPolicyInput{
			PolicyInputList: []string{`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
				`"Action":"s3:GetObject","Resource":["arn:aws:s3:::home/${aws:username}/*","*"]}}`},
			ActionNames: []string{"s3:GetObject"}, ResourceArns: []string{resource},
		})
		if err != nil {
			t.Fatal(err)
		}

		if string(out.EvaluationResults[0].EvalDecision) != "allowed" ||
			!slices.Contains(out.EvaluationResults[0].MissingContextValues, "aws:username") {
			t.Fatalf("absent variable: %+v", out)
		}
	})

	t.Run("invalid policies", func(t *testing.T) {
		for _, document := range []string{
			`{`, `{"Statement":[]}`, `{"Statement":{"Effect":"Allow","Action":"*","Resource":"*","Bogus":true}}`,
			`{"Statement":{"Effect":"Allow","Action":"unrelated:Action","Resource":"*",` +
				`"Condition":{"UnknownOperator":{"aws:username":"x"}}}}`,
		} {
			_, err := c.SimulateCustomPolicy(ctx, &iam.SimulateCustomPolicyInput{
				PolicyInputList: []string{s3ReadPolicy, document}, ActionNames: []string{"s3:GetObject"},
			})
			wantCode(t, err, "InvalidInput")
		}
	})
}

func TestIAMSimulatePrincipalPolicy(t *testing.T) {
	c := iamClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	u, err := c.CreateUser(ctx, &iam.CreateUserInput{UserName: new("sim-user"), Path: new("/apps/")})
	if err != nil {
		t.Fatal(err)
	}

	g, err := c.CreateGroup(ctx, &iam.CreateGroupInput{GroupName: new("sim-group")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AddUserToGroup(ctx, &iam.AddUserToGroupInput{GroupName: g.Group.GroupName, UserName: u.User.UserName})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutGroupPolicy(ctx, &iam.PutGroupPolicyInput{
		GroupName: g.Group.GroupName, PolicyName: new("group-read"), PolicyDocument: new(s3ReadPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, source := range []*string{u.User.Arn, g.Group.Arn} {
		out, simulationErr := c.SimulatePrincipalPolicy(ctx, &iam.SimulatePrincipalPolicyInput{
			PolicySourceArn: source, ActionNames: []string{"s3:GetObject"},
		})
		if simulationErr != nil {
			t.Fatal(simulationErr)
		}

		if string(out.EvaluationResults[0].EvalDecision) != "allowed" ||
			string(out.EvaluationResults[0].MatchedStatements[0].SourcePolicyType) != "group" {
			t.Fatalf("group inheritance: %+v", out)
		}
	}

	boundary := boundaryManaged(t, c, "sim-boundary", boundaryOther)
	_, err = c.PutUserPermissionsBoundary(ctx, &iam.PutUserPermissionsBoundaryInput{
		UserName: u.User.UserName, PermissionsBoundary: boundary,
	})
	if err != nil {
		t.Fatal(err)
	}

	input := &iam.SimulatePrincipalPolicyInput{PolicySourceArn: u.User.Arn, ActionNames: []string{"s3:GetObject"}}
	out, err := c.SimulatePrincipalPolicy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	if string(out.EvaluationResults[0].EvalDecision) != "implicitDeny" {
		t.Fatalf("stored boundary: %+v", out)
	}

	input.PermissionsBoundaryPolicyInputList = []string{s3ReadPolicy}
	out, err = c.SimulatePrincipalPolicy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	if string(out.EvaluationResults[0].EvalDecision) != "allowed" {
		t.Fatalf("boundary override: %+v", out)
	}

	input.PolicyInputList = []string{`{"Statement":{"Effect":"Deny","Action":"s3:GetObject","Resource":"*"}}`}
	out, err = c.SimulatePrincipalPolicy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	if string(out.EvaluationResults[0].EvalDecision) != "explicitDeny" ||
		len(out.EvaluationResults[0].MatchedStatements) != 1 {
		t.Fatalf("extra identity deny: %+v", out)
	}

	r, err := c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("sim-role"), AssumeRolePolicyDocument: new(trustPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName: r.Role.RoleName, PolicyArn: new("arn:aws:iam::aws:policy/AmazonS3FullAccess"),
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err = c.SimulatePrincipalPolicy(ctx, &iam.SimulatePrincipalPolicyInput{
		PolicySourceArn: r.Role.Arn, ActionNames: []string{"s3:PutObject"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if string(out.EvaluationResults[0].EvalDecision) != "allowed" {
		t.Fatalf("role managed policy: %+v", out)
	}

	_, err = c.SimulatePrincipalPolicy(ctx, &iam.SimulatePrincipalPolicyInput{
		PolicySourceArn: new("arn:aws:iam::000000000000:user/unknown"), ActionNames: []string{"s3:GetObject"},
	})
	wantCode(t, err, "NoSuchEntity")
}
