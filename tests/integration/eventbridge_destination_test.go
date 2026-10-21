package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func apiKeyAuth(name, value string) *ebtypes.CreateConnectionAuthRequestParameters {
	return &ebtypes.CreateConnectionAuthRequestParameters{
		ApiKeyAuthParameters: &ebtypes.CreateConnectionApiKeyAuthRequestParameters{
			ApiKeyName: aws.String(name), ApiKeyValue: aws.String(value),
		},
	}
}

func createConnection(
	t *testing.T,
	c *eventbridge.Client,
	name string,
	kind ebtypes.ConnectionAuthorizationType,
	auth *ebtypes.CreateConnectionAuthRequestParameters,
) string {
	t.Helper()
	out, err := c.CreateConnection(context.Background(), &eventbridge.CreateConnectionInput{
		Name: aws.String(name), AuthorizationType: kind, AuthParameters: auth,
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.ConnectionArn)
}

func TestEventBridgeConnections(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()

	created, err := c.CreateConnection(ctx, &eventbridge.CreateConnectionInput{
		Name:              aws.String("keyed"),
		Description:       aws.String("api key"),
		AuthorizationType: ebtypes.ConnectionAuthorizationTypeApiKey,
		AuthParameters: &ebtypes.CreateConnectionAuthRequestParameters{
			ApiKeyAuthParameters: &ebtypes.CreateConnectionApiKeyAuthRequestParameters{
				ApiKeyName: aws.String("X-Api-Key"), ApiKeyValue: aws.String("hunter2"),
			},
			InvocationHttpParameters: &ebtypes.ConnectionHttpParameters{
				HeaderParameters: []ebtypes.ConnectionHeaderParameter{
					{Key: aws.String("X-Plain"), Value: aws.String("visible")},
					{Key: aws.String("X-Secret"), Value: aws.String("hidden"), IsValueSecret: true},
				},
				QueryStringParameters: []ebtypes.ConnectionQueryStringParameter{
					{Key: aws.String("token"), Value: aws.String("q-secret"), IsValueSecret: true},
				},
				BodyParameters: []ebtypes.ConnectionBodyParameter{
					{Key: aws.String("tenant"), Value: aws.String("acme")},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := aws.ToString(created.ConnectionArn)
	if !strings.HasPrefix(arn, "arn:aws:events:us-east-1:000000000000:connection/keyed/") ||
		created.ConnectionState != ebtypes.ConnectionStateAuthorized || created.CreationTime == nil {
		t.Fatalf("created %+v", created)
	}

	described, err := c.DescribeConnection(ctx, &eventbridge.DescribeConnectionInput{Name: aws.String("keyed")})
	if err != nil {
		t.Fatal(err)
	}

	secretPrefix := "arn:aws:secretsmanager:us-east-1:000000000000:secret:events!connection/keyed/"
	if aws.ToString(described.ConnectionArn) != arn || aws.ToString(described.Description) != "api key" ||
		described.AuthorizationType != ebtypes.ConnectionAuthorizationTypeApiKey ||
		described.ConnectionState != ebtypes.ConnectionStateAuthorized || described.LastAuthorizedTime == nil ||
		!strings.HasPrefix(aws.ToString(described.SecretArn), secretPrefix) ||
		aws.ToString(described.AuthParameters.ApiKeyAuthParameters.ApiKeyName) != "X-Api-Key" {
		t.Fatalf("described %+v", described)
	}

	// Secrets are never described, not even those of the invocation parameters.
	invocation := described.AuthParameters.InvocationHttpParameters
	if len(invocation.HeaderParameters) != 2 || aws.ToString(invocation.HeaderParameters[0].Value) != "visible" ||
		invocation.HeaderParameters[1].Value != nil || !invocation.HeaderParameters[1].IsValueSecret ||
		invocation.QueryStringParameters[0].Value != nil ||
		aws.ToString(invocation.BodyParameters[0].Value) != "acme" {
		t.Fatalf("invocation parameters %+v", invocation)
	}

	if strings.Contains(fmt.Sprintf("%+v", described), "hunter2") {
		t.Fatal("the API key value was described")
	}

	// An update that gives only some members keeps the others, and a parameter without a value keeps its secret.
	updated, err := c.UpdateConnection(ctx, &eventbridge.UpdateConnectionInput{
		Name:        aws.String("keyed"),
		Description: aws.String("changed"),
		AuthParameters: &ebtypes.UpdateConnectionAuthRequestParameters{
			ApiKeyAuthParameters: &ebtypes.UpdateConnectionApiKeyAuthRequestParameters{
				ApiKeyValue: aws.String("rotated"),
			},
			InvocationHttpParameters: &ebtypes.ConnectionHttpParameters{
				HeaderParameters: []ebtypes.ConnectionHeaderParameter{
					{Key: aws.String("X-Secret"), IsValueSecret: true},
				},
			},
		},
	})
	if err != nil || aws.ToString(updated.ConnectionArn) != arn {
		t.Fatalf("updated %+v, %v", updated, err)
	}

	described, err = c.DescribeConnection(ctx, &eventbridge.DescribeConnectionInput{Name: aws.String("keyed")})
	if err != nil || aws.ToString(described.Description) != "changed" ||
		aws.ToString(described.AuthParameters.ApiKeyAuthParameters.ApiKeyName) != "X-Api-Key" ||
		len(described.AuthParameters.InvocationHttpParameters.HeaderParameters) != 1 ||
		described.AuthParameters.InvocationHttpParameters.QueryStringParameters != nil {
		t.Fatalf("described %+v, %v", described, err)
	}

	// An unknown secret parameter has no value to keep.
	_, err = c.UpdateConnection(ctx, &eventbridge.UpdateConnectionInput{
		Name: aws.String("keyed"),
		AuthParameters: &ebtypes.UpdateConnectionAuthRequestParameters{
			InvocationHttpParameters: &ebtypes.ConnectionHttpParameters{
				HeaderParameters: []ebtypes.ConnectionHeaderParameter{{Key: aws.String("X-New"), IsValueSecret: true}},
			},
		},
	})
	code(t, err, "ValidationException")

	// Deauthorizing removes the credentials, so authorizing again must give them.
	deauthorized, err := c.DeauthorizeConnection(
		ctx,
		&eventbridge.DeauthorizeConnectionInput{Name: aws.String("keyed")},
	)
	if err != nil || deauthorized.ConnectionState != ebtypes.ConnectionStateDeauthorized ||
		aws.ToString(deauthorized.ConnectionArn) != arn {
		t.Fatalf("deauthorized %+v, %v", deauthorized, err)
	}

	_, err = c.UpdateConnection(ctx, &eventbridge.UpdateConnectionInput{
		Name: aws.String("keyed"), AuthParameters: &ebtypes.UpdateConnectionAuthRequestParameters{
			ApiKeyAuthParameters: &ebtypes.UpdateConnectionApiKeyAuthRequestParameters{ApiKeyName: aws.String("X-Key")},
		},
	})
	code(t, err, "ValidationException")
	reauthorized, err := c.UpdateConnection(ctx, &eventbridge.UpdateConnectionInput{
		Name: aws.String("keyed"), AuthParameters: &ebtypes.UpdateConnectionAuthRequestParameters{
			ApiKeyAuthParameters: &ebtypes.UpdateConnectionApiKeyAuthRequestParameters{
				ApiKeyValue: aws.String("again"),
			},
		},
	})
	if err != nil || reauthorized.ConnectionState != ebtypes.ConnectionStateAuthorized {
		t.Fatalf("reauthorized %+v, %v", reauthorized, err)
	}

	// The type can change with a complete set of parameters.
	_, err = c.UpdateConnection(ctx, &eventbridge.UpdateConnectionInput{
		Name: aws.String("keyed"), AuthorizationType: ebtypes.ConnectionAuthorizationTypeBasic,
		AuthParameters: &ebtypes.UpdateConnectionAuthRequestParameters{
			BasicAuthParameters: &ebtypes.UpdateConnectionBasicAuthRequestParameters{
				Username: aws.String("u"), Password: aws.String("p"),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err = c.DescribeConnection(ctx, &eventbridge.DescribeConnectionInput{Name: aws.String("keyed")})
	if err != nil || described.AuthorizationType != ebtypes.ConnectionAuthorizationTypeBasic ||
		aws.ToString(described.AuthParameters.BasicAuthParameters.Username) != "u" ||
		described.AuthParameters.ApiKeyAuthParameters != nil {
		t.Fatalf("described %+v, %v", described, err)
	}

	_, err = c.UpdateConnection(ctx, &eventbridge.UpdateConnectionInput{
		Name: aws.String("keyed"), AuthorizationType: ebtypes.ConnectionAuthorizationTypeOauthClientCredentials,
	})
	code(t, err, "ValidationException")
	_, err = c.UpdateConnection(ctx, &eventbridge.UpdateConnectionInput{Name: aws.String("missing")})
	code(t, err, "ResourceNotFoundException")

	oauth := ebtypes.ConnectionAuthorizationTypeOauthClientCredentials
	oauthAuth := &ebtypes.CreateConnectionAuthRequestParameters{
		OAuthParameters: &ebtypes.CreateConnectionOAuthRequestParameters{
			AuthorizationEndpoint: aws.String("https://auth.example.com/token"),
			HttpMethod:            ebtypes.ConnectionOAuthHttpMethodPost,
			ClientParameters: &ebtypes.CreateConnectionOAuthClientRequestParameters{
				ClientID: aws.String("client"), ClientSecret: aws.String("shh"),
			},
			OAuthHttpParameters: &ebtypes.ConnectionHttpParameters{
				BodyParameters: []ebtypes.ConnectionBodyParameter{
					{Key: aws.String("audience"), Value: aws.String("api")},
				},
			},
		},
	}
	createConnection(t, c, "oauth", oauth, oauthAuth)
	described, err = c.DescribeConnection(ctx, &eventbridge.DescribeConnectionInput{Name: aws.String("oauth")})
	if err != nil {
		t.Fatal(err)
	}

	o := described.AuthParameters.OAuthParameters
	if aws.ToString(o.AuthorizationEndpoint) != "https://auth.example.com/token" ||
		o.HttpMethod != ebtypes.ConnectionOAuthHttpMethodPost || aws.ToString(o.ClientParameters.ClientID) != "client" ||
		aws.ToString(o.OAuthHttpParameters.BodyParameters[0].Value) != "api" {
		t.Fatalf("oauth parameters %+v", o)
	}

	// Connections list by name prefix and state, a page at a time.
	createConnection(
		t,
		c,
		"basic",
		ebtypes.ConnectionAuthorizationTypeBasic,
		&ebtypes.CreateConnectionAuthRequestParameters{
			BasicAuthParameters: &ebtypes.CreateConnectionBasicAuthRequestParameters{
				Username: aws.String("u"), Password: aws.String("p"),
			},
		},
	)
	if _, err = c.DeauthorizeConnection(
		ctx,
		&eventbridge.DeauthorizeConnectionInput{Name: aws.String("basic")},
	); err != nil {
		t.Fatal(err)
	}

	var names []string
	var token *string
	for {
		page, listErr := c.ListConnections(
			ctx,
			&eventbridge.ListConnectionsInput{Limit: aws.Int32(1), NextToken: token},
		)
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, listed := range page.Connections {
			names = append(names, aws.ToString(listed.Name))
		}

		if token = page.NextToken; token == nil {
			break
		}
	}

	if strings.Join(names, ",") != "basic,keyed,oauth" {
		t.Fatal(names)
	}

	filtered, err := c.ListConnections(ctx, &eventbridge.ListConnectionsInput{
		ConnectionState: ebtypes.ConnectionStateDeauthorized,
	})
	if err != nil || len(filtered.Connections) != 1 || aws.ToString(filtered.Connections[0].Name) != "basic" {
		t.Fatalf("deauthorized %+v, %v", filtered, err)
	}

	prefixed, err := c.ListConnections(ctx, &eventbridge.ListConnectionsInput{NamePrefix: aws.String("o")})
	if err != nil || len(prefixed.Connections) != 1 || aws.ToString(prefixed.Connections[0].Name) != "oauth" {
		t.Fatalf("prefixed %+v, %v", prefixed, err)
	}

	deleted, err := c.DeleteConnection(ctx, &eventbridge.DeleteConnectionInput{Name: aws.String("oauth")})
	if err != nil || aws.ToString(deleted.ConnectionArn) == "" {
		t.Fatalf("deleted %+v, %v", deleted, err)
	}

	_, err = c.DescribeConnection(ctx, &eventbridge.DescribeConnectionInput{Name: aws.String("oauth")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.DeleteConnection(ctx, &eventbridge.DeleteConnectionInput{Name: aws.String("oauth")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.DeauthorizeConnection(ctx, &eventbridge.DeauthorizeConnectionInput{Name: aws.String("oauth")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.ListConnections(ctx, &eventbridge.ListConnectionsInput{Limit: aws.Int32(101)})
	code(t, err, "ValidationException")
}

func TestEventBridgeConnectionValidation(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()
	createConnection(t, c, "taken", ebtypes.ConnectionAuthorizationTypeApiKey, apiKeyAuth("X-Key", "v"))

	basic := &ebtypes.CreateConnectionAuthRequestParameters{
		BasicAuthParameters: &ebtypes.CreateConnectionBasicAuthRequestParameters{
			Username: aws.String("u"), Password: aws.String("p"),
		},
	}
	oauth := func(
		endpoint string,
		method ebtypes.ConnectionOAuthHttpMethod,
		id, secret *string,
	) *ebtypes.CreateConnectionAuthRequestParameters {
		return &ebtypes.CreateConnectionAuthRequestParameters{
			OAuthParameters: &ebtypes.CreateConnectionOAuthRequestParameters{
				AuthorizationEndpoint: aws.String(endpoint),
				HttpMethod:            method,
				ClientParameters: &ebtypes.CreateConnectionOAuthClientRequestParameters{
					ClientID:     id,
					ClientSecret: secret,
				},
			},
		}
	}

	apiKey, basicType := ebtypes.ConnectionAuthorizationTypeApiKey, ebtypes.ConnectionAuthorizationTypeBasic
	oauthType := ebtypes.ConnectionAuthorizationTypeOauthClientCredentials
	for name, input := range map[string]*eventbridge.CreateConnectionInput{
		"bad name":     {Name: aws.String("a b"), AuthorizationType: apiKey, AuthParameters: apiKeyAuth("X-Key", "v")},
		"bad type":     {Name: aws.String("n"), AuthorizationType: "TOKEN", AuthParameters: apiKeyAuth("X-Key", "v")},
		"wrong block":  {Name: aws.String("n"), AuthorizationType: basicType, AuthParameters: apiKeyAuth("X-Key", "v")},
		"no block":     {Name: aws.String("n"), AuthorizationType: oauthType, AuthParameters: basic},
		"no key value": {Name: aws.String("n"), AuthorizationType: apiKey, AuthParameters: apiKeyAuth("X-Key", "")},
		"bad header":   {Name: aws.String("n"), AuthorizationType: apiKey, AuthParameters: apiKeyAuth("X Key", "v")},
		"long text": {
			Name: aws.String("n"), AuthorizationType: apiKey, AuthParameters: apiKeyAuth("X-Key", strings.Repeat("v", 513)),
		},
		"long description": {
			Name: aws.String("n"), AuthorizationType: apiKey, AuthParameters: apiKeyAuth("X-Key", "v"),
			Description: aws.String(strings.Repeat("d", 513)),
		},
		"oauth endpoint": {
			Name: aws.String("n"), AuthorizationType: oauthType,
			AuthParameters: oauth("not a url", ebtypes.ConnectionOAuthHttpMethodPost, aws.String("i"), aws.String("s")),
		},
		"oauth method": {
			Name: aws.String("n"), AuthorizationType: oauthType,
			AuthParameters: oauth("https://a.example/t", "DELETE", aws.String("i"), aws.String("s")),
		},
		"oauth secret": {
			Name: aws.String("n"), AuthorizationType: oauthType,
			AuthParameters: oauth("https://a.example/t", ebtypes.ConnectionOAuthHttpMethodGet, aws.String("i"), aws.String("")),
		},
		"duplicate parameter": {
			Name: aws.String("n"), AuthorizationType: basicType, AuthParameters: &ebtypes.CreateConnectionAuthRequestParameters{
				BasicAuthParameters: basic.BasicAuthParameters,
				InvocationHttpParameters: &ebtypes.ConnectionHttpParameters{
					QueryStringParameters: []ebtypes.ConnectionQueryStringParameter{
						{Key: aws.String("a"), Value: aws.String("1")}, {Key: aws.String("a"), Value: aws.String("2")},
					},
				},
			},
		},
		"parameter without value": {
			Name: aws.String("n"), AuthorizationType: basicType, AuthParameters: &ebtypes.CreateConnectionAuthRequestParameters{
				BasicAuthParameters: basic.BasicAuthParameters,
				InvocationHttpParameters: &ebtypes.ConnectionHttpParameters{
					BodyParameters: []ebtypes.ConnectionBodyParameter{{Key: aws.String("a")}},
				},
			},
		},
	} {
		_, err := c.CreateConnection(ctx, input)
		if err == nil {
			t.Fatal(name, "accepted")
		}

		code(t, err, "ValidationException")
	}

	_, err := c.CreateConnection(ctx, &eventbridge.CreateConnectionInput{
		Name: aws.String("taken"), AuthorizationType: apiKey, AuthParameters: apiKeyAuth("X-Key", "v"),
	})
	code(t, err, "ResourceAlreadyExistsException")
	_, err = c.DescribeConnection(ctx, &eventbridge.DescribeConnectionInput{Name: aws.String("missing")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.DescribeConnection(ctx, &eventbridge.DescribeConnectionInput{Name: aws.String("a b")})
	code(t, err, "ValidationException")
}

func createDestination(
	t *testing.T,
	c *eventbridge.Client,
	name, connection, endpoint string,
	method ebtypes.ApiDestinationHttpMethod,
) string {
	t.Helper()
	out, err := c.CreateApiDestination(context.Background(), &eventbridge.CreateApiDestinationInput{
		Name: aws.String(name), ConnectionArn: aws.String(connection), InvocationEndpoint: aws.String(endpoint),
		HttpMethod: method,
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.ApiDestinationArn)
}

func TestEventBridgeApiDestinations(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()
	first := createConnection(t, c, "first", ebtypes.ConnectionAuthorizationTypeApiKey, apiKeyAuth("X-Key", "v"))
	second := createConnection(t, c, "second", ebtypes.ConnectionAuthorizationTypeApiKey, apiKeyAuth("X-Key", "v"))

	created, err := c.CreateApiDestination(ctx, &eventbridge.CreateApiDestinationInput{
		Name:                         aws.String("hook"),
		Description:                  aws.String("a hook"),
		ConnectionArn:                aws.String(first),
		InvocationEndpoint:           aws.String("https://api.example.com/v1/*/items"),
		HttpMethod:                   ebtypes.ApiDestinationHttpMethodPut,
		InvocationRateLimitPerSecond: aws.Int32(10),
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := aws.ToString(created.ApiDestinationArn)
	if !strings.HasPrefix(arn, "arn:aws:events:us-east-1:000000000000:api-destination/hook/") ||
		created.ApiDestinationState != ebtypes.ApiDestinationStateActive {
		t.Fatalf("created %+v", created)
	}

	described, err := c.DescribeApiDestination(ctx, &eventbridge.DescribeApiDestinationInput{Name: aws.String("hook")})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(described.ApiDestinationArn) != arn || aws.ToString(described.Name) != "hook" ||
		aws.ToString(described.Description) != "a hook" || aws.ToString(described.ConnectionArn) != first ||
		aws.ToString(described.InvocationEndpoint) != "https://api.example.com/v1/*/items" ||
		described.HttpMethod != ebtypes.ApiDestinationHttpMethodPut ||
		aws.ToInt32(described.InvocationRateLimitPerSecond) != 10 ||
		described.ApiDestinationState != ebtypes.ApiDestinationStateActive || described.CreationTime == nil {
		t.Fatalf("described %+v", described)
	}

	// The rate limit is 300 a second unless the destination sets one.
	createDestination(t, c, "plain", second, "http://localhost:8080/events", ebtypes.ApiDestinationHttpMethodPost)
	plain, err := c.DescribeApiDestination(ctx, &eventbridge.DescribeApiDestinationInput{Name: aws.String("plain")})
	if err != nil || aws.ToInt32(plain.InvocationRateLimitPerSecond) != 300 {
		t.Fatalf("plain %+v, %v", plain, err)
	}

	// An update changes only the members it gives.
	updated, err := c.UpdateApiDestination(ctx, &eventbridge.UpdateApiDestinationInput{
		Name: aws.String("hook"), ConnectionArn: aws.String(second), HttpMethod: ebtypes.ApiDestinationHttpMethodPatch,
	})
	if err != nil || aws.ToString(updated.ApiDestinationArn) != arn {
		t.Fatalf("updated %+v, %v", updated, err)
	}

	described, err = c.DescribeApiDestination(ctx, &eventbridge.DescribeApiDestinationInput{Name: aws.String("hook")})
	if err != nil || aws.ToString(described.ConnectionArn) != second ||
		described.HttpMethod != ebtypes.ApiDestinationHttpMethodPatch ||
		aws.ToString(described.InvocationEndpoint) != "https://api.example.com/v1/*/items" ||
		aws.ToInt32(described.InvocationRateLimitPerSecond) != 10 || aws.ToString(described.Description) != "a hook" {
		t.Fatalf("described %+v, %v", described, err)
	}

	var names []string
	var token *string
	for {
		page, listErr := c.ListApiDestinations(ctx, &eventbridge.ListApiDestinationsInput{
			Limit: aws.Int32(1), NextToken: token,
		})
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, listed := range page.ApiDestinations {
			names = append(names, aws.ToString(listed.Name))
		}

		if token = page.NextToken; token == nil {
			break
		}
	}

	if strings.Join(names, ",") != "hook,plain" {
		t.Fatal(names)
	}

	byConnection, err := c.ListApiDestinations(
		ctx,
		&eventbridge.ListApiDestinationsInput{ConnectionArn: aws.String(second)},
	)
	if err != nil || len(byConnection.ApiDestinations) != 2 {
		t.Fatalf("by connection %+v, %v", byConnection, err)
	}

	prefixed, err := c.ListApiDestinations(ctx, &eventbridge.ListApiDestinationsInput{NamePrefix: aws.String("pl")})
	if err != nil || len(prefixed.ApiDestinations) != 1 || aws.ToString(prefixed.ApiDestinations[0].Name) != "plain" {
		t.Fatalf("prefixed %+v, %v", prefixed, err)
	}

	for name, input := range map[string]*eventbridge.CreateApiDestinationInput{
		"bad name": {
			Name: aws.String("a b"), ConnectionArn: aws.String(first), InvocationEndpoint: aws.String("https://a.example"),
			HttpMethod: ebtypes.ApiDestinationHttpMethodPost,
		},
		"bad method": {
			Name: aws.String("n"), ConnectionArn: aws.String(first), InvocationEndpoint: aws.String("https://a.example"),
			HttpMethod: "TRACE",
		},
		"bad endpoint": {
			Name: aws.String("n"), ConnectionArn: aws.String(first), InvocationEndpoint: aws.String("ftp://a.example"),
			HttpMethod: ebtypes.ApiDestinationHttpMethodPost,
		},
		"rate": {
			Name: aws.String("n"), ConnectionArn: aws.String(first), InvocationEndpoint: aws.String("https://a.example"),
			HttpMethod: ebtypes.ApiDestinationHttpMethodPost, InvocationRateLimitPerSecond: aws.Int32(0),
		},
		"long description": {
			Name: aws.String("n"), ConnectionArn: aws.String(first), InvocationEndpoint: aws.String("https://a.example"),
			HttpMethod: ebtypes.ApiDestinationHttpMethodPost, Description: aws.String(strings.Repeat("d", 513)),
		},
	} {
		_, err = c.CreateApiDestination(ctx, input)
		if err == nil {
			t.Fatal(name, "accepted")
		}

		code(t, err, "ValidationException")
	}

	// The connection must exist, with the ARN's ID.
	for _, connection := range []string{
		"arn:aws:events:us-east-1:000000000000:connection/absent/0000",
		strings.TrimSuffix(first, first[strings.LastIndex(first, "/"):]) + "/wrong-id",
		"arn:aws:events:us-east-1:111111111111:connection/first/0000",
		"nonsense",
	} {
		_, err = c.CreateApiDestination(ctx, &eventbridge.CreateApiDestinationInput{
			Name: aws.String(
				"n",
			),
			ConnectionArn:      aws.String(connection),
			InvocationEndpoint: aws.String("https://a.example"),
			HttpMethod:         ebtypes.ApiDestinationHttpMethodPost,
		})
		code(t, err, "ResourceNotFoundException")
	}

	_, err = c.CreateApiDestination(ctx, &eventbridge.CreateApiDestinationInput{
		Name: aws.String("hook"), ConnectionArn: aws.String(first), InvocationEndpoint: aws.String("https://a.example"),
		HttpMethod: ebtypes.ApiDestinationHttpMethodPost,
	})
	code(t, err, "ResourceAlreadyExistsException")
	_, err = c.UpdateApiDestination(ctx, &eventbridge.UpdateApiDestinationInput{
		Name: aws.String(
			"hook",
		),
		ConnectionArn: aws.String("arn:aws:events:us-east-1:000000000000:connection/absent/0000"),
	})
	code(t, err, "ResourceNotFoundException")
	_, err = c.UpdateApiDestination(ctx, &eventbridge.UpdateApiDestinationInput{
		Name: aws.String("hook"), InvocationEndpoint: aws.String("nope"),
	})
	code(t, err, "ValidationException")
	_, err = c.UpdateApiDestination(ctx, &eventbridge.UpdateApiDestinationInput{Name: aws.String("missing")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.DescribeApiDestination(ctx, &eventbridge.DescribeApiDestinationInput{Name: aws.String("missing")})
	code(t, err, "ResourceNotFoundException")

	if _, err = c.DeleteApiDestination(
		ctx,
		&eventbridge.DeleteApiDestinationInput{Name: aws.String("hook")},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApiDestination(ctx, &eventbridge.DeleteApiDestinationInput{Name: aws.String("hook")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.DescribeApiDestination(ctx, &eventbridge.DescribeApiDestinationInput{Name: aws.String("hook")})
	code(t, err, "ResourceNotFoundException")
}

func TestEventBridgeApiDestinationTargetValidation(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()
	putRule(t, c, "orders", orderPattern)
	destination := "arn:aws:events:us-east-1:000000000000:api-destination/hook/id"
	queue := "arn:aws:sqs:us-east-1:000000000000:q"
	for name, target := range map[string]ebtypes.Target{
		"http parameters on a queue": {
			Id: aws.String("t"), Arn: aws.String(queue),
			HttpParameters: &ebtypes.HttpParameters{HeaderParameters: map[string]string{"X-A": "b"}},
		},
		"event bus": {Id: aws.String("t"), Arn: aws.String("arn:aws:events:us-east-1:000000000000:event-bus/other")},
		"other account": {
			Id: aws.String("t"), Arn: aws.String("arn:aws:events:us-east-1:111111111111:api-destination/hook/id"),
		},
		"other region": {
			Id: aws.String("t"), Arn: aws.String("arn:aws:events:eu-west-1:000000000000:api-destination/hook/id"),
		},
		"header name": {
			Id: aws.String("t"), Arn: aws.String(destination),
			HttpParameters: &ebtypes.HttpParameters{HeaderParameters: map[string]string{"X A": "b"}},
		},
		"json path header": {
			Id: aws.String("t"), Arn: aws.String(destination),
			HttpParameters: &ebtypes.HttpParameters{HeaderParameters: map[string]string{"X-A": "$.detail.id"}},
		},
		"json path query": {
			Id: aws.String("t"), Arn: aws.String(destination),
			HttpParameters: &ebtypes.HttpParameters{QueryStringParameters: map[string]string{"a": "$.detail.id"}},
		},
		"json path segment": {
			Id: aws.String("t"), Arn: aws.String(destination),
			HttpParameters: &ebtypes.HttpParameters{PathParameterValues: []string{"$.detail.id"}},
		},
	} {
		_, err := c.PutTargets(
			ctx,
			&eventbridge.PutTargetsInput{Rule: aws.String("orders"), Targets: []ebtypes.Target{target}},
		)
		if err == nil {
			t.Fatal(name, "accepted")
		}

		code(t, err, "ValidationException")
	}

	// A target keeps its HttpParameters.
	_, err := c.PutTargets(ctx, &eventbridge.PutTargetsInput{Rule: aws.String("orders"), Targets: []ebtypes.Target{{
		Id: aws.String("t"), Arn: aws.String(destination),
		HttpParameters: &ebtypes.HttpParameters{
			HeaderParameters:      map[string]string{"X-A": "b"},
			QueryStringParameters: map[string]string{"q": "1"},
			PathParameterValues:   []string{"x"},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: aws.String("orders")})
	if err != nil || len(listed.Targets) != 1 {
		t.Fatalf("listed %+v, %v", listed, err)
	}

	h := listed.Targets[0].HttpParameters
	if h == nil || h.HeaderParameters["X-A"] != "b" || h.QueryStringParameters["q"] != "1" ||
		len(h.PathParameterValues) != 1 || h.PathParameterValues[0] != "x" {
		t.Fatalf("http parameters %+v", h)
	}
}

// remote is a server for API destinations to call: it records what hook endpoints receive, answers token requests,
// and fails hook requests as the test sets.
type remote struct {
	*httptest.Server
	requests []received
	tokens   []received
	mu       sync.Mutex
	// status is the status hooks answer with, and ttl the expires_in of tokens.
	status atomic.Int32
	ttl    atomic.Int32
}

type received struct {
	Header                       http.Header
	Method, Path, RawQuery, Body string
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	r := &remote{}
	r.status.Store(http.StatusOK)
	r.ttl.Store(3600)
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		got := received{
			Method:   req.Method,
			Path:     req.URL.Path,
			RawQuery: req.URL.RawQuery,
			Body:     string(body),
			Header:   req.Header.Clone(),
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if req.URL.Path == "/token" {
			r.tokens = append(r.tokens, got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(
				w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":%d}`, len(r.tokens), r.ttl.Load(),
			)

			return
		}

		r.requests = append(r.requests, got)
		w.WriteHeader(int(r.status.Load()))
	}))
	t.Cleanup(r.Close)
	return r
}

// calls returns the hook requests received so far.
func (r *remote) calls() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.requests...)
}

func (r *remote) tokenCalls() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.tokens...)
}

// only returns the only hook request received, failing the test if there is not exactly one.
func (r *remote) only(t *testing.T) received {
	t.Helper()
	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("got %d requests, wanted 1: %+v", len(calls), calls)
	}

	return calls[0]
}

func (r *remote) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests, r.tokens = nil, nil
}

// route creates a connection, an API destination to endpoint, and a rule on source that delivers to it.
func route(
	t *testing.T,
	c *eventbridge.Client,
	name string,
	auth ebtypes.ConnectionAuthorizationType,
	params *ebtypes.CreateConnectionAuthRequestParameters,
	method ebtypes.ApiDestinationHttpMethod,
	endpoint string,
	target *ebtypes.Target,
) {
	t.Helper()
	connection := createConnection(t, c, name, auth, params)
	target.Id, target.Arn = aws.String("t"), aws.String(createDestination(t, c, name, connection, endpoint, method))
	ctx := context.Background()
	_, err := c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: aws.String(name), EventPattern: aws.String(`{"source":["` + name + `"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.PutTargets(
		ctx,
		&eventbridge.PutTargetsInput{Rule: aws.String(name), Targets: []ebtypes.Target{*target}},
	)
	if err != nil || out.FailedEntryCount != 0 {
		t.Fatalf("got %+v, %v", out, err)
	}
}

func emit(t *testing.T, c *eventbridge.Client, source, detail string) {
	t.Helper()
	out, err := c.PutEvents(context.Background(), &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
		Source: aws.String(source), DetailType: aws.String("order"), Detail: aws.String(detail),
	}}})
	if err != nil || out.FailedEntryCount != 0 {
		t.Fatalf("got %+v, %v", out, err)
	}
}

// eventOf parses the event a request carries.
func eventOf(t *testing.T, got *received) map[string]any {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal([]byte(got.Body), &event); err != nil {
		t.Fatalf("body %q: %v", got.Body, err)
	}

	return event
}

func TestEventBridgeApiDestinationDelivery(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	r := newRemote(t)

	t.Run("api key", func(t *testing.T) {
		route(t, c, "keyed", ebtypes.ConnectionAuthorizationTypeApiKey, &ebtypes.CreateConnectionAuthRequestParameters{
			ApiKeyAuthParameters: &ebtypes.CreateConnectionApiKeyAuthRequestParameters{
				ApiKeyName: aws.String("X-Api-Key"), ApiKeyValue: aws.String("key-1"),
			},
		}, ebtypes.ApiDestinationHttpMethodPost, r.URL+"/hook/keyed?fixed=1", &ebtypes.Target{
			HttpParameters: &ebtypes.HttpParameters{
				HeaderParameters:      map[string]string{"X-Rule": "keyed", "X-Api-Key": "overridden"},
				QueryStringParameters: map[string]string{"q": "a b"},
			},
		})
		emit(t, c, "keyed", `{"id":1}`)
		got := r.only(t)
		query, _ := url.ParseQuery(got.RawQuery)
		event := eventOf(t, &got)
		detail, _ := event["detail"].(map[string]any)
		if got.Method != http.MethodPost || got.Path != "/hook/keyed" || query.Get("fixed") != "1" ||
			query.Get("q") != "a b" || got.Header.Get("X-Api-Key") != "key-1" || got.Header.Get("X-Rule") != "keyed" ||
			got.Header.Get("Content-Type") != "application/json" || event["source"] != "keyed" ||
			event["detail-type"] != "order" || detail["id"] != float64(1) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("basic with a method and input", func(t *testing.T) {
		r.reset()
		route(t, c, "basic", ebtypes.ConnectionAuthorizationTypeBasic, &ebtypes.CreateConnectionAuthRequestParameters{
			BasicAuthParameters: &ebtypes.CreateConnectionBasicAuthRequestParameters{
				Username: aws.String("alice"), Password: aws.String("p:ss"),
			},
		}, ebtypes.ApiDestinationHttpMethodPut, r.URL+"/hook/basic", &ebtypes.Target{Input: aws.String(`{"fixed":true}`)})
		emit(t, c, "basic", `{"id":2}`)
		got := r.only(t)
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:p:ss"))
		if got.Method != http.MethodPut || got.Path != "/hook/basic" || got.Body != `{"fixed":true}` ||
			got.Header.Get("Authorization") != wantAuth {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("path wildcards and input path", func(t *testing.T) {
		r.reset()
		route(t, c, "paths", ebtypes.ConnectionAuthorizationTypeApiKey, apiKeyAuth("X-Key", "k"),
			ebtypes.ApiDestinationHttpMethodPatch, r.URL+"/hook/*/items/*", &ebtypes.Target{
				InputPath:      aws.String("$.detail"),
				HttpParameters: &ebtypes.HttpParameters{PathParameterValues: []string{"acme", "42"}},
			})
		emit(t, c, "paths", `{"id":3}`)
		got := r.only(t)
		if got.Method != http.MethodPatch || got.Path != "/hook/acme/items/42" || got.Body != `{"id":3}` {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("get", func(t *testing.T) {
		r.reset()
		route(t, c, "get", ebtypes.ConnectionAuthorizationTypeApiKey, apiKeyAuth("X-Key", "k"),
			ebtypes.ApiDestinationHttpMethodGet, r.URL+"/hook/get", &ebtypes.Target{})
		emit(t, c, "get", `{"id":4}`)
		if got := r.only(t); got.Method != http.MethodGet || got.Path != "/hook/get" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("invocation parameters", func(t *testing.T) {
		r.reset()
		route(t, c, "params", ebtypes.ConnectionAuthorizationTypeApiKey, &ebtypes.CreateConnectionAuthRequestParameters{
			ApiKeyAuthParameters: &ebtypes.CreateConnectionApiKeyAuthRequestParameters{
				ApiKeyName: aws.String("X-Key"), ApiKeyValue: aws.String("k"),
			},
			InvocationHttpParameters: &ebtypes.ConnectionHttpParameters{
				HeaderParameters: []ebtypes.ConnectionHeaderParameter{
					{Key: aws.String("X-Conn"), Value: aws.String("from-connection")},
					{Key: aws.String("X-Secret"), Value: aws.String("s3cret"), IsValueSecret: true},
				},
				QueryStringParameters: []ebtypes.ConnectionQueryStringParameter{
					{Key: aws.String("q"), Value: aws.String("connection")},
				},
				BodyParameters: []ebtypes.ConnectionBodyParameter{
					{Key: aws.String("tenant"), Value: aws.String("acme")},
				},
			},
		}, ebtypes.ApiDestinationHttpMethodPost, r.URL+"/hook/params", &ebtypes.Target{
			HttpParameters: &ebtypes.HttpParameters{
				HeaderParameters:      map[string]string{"X-Conn": "from-target"},
				QueryStringParameters: map[string]string{"q": "target"},
			},
		})
		emit(t, c, "params", `{"id":5}`)
		got := r.only(t)
		query, _ := url.ParseQuery(got.RawQuery)
		event := eventOf(t, &got)
		// The connection's parameters win over the target's, and its body parameters join the event.
		if got.Header.Get("X-Conn") != "from-connection" || got.Header.Get("X-Secret") != "s3cret" ||
			query.Get("q") != "connection" || event["tenant"] != "acme" || event["source"] != "params" {
			t.Fatalf("got %+v", got)
		}

		// An update that leaves the secret out keeps it.
		_, err := c.UpdateConnection(context.Background(), &eventbridge.UpdateConnectionInput{
			Name: aws.String("params"),
			AuthParameters: &ebtypes.UpdateConnectionAuthRequestParameters{
				InvocationHttpParameters: &ebtypes.ConnectionHttpParameters{
					HeaderParameters: []ebtypes.ConnectionHeaderParameter{
						{Key: aws.String("X-Secret"), IsValueSecret: true},
					},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		r.reset()
		emit(t, c, "params", `{"id":6}`)
		got = r.only(t)
		event = eventOf(t, &got)
		if got.Header.Get("X-Secret") != "s3cret" || got.Header.Get("X-Conn") != "from-target" ||
			event["tenant"] != nil {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("oauth", func(t *testing.T) {
		r.reset()
		route(t, c, "oauth", ebtypes.ConnectionAuthorizationTypeOauthClientCredentials,
			&ebtypes.CreateConnectionAuthRequestParameters{
				OAuthParameters: &ebtypes.CreateConnectionOAuthRequestParameters{
					AuthorizationEndpoint: aws.String(r.URL + "/token"),
					HttpMethod:            ebtypes.ConnectionOAuthHttpMethodPost,
					ClientParameters: &ebtypes.CreateConnectionOAuthClientRequestParameters{
						ClientID: aws.String("client-1"), ClientSecret: aws.String("secret-1"),
					},
					OAuthHttpParameters: &ebtypes.ConnectionHttpParameters{
						HeaderParameters: []ebtypes.ConnectionHeaderParameter{
							{Key: aws.String("X-Tenant"), Value: aws.String("acme")},
						},
						QueryStringParameters: []ebtypes.ConnectionQueryStringParameter{
							{Key: aws.String("scope"), Value: aws.String("orders")},
						},
						BodyParameters: []ebtypes.ConnectionBodyParameter{
							{Key: aws.String("audience"), Value: aws.String("api"), IsValueSecret: true},
						},
					},
				},
			}, ebtypes.ApiDestinationHttpMethodPost, r.URL+"/hook/oauth", &ebtypes.Target{})

		// The token is fetched once and kept for the next deliveries.
		emit(t, c, "oauth", `{"id":7}`)
		emit(t, c, "oauth", `{"id":8}`)
		calls, tokens := r.calls(), r.tokenCalls()
		if len(calls) != 2 || len(tokens) != 1 {
			t.Fatalf("got %d requests and %d token requests", len(calls), len(tokens))
		}

		token := tokens[0]
		form, _ := url.ParseQuery(token.Body)
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("client-1:secret-1"))
		if token.Method != http.MethodPost || token.Header.Get("Authorization") != wantAuth ||
			token.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
			token.Header.Get("X-Tenant") != "acme" || token.RawQuery != "scope=orders" ||
			form.Get("grant_type") != "client_credentials" || form.Get("audience") != "api" {
			t.Fatalf("token request %+v", token)
		}

		for _, got := range calls {
			if got.Header.Get("Authorization") != "Bearer token-1" || got.Path != "/hook/oauth" {
				t.Fatalf("got %+v", got)
			}
		}

		// New credentials need a new token.
		_, err := c.UpdateConnection(context.Background(), &eventbridge.UpdateConnectionInput{
			Name: aws.String("oauth"),
			AuthParameters: &ebtypes.UpdateConnectionAuthRequestParameters{
				OAuthParameters: &ebtypes.UpdateConnectionOAuthRequestParameters{
					ClientParameters: &ebtypes.UpdateConnectionOAuthClientRequestParameters{
						ClientSecret: aws.String("secret-2"),
					},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		emit(t, c, "oauth", `{"id":9}`)
		calls, tokens = r.calls(), r.tokenCalls()
		if len(calls) != 3 || len(tokens) != 2 || calls[2].Header.Get("Authorization") != "Bearer token-2" ||
			tokens[1].Header.Get("Authorization") !=
				"Basic "+base64.StdEncoding.EncodeToString([]byte("client-1:secret-2")) {
			t.Fatalf("got %+v and %+v", calls, tokens)
		}

		// A token that expires is fetched again.
		r.ttl.Store(1)
		_, err = c.UpdateConnection(context.Background(), &eventbridge.UpdateConnectionInput{
			Name: aws.String("oauth"), Description: aws.String("short tokens"),
		})
		if err != nil {
			t.Fatal(err)
		}

		emit(t, c, "oauth", `{"id":10}`)
		emit(t, c, "oauth", `{"id":11}`)
		time.Sleep(1100 * time.Millisecond)
		emit(t, c, "oauth", `{"id":12}`)
		calls, tokens = r.calls(), r.tokenCalls()
		if len(calls) != 6 || len(tokens) != 4 || calls[3].Header.Get("Authorization") != "Bearer token-3" ||
			calls[4].Header.Get("Authorization") != "Bearer token-3" ||
			calls[5].Header.Get("Authorization") != "Bearer token-4" {
			t.Fatalf("got %d requests and %+v", len(calls), tokens)
		}
	})

	t.Run("oauth with get", func(t *testing.T) {
		r.reset()
		route(t, c, "oauthget", ebtypes.ConnectionAuthorizationTypeOauthClientCredentials,
			&ebtypes.CreateConnectionAuthRequestParameters{
				OAuthParameters: &ebtypes.CreateConnectionOAuthRequestParameters{
					AuthorizationEndpoint: aws.String(r.URL + "/token"),
					HttpMethod:            ebtypes.ConnectionOAuthHttpMethodGet,
					ClientParameters: &ebtypes.CreateConnectionOAuthClientRequestParameters{
						ClientID: aws.String("c"), ClientSecret: aws.String("s"),
					},
				},
			}, ebtypes.ApiDestinationHttpMethodPost, r.URL+"/hook/oauthget", &ebtypes.Target{})
		emit(t, c, "oauthget", `{"id":13}`)
		tokens := r.tokenCalls()
		if len(tokens) != 1 || tokens[0].Method != http.MethodGet ||
			tokens[0].RawQuery != "grant_type=client_credentials" ||
			tokens[0].Body != "" {
			t.Fatalf("token request %+v", tokens)
		}
	})
}

// deliveryFailure creates the route name to endpoint with a dead-letter queue and returns the queue's URL.
func deliveryFailure(
	t *testing.T,
	s *httptest.Server,
	name, endpoint string,
	policy *ebtypes.RetryPolicy,
) (c *eventbridge.Client, dlqURL string) {
	t.Helper()
	c = eventsClient(s, "test")
	q := sqsClient(s, "test")
	dlqURL = createQueue(t, q, name+"-dlq", nil)
	connection := createConnection(t, c, name, ebtypes.ConnectionAuthorizationTypeApiKey, apiKeyAuth("X-Key", "k"))
	destination := createDestination(t, c, name, connection, endpoint, ebtypes.ApiDestinationHttpMethodPost)
	putRuleSource(t, c, name)
	_, err := c.PutTargets(context.Background(), &eventbridge.PutTargetsInput{
		Rule: aws.String(name),
		Targets: []ebtypes.Target{
			{
				Id:          aws.String("t"),
				Arn:         aws.String(destination),
				RetryPolicy: policy,
				DeadLetterConfig: &ebtypes.DeadLetterConfig{
					Arn: aws.String("arn:aws:sqs:us-east-1:000000000000:" + name + "-dlq"),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return c, dlqURL
}

func putRuleSource(t *testing.T, c *eventbridge.Client, name string) {
	t.Helper()
	putRule(t, c, name, `{"source":["`+name+`"]}`)
}

func TestEventBridgeApiDestinationFailures(t *testing.T) {
	s := server(t)
	q := sqsClient(s, "test")
	r := newRemote(t)

	// A response that no retry would change goes to the dead-letter queue at once.
	r.status.Store(http.StatusBadRequest)
	c, dlq := deliveryFailure(t, s, "rejected", r.URL+"/hook/rejected", nil)
	emit(t, c, "rejected", `{"id":1}`)
	dead := receiveAttributes(t, q, dlq)
	if dead["ERROR_CODE"] != "ApiDestinationFailed" || dead["RETRY_ATTEMPTS"] != "0" ||
		!strings.Contains(dead["ERROR_MESSAGE"], "HTTP status 400") ||
		dead["RULE_ARN"] != "arn:aws:events:us-east-1:000000000000:rule/rejected" ||
		!strings.Contains(dead["TARGET_ARN"], ":api-destination/rejected/") {
		t.Fatalf("dead letter %v", dead)
	}

	if calls := r.calls(); len(calls) != 1 || calls[0].Header.Get("X-Key") != "k" {
		t.Fatalf("got %+v", calls)
	}

	// A server error is retried until the retries are used.
	r.reset()
	r.status.Store(http.StatusInternalServerError)
	c, dlq = deliveryFailure(
		t,
		s,
		"broken",
		r.URL+"/hook/broken",
		&ebtypes.RetryPolicy{MaximumRetryAttempts: aws.Int32(0)},
	)
	emit(t, c, "broken", `{"id":2}`)
	dead = receiveAttributes(t, q, dlq)
	if dead["ERROR_CODE"] != "ApiDestinationRetryable" || !strings.Contains(dead["ERROR_MESSAGE"], "HTTP status 500") ||
		dead["EXHAUSTED_RETRY_CONDITION"] != "MaximumRetryAttemptsExhausted" {
		t.Fatalf("dead letter %v", dead)
	}

	// A delivery that fails once is retried and then succeeds.
	r.reset()
	c, dlq = deliveryFailure(t, s, "flaky", r.URL+"/hook/flaky", nil)
	r.status.Store(http.StatusServiceUnavailable)
	emit(t, c, "flaky", `{"id":3}`)
	r.status.Store(http.StatusOK)
	waitFor(t, "the delivery to be retried", func() bool { return len(r.calls()) >= 2 })
	for _, got := range r.calls() {
		if event := eventOf(t, &got); event["source"] != "flaky" {
			t.Fatalf("got %+v", got)
		}
	}

	if out, err := q.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: &dlq}); err != nil ||
		len(out.Messages) != 0 {
		t.Fatalf("dead letter %+v, %v", out, err)
	}

	// A connection that is not authorized fails the delivery, and works again once it is.
	r.reset()
	r.status.Store(http.StatusOK)
	c, dlq = deliveryFailure(t, s, "locked", r.URL+"/hook/locked", nil)
	if _, err := c.DeauthorizeConnection(context.Background(), &eventbridge.DeauthorizeConnectionInput{
		Name: aws.String("locked"),
	}); err != nil {
		t.Fatal(err)
	}

	emit(t, c, "locked", `{"id":4}`)
	dead = receiveAttributes(t, q, dlq)
	if dead["ERROR_CODE"] != "ApiDestinationFailed" || !strings.Contains(dead["ERROR_MESSAGE"], "DEAUTHORIZED") ||
		len(r.calls()) != 0 {
		t.Fatalf("dead letter %v", dead)
	}

	_, err := c.UpdateConnection(context.Background(), &eventbridge.UpdateConnectionInput{
		Name: aws.String("locked"), AuthParameters: &ebtypes.UpdateConnectionAuthRequestParameters{
			ApiKeyAuthParameters: &ebtypes.UpdateConnectionApiKeyAuthRequestParameters{ApiKeyValue: aws.String("k2")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	emit(t, c, "locked", `{"id":5}`)
	if got := r.only(t); got.Header.Get("X-Key") != "k2" {
		t.Fatalf("got %+v", got)
	}

	// A deleted API destination fails the delivery without retries.
	if _, err = c.DeleteApiDestination(context.Background(), &eventbridge.DeleteApiDestinationInput{
		Name: aws.String("locked"),
	}); err != nil {
		t.Fatal(err)
	}

	emit(t, c, "locked", `{"id":6}`)
	dead = receiveAttributes(t, q, dlq)
	if dead["ERROR_CODE"] != "ResourceNotFoundException" || dead["RETRY_ATTEMPTS"] != "0" {
		t.Fatalf("dead letter %v", dead)
	}

	// An endpoint nobody listens on is a transport failure, which is retried.
	gone := httptest.NewServer(http.NotFoundHandler())
	endpoint := gone.URL + "/hook/gone?token=query-secret"
	gone.Close()
	c, dlq = deliveryFailure(t, s, "gone", endpoint, &ebtypes.RetryPolicy{MaximumRetryAttempts: aws.Int32(0)})
	emit(t, c, "gone", `{"id":7}`)
	dead = receiveAttributes(t, q, dlq)
	if dead["ERROR_CODE"] != "ApiDestinationRetryable" || strings.Contains(dead["ERROR_MESSAGE"], "query-secret") ||
		dead["EXHAUSTED_RETRY_CONDITION"] != "MaximumRetryAttemptsExhausted" {
		t.Fatalf("dead letter %v", dead)
	}

	// A failed authorization endpoint fails the delivery too.
	r.reset()
	r.status.Store(http.StatusOK)
	connection := createConnection(t, c, "tokenless", ebtypes.ConnectionAuthorizationTypeOauthClientCredentials,
		&ebtypes.CreateConnectionAuthRequestParameters{OAuthParameters: &ebtypes.CreateConnectionOAuthRequestParameters{
			AuthorizationEndpoint: aws.String(r.URL + "/missing-token-endpoint"),
			HttpMethod:            ebtypes.ConnectionOAuthHttpMethodPost,
			ClientParameters: &ebtypes.CreateConnectionOAuthClientRequestParameters{
				ClientID: aws.String("c"), ClientSecret: aws.String("s"),
			},
		}})
	destination := createDestination(t, c, "tokenless", connection, r.URL+"/hook/tokenless",
		ebtypes.ApiDestinationHttpMethodPost)
	putRuleSource(t, c, "tokenless")
	_, err = c.PutTargets(context.Background(), &eventbridge.PutTargetsInput{
		Rule: aws.String("tokenless"),
		Targets: []ebtypes.Target{{
			Id: aws.String("t"), Arn: aws.String(destination),
			RetryPolicy:      &ebtypes.RetryPolicy{MaximumRetryAttempts: aws.Int32(0)},
			DeadLetterConfig: &ebtypes.DeadLetterConfig{Arn: aws.String("arn:aws:sqs:us-east-1:000000000000:gone-dlq")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	emit(t, c, "tokenless", `{"id":8}`)
	dead = receiveAttributes(t, q, dlq)
	if dead["ERROR_CODE"] != "ApiDestinationFailed" ||
		!strings.Contains(dead["ERROR_MESSAGE"], "authorization endpoint") {
		t.Fatalf("dead letter %v", dead)
	}
}

func TestEventBridgeApiDestinationsInCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	stacks := cfnClient(s)
	remote := newRemote(t)
	template := `
Resources:
  Connection:
    Type: AWS::Events::Connection
    Properties:
      Name: stack-connection
      Description: from a stack
      AuthorizationType: API_KEY
      AuthParameters:
        ApiKeyAuthParameters: {ApiKeyName: X-Key, ApiKeyValue: stack-key}
        InvocationHttpParameters:
          HeaderParameters: [{Key: X-Secret, Value: stack-secret, IsValueSecret: true}]
  Destination:
    Type: AWS::Events::ApiDestination
    Properties:
      Name: stack-destination
      ConnectionArn: !GetAtt Connection.Arn
      InvocationEndpoint: ` + remote.URL + `/hook/stack
      HttpMethod: POST
      InvocationRateLimitPerSecond: 20
  Rule:
    Type: AWS::Events::Rule
    Properties:
      Name: stack-rule
      EventPattern: {source: [stack]}
      Targets:
        - Id: hook
          Arn: !GetAtt Destination.Arn
          HttpParameters:
            HeaderParameters: {X-Target: from-stack}
Outputs:
  Connection: {Value: !Ref Connection}
  ConnectionArn: {Value: !GetAtt Connection.Arn}
  SecretArn: {Value: !GetAtt Connection.SecretArn}
  Destination: {Value: !Ref Destination}
  DestinationArn: {Value: !GetAtt Destination.Arn}
`
	_, err := stacks.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("hooks"), TemplateBody: aws.String(template),
	})
	if err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, stacks, "hooks", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)
	c := eventsClient(s, "test")
	described, err := c.DescribeConnection(
		ctx,
		&eventbridge.DescribeConnectionInput{Name: aws.String("stack-connection")},
	)
	if err != nil || outputs["Connection"] != "stack-connection" ||
		outputs["ConnectionArn"] != aws.ToString(described.ConnectionArn) ||
		outputs["SecretArn"] != aws.ToString(
			described.SecretArn,
		) || aws.ToString(described.Description) != "from a stack" {
		t.Fatalf("outputs %v, connection %+v, %v", outputs, described, err)
	}

	destination, err := c.DescribeApiDestination(ctx, &eventbridge.DescribeApiDestinationInput{
		Name: aws.String("stack-destination"),
	})
	if err != nil || outputs["Destination"] != "stack-destination" ||
		outputs["DestinationArn"] != aws.ToString(destination.ApiDestinationArn) ||
		aws.ToInt32(destination.InvocationRateLimitPerSecond) != 20 {
		t.Fatalf("outputs %v, destination %+v, %v", outputs, destination, err)
	}

	emit(t, c, "stack", `{"id":1}`)
	if got := remote.only(t); got.Header.Get("X-Key") != "stack-key" || got.Header.Get("X-Secret") != "stack-secret" ||
		got.Header.Get("X-Target") != "from-stack" || got.Path != "/hook/stack" {
		t.Fatalf("got %+v", got)
	}

	if _, err = stacks.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("hooks")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the stack to be deleted", func() bool {
		_, describeErr := c.DescribeConnection(
			ctx,
			&eventbridge.DescribeConnectionInput{Name: aws.String("stack-connection")},
		)
		return describeErr != nil
	})
	_, err = c.DescribeApiDestination(
		ctx,
		&eventbridge.DescribeApiDestinationInput{Name: aws.String("stack-destination")},
	)
	code(t, err, "ResourceNotFoundException")
}
