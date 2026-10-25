package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

func TestSigV4aIdentity(t *testing.T) {
	for _, mode := range []string{config.IdentityParse, config.IdentityVerify} {
		for _, presigned := range []bool{false, true} {
			for _, regionSet := range []string{"ap-southeast-1,us-east-1", "*"} {
				cfg := config.Default()
				cfg.Identity.Mode = mode
				cfg.Identity.SecretKeys = map[string]string{"mapped": "secret"}
				cfg.Identity.Accounts = map[string]string{"mapped": "111111111111"}
				s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodGet, "http://localhost/", http.NoBody)
				if presigned {
					q := url.Values{
						"X-Amz-Algorithm":  {"AWS4-ECDSA-P256-SHA256"},
						"X-Amz-Credential": {"mapped/20261026/cloudfront-keyvaluestore/aws4_request"},
						"X-Amz-Expires":    {"60"},
						"X-Amz-Region-Set": {regionSet},
					}
					r.URL.RawQuery = q.Encode()
				} else {
					r.Header.Set(
						"Authorization",
						"AWS4-ECDSA-P256-SHA256 Credential=mapped/20261026/cloudfront-keyvaluestore/aws4_request, "+
							"SignedHeaders=host;x-amz-region-set, Signature=abcd",
					)
					r.Header.Set("X-Amz-Region-Set", regionSet)
				}
				m := request.Metadata{
					Scope: request.Scope{AccountID: cfg.Defaults.Account, Region: cfg.Defaults.Region},
				}
				service, err := s.identify(r, &m)
				if err != nil || service != "cloudfront-keyvaluestore" || m.AccountID != "111111111111" {
					t.Fatalf("identity: %+v %s %v", m, service, err)
				}
				wantRegion := "ap-southeast-1"
				if regionSet == "*" {
					wantRegion = cfg.Defaults.Region
				}
				if m.Region != wantRegion {
					t.Fatal(m.Region)
				}
				c := &call{meta: m}
				c.meta.Protocol = catalog.RESTJSON
				err = s.authenticate(httptest.NewRecorder(), r, c)
				if mode == config.IdentityParse && err != nil {
					t.Fatal(err)
				}
				if mode == config.IdentityVerify {
					var awsErr *awserror.Error
					if !errors.As(err, &awsErr) || awsErr.Code != "InvalidSignatureException" ||
						!strings.Contains(awsErr.Message, "SigV4a verification is not supported yet") {
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func TestSigV4aIssuedCredentials(t *testing.T) {
	cfg := config.Default()
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.UseCredentials(func(context.Context, string) (request.Credentials, bool, error) {
		return request.Credentials{AccountID: "222222222222"}, true, nil
	})
	r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	r.Header.Set(
		"Authorization",
		"AWS4-ECDSA-P256-SHA256 Credential=issued/20261026/cloudfront-keyvaluestore/aws4_request",
	)
	c := &call{}
	_, err = s.identify(r, &c.meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.resolveCredentials(r.Context(), c); err != nil {
		t.Fatal(err)
	}
	if c.meta.AccountID != "222222222222" {
		t.Fatal(c.meta.AccountID)
	}
}
