package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// callURL sends a request to a function URL through TinyStack's host, by the
// URL's ID in the path, and returns the response and its body.
func callURL(
	t *testing.T, server, functionURL, method, path string, header http.Header,
) (resp *http.Response, body string) {
	t.Helper()
	u, err := url.Parse(functionURL)
	if err != nil {
		t.Fatal(err)
	}

	id, _, _ := strings.Cut(u.Hostname(), ".")
	req, err := http.NewRequest(method, server+"/_aws/lambda-url/"+id+path, strings.NewReader(`{"hi":1}`))
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range header {
		req.Header[k] = v
	}

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	return resp, string(data)
}

func TestLambdaFunctionURLs(t *testing.T) {
	s := server(t)
	c := lambdaClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "web", "app.respond")
	created, err := c.CreateFunctionUrlConfig(ctx, &sdklambda.CreateFunctionUrlConfigInput{
		FunctionName: aws.String("web"), AuthType: lambdatypes.FunctionUrlAuthTypeNone,
		Cors: &lambdatypes.Cors{
			AllowOrigins: []string{"http://localhost:3000"}, AllowMethods: []string{"GET", "POST"},
			AllowHeaders: []string{"content-type"}, ExposeHeaders: []string{"x-handler"}, MaxAge: aws.Int32(60),
		},
	})
	if err != nil || !strings.HasSuffix(aws.ToString(created.FunctionUrl), ".lambda-url.us-east-1.localhost:"+
		strings.Split(s.URL, ":")[2]+"/") || aws.ToString(created.FunctionArn) !=
		"arn:aws:lambda:us-east-1:000000000000:function:web" || created.InvokeMode != lambdatypes.InvokeModeBuffered {
		t.Fatalf("created: %+v %v", created, err)
	}

	var conflict *lambdatypes.ResourceConflictException
	if _, err = c.CreateFunctionUrlConfig(ctx, &sdklambda.CreateFunctionUrlConfigInput{
		FunctionName: aws.String("web"), AuthType: lambdatypes.FunctionUrlAuthTypeNone,
	}); !errors.As(err, &conflict) {
		t.Fatalf("second URL: %v", err)
	}

	origin := http.Header{"Origin": {"http://localhost:3000"}}
	resp, body := callURL(t, s.URL, aws.ToString(created.FunctionUrl), http.MethodPost, "/orders?id=7", origin)
	var event struct {
		Version        string            `json:"version"`
		RawPath        string            `json:"rawPath"`
		RawQueryString string            `json:"rawQueryString"`
		Body           string            `json:"body"`
		Headers        map[string]string `json:"headers"`
		RequestContext struct {
			HTTP struct {
				Method string `json:"method"`
			} `json:"http"`
		} `json:"requestContext"`
	}
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Handler") != "respond" ||
		resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" ||
		json.Unmarshal([]byte(body), &event) != nil || event.Version != "2.0" || event.RawPath != "/orders" ||
		event.RawQueryString != "id=7" || event.Body != `{"hi":1}` || event.RequestContext.HTTP.Method != "POST" {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}

	// The URL's own host reaches it too.
	byHost, err := http.NewRequest(http.MethodGet, s.URL+"/status", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	functionURL, err := url.Parse(aws.ToString(created.FunctionUrl))
	if err != nil {
		t.Fatal(err)
	}

	byHost.Host = functionURL.Host
	hostResp, err := http.DefaultClient.Do(byHost)
	if err != nil {
		t.Fatal(err)
	}

	_ = hostResp.Body.Close()
	if hostResp.StatusCode != http.StatusCreated {
		t.Fatalf("by host: %d", hostResp.StatusCode)
	}

	preflight := http.Header{"Origin": {"http://localhost:3000"}, "Access-Control-Request-Method": {"POST"}}
	resp, _ = callURL(t, s.URL, aws.ToString(created.FunctionUrl), http.MethodOptions, "/", preflight)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Methods") != "GET,POST" ||
		resp.Header.Get("Access-Control-Max-Age") != "60" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}

	// An alias gets its own URL, which requires signed requests with AWS_IAM.
	version, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: aws.String("web")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateAlias(ctx, &sdklambda.CreateAliasInput{
		FunctionName: aws.String("web"), Name: aws.String("live"), FunctionVersion: version.Version,
	}); err != nil {
		t.Fatal(err)
	}

	live, err := c.CreateFunctionUrlConfig(ctx, &sdklambda.CreateFunctionUrlConfigInput{
		FunctionName: aws.String("web"), Qualifier: aws.String("live"),
		AuthType: lambdatypes.FunctionUrlAuthTypeAwsIam,
	})
	if err != nil || aws.ToString(live.FunctionArn) != "arn:aws:lambda:us-east-1:000000000000:function:web:live" {
		t.Fatalf("alias URL: %+v %v", live, err)
	}

	if resp, _ = callURL(t, s.URL, aws.ToString(live.FunctionUrl), http.MethodGet, "/", nil); resp.StatusCode != 403 {
		t.Fatalf("unsigned AWS_IAM request: %d", resp.StatusCode)
	}

	var invalid *lambdatypes.InvalidParameterValueException
	if _, err = c.CreateFunctionUrlConfig(ctx, &sdklambda.CreateFunctionUrlConfigInput{
		FunctionName: aws.String("web"), Qualifier: version.Version, AuthType: lambdatypes.FunctionUrlAuthTypeNone,
	}); !errors.As(err, &invalid) {
		t.Fatalf("version URL: %v", err)
	}

	updated, err := c.UpdateFunctionUrlConfig(ctx, &sdklambda.UpdateFunctionUrlConfigInput{
		FunctionName: aws.String("web"), Qualifier: aws.String("live"), AuthType: lambdatypes.FunctionUrlAuthTypeNone,
	})
	if err != nil || updated.AuthType != lambdatypes.FunctionUrlAuthTypeNone ||
		aws.ToString(updated.FunctionUrl) != aws.ToString(live.FunctionUrl) {
		t.Fatalf("updated: %+v %v", updated, err)
	}

	if resp, _ = callURL(t, s.URL, aws.ToString(live.FunctionUrl), http.MethodGet, "/", nil); resp.StatusCode != 201 {
		t.Fatalf("after update: %d", resp.StatusCode)
	}

	list, err := c.ListFunctionUrlConfigs(ctx, &sdklambda.ListFunctionUrlConfigsInput{FunctionName: aws.String("web")})
	if err != nil || len(list.FunctionUrlConfigs) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}

	// Deleting the alias deletes its URL, and the URL stops answering.
	if _, err = c.DeleteAlias(ctx, &sdklambda.DeleteAliasInput{
		FunctionName: aws.String("web"), Name: aws.String("live"),
	}); err != nil {
		t.Fatal(err)
	}

	if resp, _ = callURL(t, s.URL, aws.ToString(live.FunctionUrl), http.MethodGet, "/", nil); resp.StatusCode != 404 {
		t.Fatalf("deleted alias URL: %d", resp.StatusCode)
	}

	if _, err = c.DeleteFunctionUrlConfig(ctx, &sdklambda.DeleteFunctionUrlConfigInput{
		FunctionName: aws.String("web"),
	}); err != nil {
		t.Fatal(err)
	}

	var missing *lambdatypes.ResourceNotFoundException
	if _, err = c.GetFunctionUrlConfig(ctx, &sdklambda.GetFunctionUrlConfigInput{
		FunctionName: aws.String("web"),
	}); !errors.As(err, &missing) {
		t.Fatalf("deleted: %v", err)
	}
}
