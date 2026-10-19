package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	httpapi "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	httptypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

// iamExecutionAPI publishes both literal and parameterized IAM routes. Lambda
// returns its event so the same requests check resolved proxy caller identities.
func iamExecutionAPI(t *testing.T, s *httptest.Server, kind string) string {
	t.Helper()
	ctx := context.Background()
	fn := createFunction(t, lambdaClient(s, "test"), "iam-events", "app.respond")
	if kind == "REST" {
		c := restClient(s)
		api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("iam")})
		mustAuthorize(t, err)
		pets, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
			RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: new("pets"),
		})
		mustAuthorize(t, err)
		pet, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
			RestApiId: api.Id, ParentId: pets.Id, PathPart: new("{petId}"),
		})
		mustAuthorize(t, err)
		for _, resource := range []*string{api.RootResourceId, pets.Id, pet.Id} {
			for _, method := range []string{"GET", "POST"} {
				_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
					RestApiId: api.Id, ResourceId: resource, HttpMethod: new(method), AuthorizationType: new("AWS_IAM"),
				})
				mustAuthorize(t, err)
				_, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
					RestApiId:             api.Id,
					ResourceId:            resource,
					HttpMethod:            new(method),
					Type:                  resttypes.IntegrationTypeAwsProxy,
					IntegrationHttpMethod: new("POST"),
					Uri:                   lambdaURI(fn.FunctionArn),
				})
				mustAuthorize(t, err)
			}
		}

		_, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{RestApiId: api.Id, StageName: new("v1")})
		mustAuthorize(t, err)
		return aws.ToString(api.Id)
	}

	c := apigwClient(s)
	api, err := c.CreateApi(ctx, &httpapi.CreateApiInput{Name: new("iam"), ProtocolType: httptypes.ProtocolTypeHttp})
	mustAuthorize(t, err)
	version := strings.TrimPrefix(kind, "HTTP")
	integration, err := c.CreateIntegration(ctx, &httpapi.CreateIntegrationInput{
		ApiId: api.ApiId, IntegrationType: httptypes.IntegrationTypeAwsProxy,
		IntegrationUri: fn.FunctionArn, PayloadFormatVersion: new(version),
	})
	mustAuthorize(t, err)
	for _, route := range []string{"GET /", "GET /pets", "POST /pets", "ANY /pets/{petId}", "$default"} {
		_, err = c.CreateRoute(ctx, &httpapi.CreateRouteInput{
			ApiId: api.ApiId, RouteKey: new(route), AuthorizationType: httptypes.AuthorizationTypeAwsIam,
			Target: new("integrations/" + aws.ToString(integration.IntegrationId)),
		})
		mustAuthorize(t, err)
	}

	for _, stage := range []string{"v1", "$default"} {
		_, err = c.CreateStage(ctx, &httpapi.CreateStageInput{
			ApiId: api.ApiId, StageName: new(stage), AutoDeploy: new(true),
		})
		mustAuthorize(t, err)
	}

	return aws.ToString(api.ApiId)
}

