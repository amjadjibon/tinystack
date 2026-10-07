package integration_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestIoTCredentialsProvider(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	cfg.IoT.MQTTSPort = 8883
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	c := iotClient(s, "test")
	roles := iamClient(s, "test", "us-east-1")
	trust := func(service string) *string {
		return aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"` +
			service + `"},"Action":"sts:AssumeRole"}]}`)
	}

	for name, service := range map[string]string{
		"devices": "credentials.iot.amazonaws.com", "other": "ec2.amazonaws.com",
	} {
		if _, err := roles.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName: aws.String(name), AssumeRolePolicyDocument: trust(service),
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := c.CreateRoleAlias(ctx, &iot.CreateRoleAliasInput{
		RoleAlias: aws.String("bad"), RoleArn: aws.String("arn:aws:iam::000000000000:role/devices"),
		CredentialDurationSeconds: aws.Int32(60),
	})
	wantCode(t, err, "InvalidRequestException")
	for alias, role := range map[string]string{"devices": "devices", "untrusted": "other"} {
		if _, err = c.CreateRoleAlias(ctx, &iot.CreateRoleAliasInput{
			RoleAlias: aws.String(alias), RoleArn: aws.String("arn:aws:iam::000000000000:role/" + role),
		}); err != nil {
			t.Fatal(err)
		}
	}

	described, err := c.DescribeRoleAlias(ctx, &iot.DescribeRoleAliasInput{RoleAlias: aws.String("devices")})
	if err != nil || aws.ToInt32(described.RoleAliasDescription.CredentialDurationSeconds) != 3600 ||
		aws.ToString(described.RoleAliasDescription.RoleAliasArn) !=
			"arn:aws:iot:us-east-1:000000000000:rolealias/devices" {
		t.Fatalf("described = %+v, %v", described, err)
	}

	list, err := c.ListRoleAliases(ctx, &iot.ListRoleAliasesInput{})
	if err != nil || strings.Join(list.RoleAliases, ",") != "devices,untrusted" {
		t.Fatalf("aliases = %+v, %v", list, err)
	}

	// A device certificate attached to a thing, whose policy allows
	// AssumeRoleWithCertificate on both aliases.
	cert, err := c.CreateKeysAndCertificate(ctx, &iot.CreateKeysAndCertificateInput{SetAsActive: true})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String("meter")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AttachThingPrincipal(ctx, &iot.AttachThingPrincipalInput{
		ThingName: aws.String("meter"), Principal: cert.CertificateArn,
	}); err != nil {
		t.Fatal(err)
	}

	pair, err := tls.X509KeyPair([]byte(aws.ToString(cert.CertificatePem)),
		[]byte(aws.ToString(cert.KeyPair.PrivateKey)))
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: rootCA(t, s), Certificates: []tls.Certificate{pair}, ServerName: "127.0.0.1",
	}}}
	fetch := func(alias, thing string) (int, map[string]any) {
		t.Helper()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://"+mqttsAddr(t, s)+"/role-aliases/"+alias+"/credentials", http.NoBody)
		if reqErr != nil {
			t.Fatal(reqErr)
		}

		if thing != "" {
			req.Header.Set("x-amzn-iot-thingname", thing)
		}

		resp, reqErr := client.Do(req)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return resp.StatusCode, out
	}

	// Without a policy, a certificate may assume nothing in verify mode.
	if status, _ := fetch("devices", "meter"); status != http.StatusForbidden {
		t.Fatalf("without a policy: status %d", status)
	}

	if _, err = c.CreatePolicy(ctx, &iot.CreatePolicyInput{
		PolicyName: aws.String("assume"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":"iot:AssumeRoleWithCertificate","Resource":"arn:aws:iot:us-east-1:000000000000:rolealias/*"}]}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AttachPolicy(ctx, &iot.AttachPolicyInput{
		PolicyName: aws.String("assume"), Target: cert.CertificateArn,
	}); err != nil {
		t.Fatal(err)
	}

	status, out := fetch("devices", "meter")
	if status != http.StatusOK {
		t.Fatalf("credentials: status %d, %v", status, out)
	}

	creds, _ := out["credentials"].(map[string]any)
	key, _ := creds["accessKeyId"].(string)
	secret, _ := creds["secretAccessKey"].(string)
	token, _ := creds["sessionToken"].(string)
	if key == "" || secret == "" || token == "" || creds["expiration"] == nil {
		t.Fatalf("credentials = %v", out)
	}

	// The credentials act as the role, in a session named for the thing.
	identity, err := sts.New(sts.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: secret, SessionToken: token}, nil
		}),
	}).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(identity.Arn) != "arn:aws:sts::000000000000:assumed-role/devices/meter" {
		t.Fatalf("identity = %+v, %v", identity, err)
	}

	// A role that does not trust the credentials provider, a thing the
	// certificate is not attached to, and a missing alias are refused.
	if status, _ = fetch("untrusted", ""); status != http.StatusForbidden {
		t.Fatalf("untrusted role: status %d", status)
	}

	if status, _ = fetch("devices", "someone-else"); status != http.StatusForbidden {
		t.Fatalf("unattached thing: status %d", status)
	}

	if status, _ = fetch("missing", ""); status != http.StatusNotFound {
		t.Fatalf("missing alias: status %d", status)
	}

	if _, err = c.DeleteRoleAlias(ctx, &iot.DeleteRoleAliasInput{RoleAlias: aws.String("untrusted")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeRoleAlias(ctx, &iot.DescribeRoleAliasInput{RoleAlias: aws.String("untrusted")})
	wantCode(t, err, "ResourceNotFoundException")
}
