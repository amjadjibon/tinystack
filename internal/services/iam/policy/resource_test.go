package policy

import (
	"fmt"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/request"
)

func TestResourcePrincipals(t *testing.T) {
	principal := &request.Principal{
		Account:   "123456789012",
		Kind:      request.PrincipalRole,
		ARN:       "arn:aws:sts::123456789012:assumed-role/worker/session",
		PolicyARN: "arn:aws:iam::123456789012:role/apps/worker",
	}

	for _, test := range []struct {
		principal string
		allow     bool
	}{
		{`"*"`, true},
		{`{"AWS":"*"}`, true},
		{`{"AWS":"123456789012"}`, true},
		{`{"AWS":"arn:aws:iam::123456789012:root"}`, true},
		{`{"AWS":"arn:aws:iam::123456789012:role/apps/worker"}`, true},
		{`{"AWS":["arn:aws:iam::999999999999:root","arn:aws:sts::123456789012:assumed-role/worker/session"]}`, true},
		{`{"AWS":"arn:aws:iam::123456789012:user/alice"}`, false},
		{`{"Service":"sns.amazonaws.com"}`, false},
	} {
		t.Run(test.principal, func(t *testing.T) {
			doc := fmt.Sprintf(
				`{"Statement":{"Effect":"Allow","Principal":%s,"Action":"sqs:SendMessage","Resource":"*"}}`,
				test.principal,
			)
			if err := ValidateResource(doc); err != nil {
				t.Fatal(err)
			}

			got := EvaluateResource(Document{JSON: doc}, "sqs:SendMessage", "*", Context{}, principal)
			if got.Allowed != test.allow || len(got.Unsupported) > 0 {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestResourceAccountPrincipalDelegates(t *testing.T) {
	role := &request.Principal{
		Account: "123456789012", Kind: request.PrincipalRole,
		ARN: "arn:aws:sts::123456789012:assumed-role/worker/s", PolicyARN: "arn:aws:iam::123456789012:role/worker",
	}
	root := &request.Principal{
		Account: "123456789012", Kind: request.PrincipalRoot,
		ARN: "arn:aws:iam::123456789012:root", PolicyARN: "arn:aws:iam::123456789012:root",
	}
	statement := `{"Statement":{"Effect":"%s","Principal":{"AWS":"%s"},"Action":"sqs:SendMessage","Resource":"*"}}`
	for _, test := range []struct {
		principal                  *request.Principal
		name, effect, named, owner string
		allowed, denied            bool
	}{
		{role, "same-account role needs its identity policy", "Allow", "123456789012", "123456789012", false, false},
		{role, "same-account role by root ARN", "Allow", "arn:aws:iam::123456789012:root", "123456789012", false, false},
		{root, "owning root", "Allow", "123456789012", "123456789012", true, false},
		{role, "other account trusts the caller's account", "Allow", "123456789012", "999999999999", true, false},
		{role, "account deny applies to everyone in it", "Deny", "123456789012", "123456789012", false, true},
		{role, "role named directly", "Allow", "arn:aws:iam::123456789012:role/worker", "123456789012", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := fmt.Sprintf(statement, test.effect, test.named)
			got := EvaluateResource(Document{JSON: doc}, "sqs:SendMessage", "*",
				Context{ResourceAccount: test.owner}, test.principal)
			if got.Allowed != test.allowed || got.ExplicitDeny != test.denied {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestResourceValidationAndConditions(t *testing.T) {
	for _, statement := range []string{
		`{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*","NotPrincipal":"*"}`,
		`{"Effect":"Allow","Principal":{"Federated":"provider"},"Action":"*","Resource":"*"}`,
		`{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::*:role/*"},"Action":"*","Resource":"*"}`,
		`{"Effect":"Allow","Action":"*","Resource":"*"}`,
		`{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*","Condition":{"Unknown":{"aws:SourceArn":"x"}}}`,
		`{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*","Condition":{"Null":{"s3:Unknown":"true"}}}`,
		`{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*","Conditions":{}}`,
	} {
		if err := ValidateResource(`{"Statement":` + statement + `}`); err == nil {
			t.Fatalf("accepted %s", statement)
		}
	}

	doc := `{"Statement":{"Effect":"Allow",
"Principal":"*",
"Action":"*",
"Resource":"*",
"Condition":{"ArnEquals":{"aws:SourceArn":"arn:aws:sns:us-east-1:123456789012:topic"},
"StringEquals":{"aws:SourceAccount":"123456789012"}}}}`
	if err := ValidateResource(doc); err != nil {
		t.Fatal(err)
	}

	context := Context{
		Values: map[string][]string{
			"aws:sourcearn":     {"arn:aws:sns:us-east-1:123456789012:topic"},
			"aws:sourceaccount": {"123456789012"},
		},
	}

	if !EvaluateResource(Document{JSON: doc}, "sqs:SendMessage", "*", context, &request.Principal{}).Allowed {
		t.Fatal("source conditions did not match")
	}
}
