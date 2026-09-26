package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/amjadjibon/tinystack/internal/config"
)

func ec2Client(s *httptest.Server, key, region string) *ec2.Client {
	return ec2.New(ec2.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// postEC2 sends an unsigned EC2 Query request to EC2's regional host name,
// which names the service, and returns the status and body.
func postEC2(t *testing.T, s *httptest.Server, form url.Values) (status int, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL,
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}

	req.Host = "ec2.us-east-1.amazonaws.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(data)
}

func TestEC2Regions(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "eu-west-1")
	ctx := context.Background()
	all, err := c.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil || len(all.Regions) < 20 {
		t.Fatalf("got %+v, %v", all, err)
	}

	named, err := c.DescribeRegions(ctx, &ec2.DescribeRegionsInput{RegionNames: []string{"us-west-2", "eu-west-1"}})
	if err != nil || len(named.Regions) != 2 || aws.ToString(named.Regions[0].RegionName) != "eu-west-1" ||
		aws.ToString(named.Regions[0].Endpoint) != "ec2.eu-west-1.amazonaws.com" ||
		aws.ToString(named.Regions[0].OptInStatus) != "opt-in-not-required" {
		t.Fatalf("got %+v, %v", named, err)
	}

	_, err = c.DescribeRegions(ctx, &ec2.DescribeRegionsInput{RegionNames: []string{"moon-1"}})
	code(t, err, "InvalidParameterValue")
	_, err = c.DescribeRegions(ctx, &ec2.DescribeRegionsInput{DryRun: aws.Bool(true)})
	code(t, err, "DryRunOperation")
}

// TestEC2QueryWire checks the EC2 Query envelopes: flat request lists, a
// response with its requestId and item-wrapped lists, and the Response/Errors
// error body.
func TestEC2QueryWire(t *testing.T) {
	s := server(t)
	status, body := postEC2(t, s, url.Values{
		"Action": {"DescribeRegions"}, "Version": {"2016-11-15"}, "RegionName.1": {"us-east-1"},
	})
	if status != 200 ||
		!strings.Contains(body, `<DescribeRegionsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15">`) ||
		!strings.Contains(body, "<requestId>") ||
		!strings.Contains(body, "<regionInfo><item><regionEndpoint>ec2.us-east-1.amazonaws.com</regionEndpoint>"+
			"<regionName>us-east-1</regionName><optInStatus>opt-in-not-required</optInStatus></item></regionInfo>") {
		t.Fatalf("%d %s", status, body)
	}

	for name, form := range map[string]url.Values{
		"unknown parameter": {"Action": {"DescribeRegions"}, "Version": {"2016-11-15"}, "Bogus": {"1"}},
		"wrong version":     {"Action": {"DescribeRegions"}, "Version": {"2012-11-05"}},
		"unknown action":    {"Action": {"LaunchRocket"}, "Version": {"2016-11-15"}},
	} {
		status, body = postEC2(t, s, form)
		if status != 400 || !strings.HasPrefix(body, `<?xml`) ||
			!strings.Contains(body, "<Response><Errors><Error><Code>") || !strings.Contains(body, "<RequestID>") {
			t.Fatalf("%s: %d %s", name, status, body)
		}
	}
}

func TestEC2Authentication(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	if _, err := ec2Client(s, "test", "us-east-1").DescribeRegions(ctx, &ec2.DescribeRegionsInput{}); err != nil {
		t.Fatal(err)
	}

	bad := ec2.New(ec2.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "wrong"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
	_, err := bad.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	code(t, err, "AuthFailure")
}
