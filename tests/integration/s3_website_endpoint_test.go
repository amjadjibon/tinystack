package integration_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3WebsiteEndpoint(t *testing.T) {
	ctx := context.Background()
	srv := verifyingServer(t)
	c, bucket := checksumBucket(t, srv)
	putWebsite := func(cfg *s3types.WebsiteConfiguration) {
		t.Helper()
		_, err := c.PutBucketWebsite(ctx, &s3.PutBucketWebsiteInput{Bucket: bucket, WebsiteConfiguration: cfg})
		if err != nil {
			t.Fatal(err)
		}
	}
	putWebsite(&s3types.WebsiteConfiguration{
		IndexDocument: &s3types.IndexDocument{Suffix: new("index.html")},
		ErrorDocument: &s3types.ErrorDocument{Key: new("error.html")},
		RoutingRules: []s3types.RoutingRule{{
			Condition: &s3types.Condition{KeyPrefixEquals: new("old/"), HttpErrorCodeReturnedEquals: new("404")},
			Redirect:  &s3types.Redirect{ReplaceKeyPrefixWith: new("new/"), HttpRedirectCode: new("302")},
		}},
	})

	for key, value := range map[string]string{"index.html": "home", "about/index.html": "about", "error.html": "custom"} {
		putObject(t, c, *bucket, key, value, &s3.PutObjectInput{ContentType: new("text/html")})
	}

	uri := "/about/"
	putObject(t, c, *bucket, "link", "direct REST body", &s3.PutObjectInput{WebsiteRedirectLocation: &uri})
	host := *bucket + ".s3-website.us-east-1.localhost"
	httpClient := *srv.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	probe := func(method, host, key string, status int, body, location string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+key, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		req.Host = host
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		if resp.StatusCode != status || resp.Header.Get("Location") != location ||
			(body != "" && strings.TrimSpace(string(data)) != body) {
			t.Fatalf("%s %s: status %d body %q location %q", method, key, resp.StatusCode, data,
				resp.Header.Get("Location"))
		}
	}
	probe(http.MethodGet, host, "/", http.StatusForbidden, "", "")

	_, err := c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}

	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject",` +
		`"Resource":"arn:aws:s3:::` + *bucket + `/*"}]}`
	_, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: bucket, Policy: &policy})
	if err != nil {
		t.Fatal(err)
	}

	probe(http.MethodGet, host+":12345", "/", http.StatusOK, "home", "")
	probe(http.MethodHead, *bucket+".s3-website-us-east-1.localhost", "/", http.StatusOK, "", "")
	probe(http.MethodGet, host, "/about", http.StatusFound, "", "http://"+host+"/about/")
	probe(http.MethodGet, host, "/about/", http.StatusOK, "about", "")
	probe(http.MethodGet, host, "/absent", http.StatusNotFound, "custom", "")
	probe(http.MethodGet, host, "/old/info", http.StatusFound, "", "http://"+host+"/new/info")
	probe(http.MethodGet, host, "/link", http.StatusMovedPermanently, "", uri)
	probe(http.MethodPut, host, "/link", http.StatusMethodNotAllowed, "", "")

	if got, _ := readVersion(t, c, *bucket, "link", ""); got != "direct REST body" {
		t.Fatalf("REST body = %q", got)
	}

	virtual, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/link", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	virtual.Host = *bucket + ".localhost"
	virtual.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	status, body := send(t, srv, signedWithHeader(t, virtual, time.Now()))
	if status != http.StatusOK || body != "direct REST body" {
		t.Fatalf("virtual-hosted REST GET: %d %s", status, body)
	}

	putWebsite(&s3types.WebsiteConfiguration{RedirectAllRequestsTo: &s3types.RedirectAllRequestsTo{
		HostName: new("example.test"), Protocol: s3types.ProtocolHttps,
	}})
	probe(http.MethodGet, host, "/about", http.StatusMovedPermanently, "", "https://example.test/about")

	_, err = c.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: bucket, PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			RestrictPublicBuckets: new(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	probe(http.MethodGet, host, "/about", http.StatusForbidden, "", "")
}
