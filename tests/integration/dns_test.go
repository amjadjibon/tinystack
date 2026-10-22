package integration_test

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/dns"
)

func TestDNSSDKEndpoint(t *testing.T) {
	endpoint := server(t)
	listener, err := dns.Start(context.Background(), "127.0.0.1:0", "127.0.0.1", []string{"127.0.0.1:1"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(listener.Stop)
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, listener.Address())
	}}
	_, port, err := net.SplitHostPort(endpoint.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, splitErr
		}

		ips, lookupErr := resolver.LookupHost(ctx, host)
		if lookupErr != nil {
			return nil, lookupErr
		}

		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ips[0], port))
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := sts.New(sts.Options{
		Region: "us-east-1", HTTPClient: &http.Client{Transport: transport},
		EndpointOptions: sts.EndpointResolverOptions{DisableHTTPS: true},
		Retryer:         aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "000000000000", SecretAccessKey: "test"}, nil
		}),
	})
	identity, err := client.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(identity.Account) != "000000000000" {
		t.Fatalf("wrong identity: %v", identity)
	}
}
