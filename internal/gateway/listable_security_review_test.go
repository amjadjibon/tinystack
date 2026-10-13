package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/iam/policy"
)

func TestMemberInventoryDoesNotExposeUnrelatedReadsOrDetails(t *testing.T) {
	for _, tc := range []struct {
		name, kind, document string
		count                int
	}{
		{"unrelated read", "unknown", `{"Statement":{"Effect":"Allow","Action":"ssm:GetParameter","Resource":"*"}}`, 0},
		{
			"list metadata only", "parameter",
			`{"Statement":{"Effect":"Allow","Action":"ssm:DescribeParameters","Resource":"*"}}`, 1,
		},
		{
			"source IP deny", "parameter",
			`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"},` +
				`{"Effect":"Deny","Action":"*","Resource":"*",` +
				`"Condition":{"IpAddress":{"aws:SourceIp":"192.0.2.0/24"}}}]}`, 0,
		},
		{
			"region deny", "parameter",
			`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"},` +
				`{"Effect":"Deny","Action":"*","Resource":"*",` +
				`"Condition":{"StringEquals":{"aws:RequestedRegion":"us-east-1"}}}]}`, 0,
		},
		{
			"resource tag deny", "parameter",
			`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"},` +
				`{"Effect":"Deny","Action":"*","Resource":"*",` +
				`"Condition":{"StringEquals":{"aws:ResourceTag/Restricted":"yes"}}}]}`, 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				services: map[string]*catalog.Service{
					"ssm": {Name: "ssm", IAMPrefix: "ssm", Operations: map[string]catalog.Operation{
						"DescribeParameters": {Name: "DescribeParameters"}, "GetParameter": {Name: "GetParameter"},
					}},
				},
			}
			s.cfg.Identity.Authorization = config.AuthorizationEnforce
			principal := &request.Principal{Kind: request.PrincipalRole, Account: "111111111111"}
			s.policies = func(context.Context, *request.Principal) (iam.Authorization, error) {
				return iam.Authorization{Principal: *principal, Documents: []policy.Document{{JSON: tc.document}}}, nil
			}
			out := s.listable(
				httptest.NewRequest("GET", "http://localhost/_tinystack/resources", http.NoBody),
				principal,
				[]catalog.Owned{{Service: "ssm", Resource: catalog.Resource{
					Type:    tc.kind,
					Account: principal.Account,
					Region:  "us-east-1",
					Name:    "password",
					ARN:     "arn:aws:ssm:us-east-1:111111111111:parameter/password",
					Tags:    map[string]string{"Restricted": "yes"},
					Details: map[string]any{"Value": "secret-only-GetParameter-may-read"},
				}}},
			)
			if len(out) != tc.count {
				t.Fatalf("returned %d resources, want %d", len(out), tc.count)
			}
			if len(out) != 0 && out[0].Details != nil {
				t.Fatal("list-only inventory exposed value/details")
			}
		})
	}
}

func TestMemberInventoryGlobalRegionConditions(t *testing.T) {
	s := &Server{
		services: map[string]*catalog.Service{"iam": {IAMPrefix: "iam", Operations: map[string]catalog.Operation{
			"ListRoles": {Name: "ListRoles"},
		}}},
	}
	s.cfg.Identity.Authorization = config.AuthorizationEnforce
	principal := &request.Principal{Kind: request.PrincipalRole, Account: "111111111111"}
	s.policies = func(context.Context, *request.Principal) (iam.Authorization, error) {
		return iam.Authorization{Principal: *principal, Documents: []policy.Document{{
			JSON: `{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"},` +
				`{"Effect":"Deny","Action":"*","Resource":"*","Condition":{"StringEquals":{"aws:RequestedRegion":"us-east-1"}}}]}`,
		}}}, nil
	}
	r := httptest.NewRequest(http.MethodGet, "http://localhost/_tinystack/resources", http.NoBody)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=key/20261013/us-east-1/tinystack/aws4_request")
	if out := s.listable(r, principal, []catalog.Owned{{Service: "iam", Resource: catalog.Resource{
		Type: "role", Account: principal.Account, ARN: "arn:aws:iam::111111111111:role/Private",
	}}}); len(out) != 0 {
		t.Fatal("global inventory bypassed the signed region condition")
	}
}
