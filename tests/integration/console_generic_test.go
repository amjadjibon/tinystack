package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/aws/aws-sdk-go-v2/service/memorydb"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestConsoleGenericActions(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	_, err := kinesisClient(
		s,
	).CreateStream(ctx, &kinesis.CreateStreamInput{StreamName: new("console-stream"), ShardCount: new(int32(1))})
	if err != nil {
		t.Fatal(err)
	}

	put(t, ssmClient(s, "test", "us-east-1"), "/console/param", "value")
	createRepository(ctx, t, ecrClient(s, "test", "us-east-1"), "console/repo")
	_, err = route53Client(
		s,
	).CreateHostedZone(ctx, &route53.CreateHostedZoneInput{Name: new("console.example."), CallerReference: new("console")})
	if err != nil {
		t.Fatal(err)
	}

	createPool(t, cognitoClient(s, "test", "us-east-1"), "console-pool", &cognitoidentityprovider.CreateUserPoolInput{})
	_, err = cfnClient(
		s,
	).CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("console-stack"), TemplateBody: new(`{"Resources":{"Parameter":{
 "Type":"AWS::SSM::Parameter","Properties":{"Name":"stack-owned","Type":"String","Value":"x"}}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	settled(ctx, t, cfnClient(s), "console-stack")
	_, err = cacheClient(
		s,
	).CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId: new("console-cache"), Engine: new("redis"), CacheNodeType: new("cache.t4g.micro"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = memoryDBClient(
		s,
		"test",
		"us-east-1",
	).CreateCluster(ctx, &memorydb.CreateClusterInput{
		ClusterName: new("console-memory"), NodeType: new("db.r6g.large"), ACLName: new("open-access"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var capabilities struct {
		Deletes  map[string]struct{ Service, Operation, Field, Identifier string } `json:"resource_deletes"`
		Taggable []string                                                          `json:"taggable_services"`
	}

	if internal(t, s, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK {
		t.Fatal("capabilities")
	}

	if !slices.Contains(capabilities.Taggable, "ssm") || slices.Contains(capabilities.Taggable, "sts") ||
		len(capabilities.Deletes) != 8 {
		t.Fatal(capabilities)
	}

	records := genericInventory(t, s)
	for key, action := range capabilities.Deletes {
		t.Run(key, func(t *testing.T) {
			var resource genericInventoryResource
			for _, r := range records {
				if r.Service+"/"+r.Type == key && r.Name != "stack-owned" {
					resource = r
					break
				}
			}

			if resource.Name == "" {
				t.Fatalf("no inventory entry for %s: %+v", key, records)
			}

			id := resource.Name
			switch action.Identifier {
			case "arn":
				id = resource.ARN
			case "arn-suffix":
				id = resource.ARN[strings.LastIndex(resource.ARN, "/")+1:]
			}

			region := resource.Region
			if region == "" {
				region = "us-east-1"
			}

			consoleResponse(
				t,
				s,
				consoleLambdaRequest(
					t,
					s,
					action.Service,
					action.Operation,
					resource.Account,
					region,
					map[string]any{action.Field: id},
				),
				http.StatusOK,
			)
			for _, r := range genericInventory(t, s) {
				if r.ARN == resource.ARN {
					t.Fatalf("deleted resource remains: %+v", r)
				}
			}
		})
	}

	if _, ok := capabilities.Deletes["ssm/document"]; ok {
		t.Fatal("unmapped type advertised")
	}

	consoleResponse(
		t,
		s,
		consoleLambdaRequest(
			t,
			s,
			"ssm",
			"DeleteDocument",
			"000000000000",
			"us-east-1",
			map[string]any{"Name": "unmapped"},
		),
		http.StatusBadRequest,
	)
}

func TestConsoleGenericTagging(t *testing.T) {
	s := server(t)
	put(t, ssmClient(s, "test", "us-east-1"), "/console/tags", "v")
	arn := "arn:aws:ssm:us-east-1:000000000000:parameter/console/tags"
	for _, op := range []string{"TagResources", "UntagResources"} {
		input := map[string]any{"ResourceARNList": []string{arn}}
		if op == "TagResources" {
			input["Tags"] = map[string]string{"team": "console"}
		} else {
			input["TagKeys"] = []string{"team"}
		}

		req := consoleLambdaRequest(t, s, "resourcegroupstaggingapi", op, "000000000000", "us-east-1", input)
		out := consoleResponse(t, s, req, http.StatusOK)["output"].(map[string]any)
		if failed, ok := out["FailedResourcesMap"].(map[string]any); ok && len(failed) != 0 {
			t.Fatal(out)
		}

		found := false
		for _, r := range genericInventory(t, s) {
			if r.ARN == arn {
				found = true
				_, tagged := r.Tags["team"]
				if tagged != (op == "TagResources") {
					t.Fatal(r)
				}
			}
		}
		if !found {
			t.Fatal("tagged resource missing from inventory")
		}
	}

	missing := "arn:aws:ssm:us-east-1:000000000000:parameter/missing"
	req := consoleLambdaRequest(t, s, "resourcegroupstaggingapi", "TagResources", "000000000000", "us-east-1",
		map[string]any{"ResourceARNList": []string{arn, missing}, "Tags": map[string]string{"team": "console"}})
	output := consoleResponse(t, s, req, http.StatusOK)["output"].(map[string]any)
	failed := output["FailedResourcesMap"].(map[string]any)
	if len(failed) != 1 || failed[missing] == nil {
		t.Fatal(output)
	}

	consoleResponse(
		t,
		s,
		consoleLambdaRequest(
			t,
			s,
			"resourcegroupstaggingapi",
			"GetResources",
			"000000000000",
			"us-east-1",
			map[string]any{},
		),
		http.StatusBadRequest,
	)
}

func TestConsoleGenericDeleteAuthorization(t *testing.T) {
	ctx := context.Background()
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	put(t, ssmClient(s, "test", "us-east-1"), "/console/denied", "v")
	key := authorizationUser(t, s)
	authorizationPolicy(
		t,
		s,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"ssm:DeleteParameter","Resource":"*"}]}`,
	)
	r := consoleLambdaRequest(
		t,
		s,
		"ssm",
		"DeleteParameter",
		"000000000000",
		"us-east-1",
		map[string]any{"Name": "/console/denied"},
	)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}

	r.Body = io.NopCloser(strings.NewReader(string(body)))
	sum := sha256.Sum256(body)
	credentials := aws.Credentials{
		AccessKeyID: aws.ToString(key.AccessKeyId), SecretAccessKey: aws.ToString(key.SecretAccessKey),
	}

	err = v4.NewSigner().SignHTTP(ctx, credentials, r, hex.EncodeToString(sum[:]), "ssm", "us-east-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	consoleResponse(t, s, r, http.StatusForbidden)
	_, err = ssmClient(
		s,
		"test",
		"us-east-1",
	).GetParameter(context.Background(), &ssm.GetParameterInput{Name: new("/console/denied")})
	if err != nil {
		t.Fatal(err)
	}
}

