package policy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEvaluation(t *testing.T) {
	tests := []struct {
		name, statement, action, resource string
		problem                           string
		allow, deny                       bool
	}{
		{
			name:      "allow scalar",
			statement: `{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::b/k",
			allow:     true,
			deny:      false,
			problem:   "",
		},
		{
			name:      "implicit",
			statement: `{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`,
			action:    "s3:PutObject",
			resource:  "arn:aws:s3:::b/k",
			allow:     false,
			deny:      false,
			problem:   "",
		},
		{
			name: "explicit", statement: `[{"Effect":"Allow",
"Action":"*",
"Resource":"*"},
{"Effect":"Deny",
"Action":"s3:*",
"Resource":"*"}]`,
			action: "s3:PutObject", resource: "arn:aws:s3:::b/k", allow: false, deny: true, problem: "",
		},
		{
			name: "deny before allow", statement: `[{"Effect":"Deny",
"Action":"*",
"Resource":"*"},
{"Effect":"Allow",
"Action":"*",
"Resource":"*"}]`,
			action: "s3:GetObject", resource: "*", allow: false, deny: true, problem: "",
		},
		{
			name: "list and case", statement: `{"Effect":"Allow",
"Action":["S3:g?t*",
"sqs:*"],
"Resource":["arn:aws:s3:::b/?",
"arn:aws:s3:::c/*"]}`,
			action: "s3:GetObject", resource: "arn:aws:s3:::b/k", allow: true, deny: false, problem: "",
		},
		{
			name:      "resource case",
			statement: `{"Effect":"Allow","Action":"*","Resource":"arn:aws:s3:::B/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::b/k",
			allow:     false,
			deny:      false,
			problem:   "",
		},
		{
			name:      "wildcard slash",
			statement: `{"Effect":"Allow","Action":"*","Resource":"arn:aws:s3:::b/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::b/folder/k",
			allow:     true,
			deny:      false,
			problem:   "",
		},
		{
			name:      "not action",
			statement: `{"Effect":"Allow","NotAction":["iam:*","s3:Delete*"],"Resource":"*"}`,
			action:    "s3:GetObject",
			resource:  "*",
			allow:     true,
			deny:      false,
			problem:   "",
		},
		{
			name:      "not action excluded",
			statement: `{"Effect":"Allow","NotAction":"s3:Delete*","Resource":"*"}`,
			action:    "s3:DeleteObject",
			resource:  "*",
			allow:     false,
			deny:      false,
			problem:   "",
		},
		{
			name:      "not resource",
			statement: `{"Effect":"Allow","Action":"*","NotResource":"arn:aws:s3:::private/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::public/k",
			allow:     true,
			deny:      false,
			problem:   "",
		},
		{
			name:      "not resource excluded",
			statement: `{"Effect":"Allow","Action":"*","NotResource":"arn:aws:s3:::private/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::private/k",
			allow:     false,
			deny:      false,
			problem:   "",
		},
		{
			name:      "both action",
			statement: `{"Effect":"Allow","Action":"*","NotAction":"iam:*","Resource":"*"}`,
			action:    "s3:GetObject",
			resource:  "*",
			allow:     false,
			deny:      false,
			problem:   "invalid Action",
		},
		{
			name:      "both resource",
			statement: `{"Effect":"Allow","Action":"*","Resource":"*","NotResource":"x"}`,
			action:    "s3:GetObject",
			resource:  "*",
			allow:     false,
			deny:      false,
			problem:   "invalid Action",
		},
		{
			name:      "missing resource",
			statement: `{"Effect":"Allow","Action":"*"}`,
			action:    "s3:GetObject",
			resource:  "*",
			allow:     false,
			deny:      false,
			problem:   "invalid Action",
		},
		{
			name:      "empty action",
			statement: `{"Effect":"Allow","Action":[],"Resource":"*"}`,
			action:    "s3:GetObject",
			resource:  "*",
			allow:     false,
			deny:      false,
			problem:   "invalid Action",
		},
		{
			name:      "bad effect",
			statement: `{"Effect":"allow","Action":"*","Resource":"*"}`,
			action:    "s3:GetObject",
			resource:  "*",
			allow:     false,
			deny:      false,
			problem:   "unsupported",
		},
		{
			name:      "principal",
			statement: `{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*"}`,
			action:    "s3:GetObject",
			resource:  "*",
			allow:     false,
			deny:      false,
			problem:   "unsupported",
		},
		{
			name: "misspelled condition", statement: `{"Effect":"Allow",
"Action":"*",
"Resource":"*",
"Conditions":{"Bool":{"aws:SecureTransport":"true"}}}`,
			action: "s3:GetObject", resource: "*", allow: false, deny: false, problem: "unsupported",
		},
		// AWS's S3 home-directory policy example, Condition operators guide.
		{
			name:      "AWS username home",
			statement: `{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::b/home/${aws:username}/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::b/home/alice/k",
			allow:     true,
			deny:      false,
			problem:   "",
		},
		{
			name:      "variable name case-insensitive",
			statement: `{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::b/home/${aws:UserName}/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::b/home/alice/k",
			allow:     true,
			deny:      false,
			problem:   "",
		},
		{
			name:      "variable not another user",
			statement: `{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::b/home/${aws:username}/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::b/home/bob/k",
			allow:     false,
			deny:      false,
			problem:   "",
		},
		{
			name:      "unknown variable",
			statement: `{"Effect":"Allow","Action":"*","Resource":"arn:aws:s3:::b/${aws:userid}/*"}`,
			action:    "s3:GetObject",
			resource:  "arn:aws:s3:::b/id/k",
			allow:     false,
			deny:      false,
			problem:   "policy variable",
		},
		{
			name: "unknown condition", statement: `{"Effect":"Allow",
"Action":"*",
"Resource":"*",
"Condition":{"BinaryEquals":{"aws:username":"alice"}}}`,
			action: "s3:GetObject", resource: "*", allow: false, deny: false, problem: "BinaryEquals",
		},
		{
			name: "unknown key IfExists", statement: `{"Effect":"Allow",
"Action":"*",
"Resource":"*",
"Condition":{"StringEqualsIfExists":{"aws:Unknown":"x"}}}`,
			action: "s3:GetObject", resource: "*", allow: false, deny: false, problem: "aws:Unknown",
		},
		{
			name: "unsupported deny doesn't match", statement: `[{"Effect":"Allow",
"Action":"*",
"Resource":"*"},
{"Effect":"Deny",
"Action":"*",
"Resource":"*",
"Condition":{"Unknown":{"aws:username":"alice"}}}]`,
			action: "s3:GetObject", resource: "*", allow: true, deny: false, problem: "Unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := `{"Version":"2012-10-17","Statement":` + test.statement + `}`
			got := Evaluate([]Document{{Name: "test", JSON: doc}}, test.action, test.resource,
				Context{Values: map[string][]string{"aws:username": {"alice"}}})
			if got.Allowed != test.allow || got.ExplicitDeny != test.deny {
				t.Fatalf("got %+v", got)
			}

			if test.problem != "" && !strings.Contains(strings.Join(got.Unsupported, ";"), test.problem) {
				t.Fatalf("missing %q: %+v", test.problem, got)
			}
		})
	}
}

func TestConditions(t *testing.T) {
	tests := []struct {
		want                any
		name, operator, key string
		actual              []string
		allow               bool
	}{
		{
			name:     "equals",
			operator: "StringEquals",
			key:      "aws:username",
			actual:   []string{"alice"},
			want:     "alice",
			allow:    true,
		},
		{
			name:     "case mismatch",
			operator: "StringEquals",
			key:      "aws:username",
			actual:   []string{"Alice"},
			want:     "alice",
			allow:    false,
		},
		{
			name:     "ignore case",
			operator: "StringEqualsIgnoreCase",
			key:      "aws:username",
			actual:   []string{"Alice"},
			want:     "alice",
			allow:    true,
		},
		{
			name:     "not equals NOR",
			operator: "StringNotEquals",
			key:      "aws:username",
			actual:   []string{"alice"},
			want:     []string{"alice", "bob"},
			allow:    false,
		},
		{
			name:     "not equals",
			operator: "StringNotEquals",
			key:      "aws:username",
			actual:   []string{"carol"},
			want:     []string{"alice", "bob"},
			allow:    true,
		},
		{
			name:     "not equals ignore case",
			operator: "StringNotEqualsIgnoreCase",
			key:      "aws:username",
			actual:   []string{"Alice"},
			want:     "alice",
			allow:    false,
		},
		{
			name:     "like",
			operator: "StringLike",
			key:      "aws:username",
			actual:   []string{"alice"},
			want:     "a?i*",
			allow:    true,
		},
		{
			name:     "not like",
			operator: "StringNotLike",
			key:      "aws:username",
			actual:   []string{"alice"},
			want:     "b*",
			allow:    true,
		},
		{
			name:     "like ignore case",
			operator: "StringLikeIgnoreCase",
			key:      "aws:username",
			actual:   []string{"ALICE"},
			want:     "a*",
			allow:    true,
		},
		{
			name:     "not like ignore case",
			operator: "StringNotLikeIgnoreCase",
			key:      "aws:username",
			actual:   []string{"ALICE"},
			want:     "a*",
			allow:    false,
		},
		{
			name:     "arn equals wildcard",
			operator: "ArnEquals",
			key:      "aws:PrincipalArn",
			actual:   []string{"arn:aws:iam::123:user/alice"},
			want:     "arn:aws:iam::*:user/*",
			allow:    true,
		},
		{
			name:     "arn like",
			operator: "ArnLike",
			key:      "aws:PrincipalArn",
			actual:   []string{"arn:aws:iam::123:role/app"},
			want:     "arn:aws:iam::*:role/*",
			allow:    true,
		},
		{
			name:     "arn not equals",
			operator: "ArnNotEquals",
			key:      "aws:PrincipalArn",
			actual:   []string{"arn:aws:iam::123:role/app"},
			want:     "arn:aws:iam::*:role/*",
			allow:    false,
		},
		{
			name:     "arn not like",
			operator: "ArnNotLike",
			key:      "aws:PrincipalArn",
			actual:   []string{"arn:aws:iam::123:role/app"},
			want:     "arn:aws:iam::*:user/*",
			allow:    true,
		},
		{name: "bool", operator: "Bool", key: "aws:SecureTransport", actual: []string{"true"}, want: true, allow: true},
		{
			name:     "bool false",
			operator: "Bool",
			key:      "aws:SecureTransport",
			actual:   []string{"false"},
			want:     "true",
			allow:    false,
		},
		{
			name:     "bad bool",
			operator: "Bool",
			key:      "aws:SecureTransport",
			actual:   []string{"true"},
			want:     "1",
			allow:    false,
		},
		{name: "null absent", operator: "Null", key: "aws:username", actual: nil, want: "true", allow: true},
		{
			name:     "null present",
			operator: "Null",
			key:      "aws:username",
			actual:   []string{"alice"},
			want:     false,
			allow:    true,
		},
		{
			name:     "null mismatch",
			operator: "Null",
			key:      "aws:username",
			actual:   []string{"alice"},
			want:     true,
			allow:    false,
		},
		{
			name:     "numeric equals",
			operator: "NumericEquals",
			key:      "aws:userid",
			actual:   []string{"42"},
			want:     42,
			allow:    true,
		},
		{
			name:     "numeric not equals",
			operator: "NumericNotEquals",
			key:      "aws:userid",
			actual:   []string{"42"},
			want:     []int{42, 43},
			allow:    false,
		},
		{
			name:     "numeric lt",
			operator: "NumericLessThan",
			key:      "aws:userid",
			actual:   []string{"41.5"},
			want:     42,
			allow:    true,
		},
		{
			name:     "numeric le",
			operator: "NumericLessThanEquals",
			key:      "aws:userid",
			actual:   []string{"42"},
			want:     42,
			allow:    true,
		},
		{
			name:     "numeric gt",
			operator: "NumericGreaterThan",
			key:      "aws:userid",
			actual:   []string{"43"},
			want:     42,
			allow:    true,
		},
		{
			name:     "numeric ge",
			operator: "NumericGreaterThanEquals",
			key:      "aws:userid",
			actual:   []string{"42"},
			want:     42,
			allow:    true,
		},
		{
			name:     "invalid numeric",
			operator: "NumericNotEquals",
			key:      "aws:userid",
			actual:   []string{"id"},
			want:     42,
			allow:    false,
		},
		{name: "nan", operator: "NumericNotEquals", key: "aws:userid", actual: []string{"NaN"}, want: 42, allow: false},
		{
			name:     "date equals",
			operator: "DateEquals",
			key:      "aws:CurrentTime",
			actual:   []string{"2020-01-01T00:00:00Z"},
			want:     "2020-01-01T01:00:00+01:00",
			allow:    true,
		},
		{
			name:     "date not equals",
			operator: "DateNotEquals",
			key:      "aws:CurrentTime",
			actual:   []string{"2021-01-01T00:00:00Z"},
			want:     "2020-01-01T00:00:00Z",
			allow:    true,
		},
		{
			name:     "date lt",
			operator: "DateLessThan",
			key:      "aws:CurrentTime",
			actual:   []string{"2020-01-01T00:00:00Z"},
			want:     "2021-01-01T00:00:00Z",
			allow:    true,
		},
		{
			name:     "date le",
			operator: "DateLessThanEquals",
			key:      "aws:CurrentTime",
			actual:   []string{"2020-01-01T00:00:00Z"},
			want:     1577836800,
			allow:    true,
		},
		{
			name:     "date gt",
			operator: "DateGreaterThan",
			key:      "aws:CurrentTime",
			actual:   []string{"2021-01-01T00:00:00Z"},
			want:     1577836800,
			allow:    true,
		},
		{
			name:     "date ge",
			operator: "DateGreaterThanEquals",
			key:      "aws:CurrentTime",
			actual:   []string{"2020-01-01T00:00:00Z"},
			want:     1577836800,
			allow:    true,
		},
		{
			name:     "bad date",
			operator: "DateNotEquals",
			key:      "aws:CurrentTime",
			actual:   []string{"today"},
			want:     42,
			allow:    false,
		},
		// AWS Condition operators guide's example CIDRs, with excluded IPs.
		{
			name:     "AWS IPv4",
			operator: "IpAddress",
			key:      "aws:SourceIp",
			actual:   []string{"203.0.113.10"},
			want:     "203.0.113.0/24",
			allow:    true,
		},
		{
			name:     "AWS excluded IPv4",
			operator: "NotIpAddress",
			key:      "aws:SourceIp",
			actual:   []string{"203.0.113.10"},
			want:     []string{"203.0.113.10/32", "203.0.113.11/32"},
			allow:    false,
		},
		{
			name:     "IPv6",
			operator: "IpAddress",
			key:      "aws:SourceIp",
			actual:   []string{"2001:db8::1"},
			want:     "2001:db8::/64",
			allow:    true,
		},
		{
			name:     "literal IP",
			operator: "IpAddress",
			key:      "aws:SourceIp",
			actual:   []string{"127.0.0.1"},
			want:     "127.0.0.1",
			allow:    true,
		},
		{
			name:     "invalid IP",
			operator: "NotIpAddress",
			key:      "aws:SourceIp",
			actual:   []string{"bad"},
			want:     "127.0.0.1",
			allow:    false,
		},
		{
			name:     "absent positive",
			operator: "StringEquals",
			key:      "aws:username",
			actual:   nil,
			want:     "alice",
			allow:    false,
		},
		{
			name:     "absent negative",
			operator: "StringNotEquals",
			key:      "aws:username",
			actual:   nil,
			want:     "alice",
			allow:    true,
		},
		{
			name:     "IfExists",
			operator: "StringEqualsIfExists",
			key:      "aws:username",
			actual:   nil,
			want:     "alice",
			allow:    true,
		},
		{
			name:     "negative IfExists",
			operator: "StringNotEqualsIfExists",
			key:      "aws:username",
			actual:   nil,
			want:     "alice",
			allow:    true,
		},
		{
			name:     "IfExists present",
			operator: "StringEqualsIfExists",
			key:      "aws:username",
			actual:   []string{"bob"},
			want:     "alice",
			allow:    false,
		},
		{
			name:     "any absent",
			operator: "ForAnyValue:StringEquals",
			key:      "aws:username",
			actual:   nil,
			want:     "alice",
			allow:    false,
		},
		{
			name:     "all absent vacuous",
			operator: "ForAllValues:StringEquals",
			key:      "aws:username",
			actual:   nil,
			want:     "alice",
			allow:    true,
		},
		{
			name:     "any",
			operator: "ForAnyValue:StringEquals",
			key:      "aws:username",
			actual:   []string{"alice", "bob"},
			want:     "alice",
			allow:    true,
		},
		{
			name:     "all",
			operator: "ForAllValues:StringEquals",
			key:      "aws:username",
			actual:   []string{"alice", "bob"},
			want:     []string{"alice", "bob"},
			allow:    true,
		},
		{
			name:     "all fails",
			operator: "ForAllValues:StringEquals",
			key:      "aws:username",
			actual:   []string{"alice", "bob"},
			want:     "alice",
			allow:    false,
		},
		{
			name:     "any negative",
			operator: "ForAnyValue:StringNotEquals",
			key:      "aws:username",
			actual:   []string{"alice", "bob"},
			want:     "alice",
			allow:    true,
		},
		{
			name:     "all negative",
			operator: "ForAllValues:StringNotEquals",
			key:      "aws:username",
			actual:   []string{"alice", "bob"},
			want:     "alice",
			allow:    false,
		},
		{
			name:     "set IfExists",
			operator: "ForAnyValue:StringEqualsIfExists",
			key:      "aws:username",
			actual:   nil,
			want:     "alice",
			allow:    true,
		},
		{
			name:     "key case",
			operator: "StringEquals",
			key:      "AWS:UserName",
			actual:   []string{"alice"},
			want:     "alice",
			allow:    true,
		},
		{
			name:     "AWS principal tag example",
			operator: "StringEquals",
			key:      "aws:PrincipalTag/job-category",
			actual:   []string{"iamuser-admin"},
			want:     "iamuser-admin",
			allow:    true,
		},
		{
			name:     "known absent principal tag",
			operator: "Null",
			key:      "aws:PrincipalTag/missing",
			actual:   nil,
			want:     true,
			allow:    true,
		},
		{
			name:     "unknown resource tag",
			operator: "Null",
			key:      "aws:ResourceTag/missing",
			actual:   nil,
			want:     true,
			allow:    false,
		},
		{name: "unknown key", operator: "Null", key: "aws:Unknown", actual: nil, want: true, allow: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			condition, err := json.Marshal(map[string]any{test.operator: map[string]any{test.key: test.want}})
			if err != nil {
				t.Fatal(err)
			}

			doc := fmt.Sprintf(`{"Statement":{"Effect":"Allow","Action":"*","Resource":"*","Condition":%s}}`, condition)
			got := Evaluate([]Document{{Name: "condition", JSON: doc}}, "s3:GetObject", "*",
				Context{Values: map[string][]string{test.key: test.actual}})
			if got.Allowed != test.allow {
				t.Fatalf("expected %t, got %+v", test.allow, got)
			}
		})
	}
}

func TestPolicySourcesAndDiagnostics(t *testing.T) {
	docs := []Document{
		{Name: "inline", JSON: `{"Statement":{"Sid":"read","Effect":"Allow","Action":"*","Resource":"*"}}`},
		{Name: "group", JSON: `{"Statement":{"Sid":"deny","Effect":"Deny","Action":"s3:*","Resource":"*"}}`},
		{Name: "broken", JSON: `not JSON`},
	}

	got := Evaluate(docs, "s3:GetObject", "*", Context{})
	if got.Allowed || !got.ExplicitDeny || len(got.Matched) != 2 || len(got.Unsupported) != 1 {
		t.Fatalf("got %+v", got)
	}

	// The AWS time-window example requires both date bounds (AND).
	doc := `{"Statement":{"Effect":"Allow","Action":"*","Resource":"*","Condition":{
		"DateGreaterThan":{"aws:CurrentTime":"2020-01-01T00:00:00Z"},
		"DateLessThan":{"aws:CurrentTime":"2021-01-01T00:00:00Z"},
		"Null":{"aws:username":"false"}}}}`
	for _, current := range []string{"2020-06-01T00:00:00Z", "2022-01-01T00:00:00Z"} {
		got := Evaluate([]Document{{JSON: doc}}, "s3:GetObject", "*", Context{Values: map[string][]string{
			"aws:currenttime": {current}, "aws:username": {"alice"},
		}})
		if got.Allowed != strings.HasPrefix(current, "2020") {
			t.Fatalf("got %+v for %s", got, current)
		}
	}

	doc = `{"Statement":{"Effect":"Allow","Action":"*","Resource":"*","Condition":{
		"Null":{"aws:ResourceTag/missing":true}}}}`
	if !Evaluate([]Document{{JSON: doc}}, "s3:GetObject", "*", Context{ResourceTags: true}).Allowed {
		t.Fatal("known absent resource tags should support Null")
	}

	for _, version := range []string{"", "2008-10-17", "2012-10-17"} {
		doc := fmt.Sprintf(`{"Version":%q,
"Statement":{"Effect":"Allow",
"Action":"*", "Resource":"arn:aws:s3:::b/${aws:username}/*"}}`, version)
		got := Evaluate([]Document{{JSON: doc}}, "s3:GetObject", "arn:aws:s3:::b/alice/k",
			Context{Values: map[string][]string{"aws:username": {"alice"}}})
		if got.Allowed != (version == "2012-10-17") {
			t.Fatalf("version %s: %+v", version, got)
		}
	}
}

func TestMalformedNegativeElementsCannotAllow(t *testing.T) {
	for _, statement := range []string{
		`{"Effect":"Allow","NotAction":"","Resource":"*"}`,
		`{"Effect":"Allow","Action":"*","NotResource":""}`,
		`{"Effect":"Allow","Action":"*","Resource":"*","Condition":{"NumericNotEquals":{"aws:username":"bad"}}}`,
		`{"Effect":"Allow","Action":"*","Resource":"*","Condition":{"BoolIfExists":{"aws:SecureTransport":"bad"}}}`,
		`{"Effect":"Allow","Action":"*","Resource":"*","Condition":{"NotIpAddress":{"aws:SourceIp":"bad"}}}`,
	} {
		doc := `{"Statement":` + statement + `}`
		got := Evaluate([]Document{{JSON: doc}}, "s3:GetObject", "*", Context{})
		if got.Allowed || len(got.Unsupported) == 0 {
			t.Fatalf("malformed statement grants access: %+v", got)
		}
	}
}
