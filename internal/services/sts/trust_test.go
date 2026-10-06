package sts

import "testing"

func TestTrusts(t *testing.T) {
	root := caller{account: "111111111111", arns: []string{"arn:aws:iam::111111111111:root"}}
	session := caller{account: "111111111111", arns: []string{
		"arn:aws:iam::111111111111:root",
		"arn:aws:iam::111111111111:role/ci",
		"arn:aws:sts::111111111111:assumed-role/ci/run",
	}}
	allow := `{"Effect":"Allow","Principal":{"AWS":"111111111111"},"Action":"sts:AssumeRole"}`
	for _, tc := range []struct {
		name       string
		document   string
		externalID string
		caller     caller
		want       bool
	}{
		{"single statement", `{"Statement":` + allow + `}`, "", root, true},
		{"statement list", `{"Statement":[` + allow + `]}`, "", root, true},
		{"other account", `{"Statement":[` + allow + `]}`, "", caller{account: "222222222222"}, false},
		{"any principal", `{"Statement":{"Effect":"Allow","Principal":"*","Action":"sts:*"}}`, "", root, true},
		{
			"service principal",
			`{"Statement":{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}}`,
			"", root, false,
		},
		{
			"role principal",
			`{"Statement":{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::111111111111:role/ci"]},` +
				`"Action":["s3:*","STS:Assume*"]}}`,
			"", session, true,
		},
		{
			"role principal, root caller",
			`{"Statement":{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::111111111111:role/ci"},` +
				`"Action":"sts:AssumeRole"}}`,
			"", root, false,
		},
		{"other action", `{"Statement":{"Effect":"Allow","Principal":"*","Action":"sts:TagSession"}}`, "", root, false},
		{
			"deny wins",
			`{"Statement":[` + allow + `,{"Effect":"Deny","Principal":"*","Action":"sts:AssumeRole"}]}`,
			"", root, false,
		},
		{
			"external ID",
			`{"Statement":{"Effect":"Allow","Principal":"*","Action":"sts:AssumeRole",` +
				`"Condition":{"StringEquals":{"sts:ExternalId":["a1","b2"]}}}}`,
			"b2", root, true,
		},
		{
			"wrong external ID",
			`{"Statement":{"Effect":"Allow","Principal":"*","Action":"sts:AssumeRole",` +
				`"Condition":{"StringEquals":{"sts:ExternalId":"a1"}}}}`,
			"", root, false,
		},
		{
			"unknown condition grants nothing",
			`{"Statement":{"Effect":"Allow","Principal":"*","Action":"sts:AssumeRole",` +
				`"Condition":{"Bool":{"aws:MultiFactorAuthPresent":"true"}}}}`,
			"", root, false,
		},
		{
			"unknown condition still denies",
			`{"Statement":[` + allow + `,{"Effect":"Deny","Principal":"*","Action":"sts:AssumeRole",` +
				`"Condition":{"Bool":{"aws:MultiFactorAuthPresent":"false"}}}]}`,
			"", root, false,
		},
		{
			"NotPrincipal grants nothing",
			`{"Statement":{"Effect":"Allow","Principal":"*","NotPrincipal":{"AWS":"x"},"Action":"sts:AssumeRole"}}`,
			"", root, false,
		},
		{"malformed", `{"Statement":"nope"}`, "", root, false},
	} {
		c := tc.caller
		c.action, c.context = "sts:AssumeRole", map[string][]string{"sts:externalid": present(tc.externalID)}
		if got := trusts(tc.document, &c); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTrustsSetOperators(t *testing.T) {
	c := caller{
		account: "111111111111", federated: "cognito-identity.amazonaws.com", action: "sts:AssumeRoleWithWebIdentity",
		context: map[string][]string{
			"cognito-identity.amazonaws.com:aud": {"us-east-1:pool"},
			"cognito-identity.amazonaws.com:amr": {"authenticated", "cognito-idp.us-east-1.amazonaws.com/pool"},
		},
	}
	allow := `{"Statement":{"Effect":"Allow","Principal":{"Federated":"cognito-identity.amazonaws.com"},` +
		`"Action":"sts:AssumeRoleWithWebIdentity","Condition":`
	for condition, want := range map[string]bool{
		`{"ForAnyValue:StringLike":{"cognito-identity.amazonaws.com:amr":"authenticated"}}`:                    true,
		`{"ForAnyValue:StringLike":{"cognito-identity.amazonaws.com:amr":"unauthenticated"}}`:                  false,
		`{"ForAllValues:StringLike":{"cognito-identity.amazonaws.com:amr":["authenticated","cognito-idp.*"]}}`: true,
		`{"ForAllValues:StringEquals":{"cognito-identity.amazonaws.com:amr":"authenticated"}}`:                 false,
		`{"StringEquals":{"cognito-identity.amazonaws.com:aud":"us-east-1:pool"}}`:                             true,
		`{"ForSomeValues:StringLike":{"cognito-identity.amazonaws.com:amr":"authenticated"}}`:                  false,
	} {
		if got := trusts(allow+condition+`}}`, &c); got != want {
			t.Errorf("%s: got %v, want %v", condition, got, want)
		}
	}
}

func TestTrustsWebIdentity(t *testing.T) {
	provider := "arn:aws:iam::111111111111:oidc-provider/token.actions.githubusercontent.com"
	c := caller{
		account: "111111111111", federated: provider, action: "sts:AssumeRoleWithWebIdentity",
		context: map[string][]string{
			"token.actions.githubusercontent.com:aud": {"sts.amazonaws.com"},
			"token.actions.githubusercontent.com:sub": {"repo:acme/app:ref:refs/heads/main"},
		},
	}
	allow := `{"Effect":"Allow","Principal":{"Federated":"` + provider + `"},"Action":"sts:AssumeRoleWithWebIdentity"`
	for _, tc := range []struct {
		name, document string
		want           bool
	}{
		{"provider", `{"Statement":` + allow + `}}`, true},
		{
			"subject pattern",
			`{"Statement":` + allow + `,"Condition":{"StringEquals":{"token.actions.githubusercontent.com:aud":` +
				`"sts.amazonaws.com"},"StringLike":{"token.actions.githubusercontent.com:sub":"repo:acme/app:*"}}}}`,
			true,
		},
		{
			"other repository",
			`{"Statement":` + allow + `,"Condition":{"StringLike":{"token.actions.githubusercontent.com:sub":` +
				`"repo:acme/other:*"}}}}`,
			false,
		},
		{
			"other provider",
			`{"Statement":{"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::111111111111:oidc-provider/x"},` +
				`"Action":"sts:AssumeRoleWithWebIdentity"}}`,
			false,
		},
		{
			"AWS principal",
			`{"Statement":{"Effect":"Allow","Principal":{"AWS":"111111111111"},` +
				`"Action":"sts:AssumeRoleWithWebIdentity"}}`,
			false,
		},
		{"AssumeRole only", `{"Statement":{"Effect":"Allow","Principal":"*","Action":"sts:AssumeRole"}}`, false},
	} {
		if got := trusts(tc.document, &c); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