type genericInventoryResource struct {
	Tags    map[string]string `json:"tags"`
	Service string            `json:"service"`
	Type    string            `json:"type"`
	Name    string            `json:"name"`
	ARN     string            `json:"arn"`
	Account string            `json:"account"`
	Region  string            `json:"region"`
}

func genericInventory(t *testing.T, s *httptest.Server) []genericInventoryResource {
	t.Helper()
	var out struct {
		Resources []genericInventoryResource `json:"resources"`
	}

	if internal(t, s, http.MethodGet, "/_tinystack/resources", &out) != http.StatusOK {
		t.Fatal("inventory")
	}

	return out.Resources
}

func TestConsoleGenericDeleteConstraints(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	ecr := ecrClient(s, "test", "us-east-1")
	createRepository(ctx, t, ecr, "occupied")
	pushImage(ctx, t, ecr, "occupied", "latest", "console-content")
	out := consoleResponse(t, s, consoleLambdaRequest(t, s, "ecr", "DeleteRepository",
		"000000000000", "us-east-1", map[string]any{"RepositoryName": "occupied"}), http.StatusBadRequest)
	if out["code"] != "RepositoryNotEmptyException" ||
		!strings.Contains(out["message"].(string), "still contains images") {
		t.Fatal(out)
	}

	zones := route53Client(s)
	zone, err := zones.CreateHostedZone(ctx, &route53.CreateHostedZoneInput{
		Name: new("occupied.example."), CallerReference: new("occupied"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err = changeRecords(zones, zone.HostedZone.Id,
		change(r53types.ChangeActionCreate, "www.occupied.example.", r53types.RRTypeA, "192.0.2.1")); err != nil {
		t.Fatal(err)
	}

	out = consoleResponse(t, s, consoleLambdaRequest(t, s, "route53", "DeleteHostedZone",
		"000000000000", "us-east-1", map[string]any{"Id": aws.ToString(zone.HostedZone.Id)}), http.StatusBadRequest)
	if out["code"] != "HostedZoneNotEmpty" {
		t.Fatal(out)
	}
}