func signedExecuteAPI(t *testing.T, s *httptest.Server, id, method, path string,
	credentials *aws.Credentials, presign, corrupt bool,
) (status int, responseBody string) {
	t.Helper()
	ctx := context.Background()
	payload := "signed payload"
	req, err := http.NewRequestWithContext(ctx, method, s.URL+"/_aws/execute-api/"+id+path,
		strings.NewReader(payload))
	mustAuthorize(t, err)
	if credentials != nil && credentials.AccessKeyID != "" {
		signer := v4.NewSigner()
		digest := sha256.Sum256([]byte(payload))
		hash := hex.EncodeToString(digest[:])
		if presign {
			req.URL.RawQuery = "X-Amz-Expires=60"
			signed, headers, signErr := signer.PresignHTTP(
				ctx,
				*credentials,
				req,
				hash,
				"execute-api",
				"us-east-1",
				time.Now(),
			)
			mustAuthorize(t, signErr)
			req, err = http.NewRequestWithContext(ctx, method, signed, strings.NewReader(payload))
			mustAuthorize(t, err)
			req.Header = headers
		} else {
			mustAuthorize(t, signer.SignHTTP(ctx, *credentials, req, hash, "execute-api", "us-east-1", time.Now()))
		}

		if corrupt {
			auth := req.Header.Get("Authorization")
			replacement := "0"
			if strings.HasSuffix(auth, replacement) {
				replacement = "1"
			}

			req.Header.Set("Authorization", auth[:len(auth)-1]+replacement)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	mustAuthorize(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	mustAuthorize(t, err)
	return resp.StatusCode, string(body)
}

func TestAPIGatewayIAMAuthorization(t *testing.T) {
	for _, kind := range []string{"REST", "HTTP1.0", "HTTP2.0"} {
		for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft, config.AuthorizationOff} {
			for _, verify := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/verify=%t", kind, mode, verify), func(t *testing.T) {
					s := authorizationServer(t, mode, verify)
					id := iamExecutionAPI(t, s, kind)
					key := authorizationUser(t, s)
					credentials, err := authorizationProvider(key).Retrieve(context.Background())
					mustAuthorize(t, err)
					base := "arn:aws:execute-api:us-east-1:000000000000:" + id
					authorizationPolicy(t, s, fmt.Sprintf(
						`{"Statement":[{"Effect":"Allow","Action":"execute-api:Invoke","Resource":[%q,%q,%q]},
       {"Effect":"Deny","Action":"execute-api:Invoke","Resource":%q}]}`,
						base+"/v1/GET/pets", base+"/v1/GET/pets/a1b2", base+"/v1/GET/", base+"/v1/POST/pets",
					))
					for _, tc := range []struct {
						method, path, resource              string
						allowed, presign, unsigned, corrupt bool
					}{
						{"GET", "/v1/pets", "/v1/GET/pets", true, false, false, false},
						{"GET", "/v1/pets", "/v1/GET/pets", true, true, false, false},
						{"GET", "/v1/pets/a1b2", "/v1/GET/pets/a1b2", true, false, false, false},
						{"GET", "/v1/", "/v1/GET/", true, false, false, false},
						{"GET", "/v1/pets/other", "/v1/GET/pets/other", false, false, false, false},
						{"POST", "/v1/pets", "/v1/POST/pets", false, false, false, false},
						{"GET", "/v1/pets", "/v1/GET/pets", false, false, true, false},
						{"GET", "/v1/pets", "/v1/GET/pets", true, false, false, true},
					} {
						creds := &credentials
						if tc.unsigned {
							creds = nil
						}

						status, body := signedExecuteAPI(t, s, id, tc.method, tc.path, creds, tc.presign, tc.corrupt)
						want := http.StatusCreated
						denied := !tc.allowed && mode == config.AuthorizationEnforce && !tc.unsigned
						if denied || tc.unsigned || (tc.corrupt && verify) {
							want = http.StatusForbidden
						}

						if status != want {
							t.Fatalf("%s %s presign=%t corrupt=%t: status=%d want=%d body=%s",
								tc.method, tc.path, tc.presign, tc.corrupt, status, want, body)
						}

						if denied {
							expected, _ := json.Marshal(map[string]string{
								"Message": "User: arn:aws:iam::000000000000:user/apps/alice" +
									" is not authorized to perform: execute-api:Invoke on resource: " +
									base + tc.resource,
							})
							if body != string(expected) {
								t.Fatalf("denial: %s want %s", body, expected)
							}
						}

						if status == http.StatusCreated {
							var event struct {
								RequestContext struct {
									Identity   map[string]string
									Authorizer struct{ IAM map[string]string }
								}
							}
							mustAuthorize(t, json.Unmarshal([]byte(body), &event))
							identity := event.RequestContext.Identity
							if kind == "HTTP2.0" {
								identity = event.RequestContext.Authorizer.IAM
							}

							if identity["userArn"] != "arn:aws:iam::000000000000:user/apps/alice" ||
								identity["accountId"] != "000000000000" || identity["accessKey"] != credentials.AccessKeyID ||
								identity["user"] == "" || identity["caller"] != identity["user"] {
								t.Fatalf("caller identity: %+v", identity)
							}
						}
					}

					if kind != "REST" {
						// Default routes have a distinct ARN; the $default stage is independent
						// from the route key and must remain in ordinary method/path ARNs.
						authorizationPolicy(t, s, fmt.Sprintf(
							`{"Statement":{"Effect":"Allow","Action":"execute-api:Invoke","Resource":[%q,%q]}}`,
							base+"/*/$default", base+"/$default/GET/pets"))
						for _, path := range []string{"/v1/unmatched", "/unmatched", "/pets"} {
							status, body := signedExecuteAPI(t, s, id, "GET", path, &credentials, false, false)
							if status != http.StatusCreated {
								t.Fatalf("default routing %s: %d %s", path, status, body)
							}
						}

						// The default route's ARN names the stage it ran in.
						authorizationPolicy(t, s, fmt.Sprintf(
							`{"Statement":[{"Effect":"Allow","Action":"execute-api:Invoke","Resource":%q},`+
								`{"Effect":"Deny","Action":"execute-api:Invoke","Resource":%q}]}`,
							base+"/*/$default", base+"/v1/$default"))
						if status, body := signedExecuteAPI(t, s, id, "GET", "/unmatched", &credentials, false,
							false); status != http.StatusCreated {
							t.Fatalf("default stage default route: %d %s", status, body)
						}

						want := http.StatusCreated
						if mode == config.AuthorizationEnforce {
							want = http.StatusForbidden
						}

						if status, body := signedExecuteAPI(t, s, id, "GET", "/v1/unmatched", &credentials, false,
							false); status != want {
							t.Fatalf("v1 default route: %d %s", status, body)
						}
					}

					resp, err := http.Get(s.URL + "/_tinystack/requests")
					mustAuthorize(t, err)
					defer resp.Body.Close()
					var page struct {
						Items []gateway.Record `json:"requests"`
					}
					mustAuthorize(t, json.NewDecoder(resp.Body).Decode(&page))
					found := false
					for _, record := range page.Items {
						if record.Service != "execute-api" || record.Path != "/_aws/execute-api/"+id+"/v1/pets" ||
							record.Method != "POST" {
							continue
						}

						found = true
						if mode == config.AuthorizationOff {
							if record.Authorization != nil {
								t.Fatalf("off record: %+v", record)
							}
							continue
						}

						auth := record.Authorization
						if auth == nil || auth.Allowed || len(auth.Checks) != 1 ||
							auth.Checks[0].Resource != base+"/v1/POST/pets" || auth.Checks[0].Action != "execute-api:Invoke" {
							t.Fatalf("decision: %+v", record)
						}

						if mode == config.AuthorizationSoft &&
							(record.Status != http.StatusCreated || !strings.HasPrefix(auth.Message, "would deny:")) {
							t.Fatalf("soft record: %+v", record)
						}
					}

					if !found {
						t.Fatal("execute-api decision missing from request inspector")
					}
				})
			}
		}
	}
}
