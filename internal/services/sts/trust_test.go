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
		if got := trusts(tc.document, tc.caller, tc.externalID); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
