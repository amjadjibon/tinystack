package integration_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestIoTCustomAuthorizers(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	cfg.IoT.MQTTSPort = 8883
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	c := iotClient(s, "test")
	fn := createFunction(t, lambdaClient(s, "test"), "iot-authorizer", "app.iotAuthorize")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	public := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	sign := func(token string) string {
		digest := sha256.Sum256([]byte(token))
		sig, signErr := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if signErr != nil {
			t.Fatal(signErr)
		}

		return base64.StdEncoding.EncodeToString(sig)
	}

	// Signing authorizers need a token key and keys.
	_, err = c.CreateAuthorizer(ctx, &iot.CreateAuthorizerInput{
		AuthorizerName: aws.String("bad"), AuthorizerFunctionArn: fn.FunctionArn,
	})
	wantCode(t, err, "InvalidRequestException")

	created, err := c.CreateAuthorizer(ctx, &iot.CreateAuthorizerInput{
		AuthorizerName:         aws.String("signed"),
		AuthorizerFunctionArn:  fn.FunctionArn,
		TokenKeyName:           aws.String("token"),
		TokenSigningPublicKeys: map[string]string{"k1": public},
		Status:                 iottypes.AuthorizerStatusActive,
		Tags:                   []iottypes.Tag{{Key: aws.String("team"), Value: aws.String("a")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateAuthorizer(ctx, &iot.CreateAuthorizerInput{
		AuthorizerName: aws.String("open"), AuthorizerFunctionArn: fn.FunctionArn,
		TokenKeyName: aws.String("token"), SigningDisabled: aws.Bool(true), Status: iottypes.AuthorizerStatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeAuthorizer(ctx, &iot.DescribeAuthorizerInput{AuthorizerName: aws.String("signed")})
	if err != nil ||
		aws.ToString(described.AuthorizerDescription.AuthorizerArn) != aws.ToString(created.AuthorizerArn) ||
		described.AuthorizerDescription.TokenSigningPublicKeys["k1"] != public {
		t.Fatalf("described = %+v, %v", described, err)
	}

	tags, err := c.ListTagsForResource(ctx, &iot.ListTagsForResourceInput{ResourceArn: created.AuthorizerArn})
	if err != nil || len(tags.Tags) != 1 {
		t.Fatalf("tags = %+v, %v", tags, err)
	}

	list, err := c.ListAuthorizers(ctx, &iot.ListAuthorizersInput{})
	if err != nil || len(list.Authorizers) != 2 {
		t.Fatalf("authorizers = %+v, %v", list, err)
	}

	// TestInvokeAuthorizer checks the signature, then asks the function.
	tested, err := c.TestInvokeAuthorizer(ctx, &iot.TestInvokeAuthorizerInput{
		AuthorizerName: aws.String("signed"), Token: aws.String("let-me-in"),
		TokenSignature: aws.String(sign("let-me-in")),
	})
	if err != nil || !aws.ToBool(tested.IsAuthenticated) || aws.ToString(tested.PrincipalId) != "device-user" ||
		len(tested.PolicyDocuments) != 1 || aws.ToInt32(tested.DisconnectAfterInSeconds) != 60 {
		t.Fatalf("tested = %+v, %v", tested, err)
	}

	tested, err = c.TestInvokeAuthorizer(ctx, &iot.TestInvokeAuthorizerInput{
		AuthorizerName: aws.String("signed"), Token: aws.String("let-me-in"), TokenSignature: aws.String(sign("x")),
	})
	if err != nil || aws.ToBool(tested.IsAuthenticated) {
		t.Fatalf("a bad signature = %+v, %v", tested, err)
	}

	// An unsigned WebSocket names the authorizer and signs its token in the
	// query; the authorizer's policies decide what it may do.
	query := url.Values{
		"x-amz-customauthorizer-name": {"signed"}, "token": {"let-me-in"},
		"x-amz-customauthorizer-signature": {sign("let-me-in")},
	}
	device, err := dialMQTT(t, mqttURL(s)+"?"+query.Encode())
	if err != nil {
		t.Fatal(err)
	}

	if code := device.connect("custom-1"); code != 0 {
		t.Fatalf("connect: reason 0x%x", code)
	}

	if codes := device.subscribe("allowed/#", "other/#"); len(codes) != 2 || codes[0] != 1 || codes[1] < 0x80 {
		t.Fatalf("suback = %v", codes)
	}

	if code := device.publish("allowed/x", "hi", false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	if msg := device.message("allowed/x"); string(msg.Payload) != "hi" {
		t.Fatalf("message = %+v", msg)
	}

	if code := device.publish("other/x", "no", false); code != packets.ErrNotAuthorized.Code {
		t.Fatalf("puback to other/x 0x%x", code)
	}

	// A wrong token, or a forged signature, is refused.
	for _, q := range []url.Values{
		{"x-amz-customauthorizer-name": {"signed"}, "token": {"nope"}, "x-amz-customauthorizer-signature": {sign("nope")}},
		{"x-amz-customauthorizer-name": {"signed"}, "token": {"let-me-in"}, "x-amz-customauthorizer-signature": {sign("x")}},
	} {
		refused, dialErr := dialMQTT(t, mqttURL(s)+"?"+q.Encode())
		if dialErr != nil {
			t.Fatal(dialErr)
		}

		if code := refused.connect("custom-2"); code != packets.ErrBadUsernameOrPassword.Code {
			t.Fatalf("connect %v: reason 0x%x", q, code)
		}
	}

	// MQTT over TLS without a certificate names the authorizer in its
	// username.
	tlsDevice, err := dialMQTTS(t, mqttsAddr(t, s), &tls.Config{RootCAs: rootCA(t, s), ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}

	if code := tlsDevice.connectAs(
		"custom-3",
		"me?x-amz-customauthorizer-name=open&token=let-me-in",
		"secret",
	); code != 0 {
		t.Fatalf("TLS connect: reason 0x%x", code)
	}

	// The default authorizer serves clients that name none.
	if _, err = c.SetDefaultAuthorizer(
		ctx,
		&iot.SetDefaultAuthorizerInput{AuthorizerName: aws.String("open")},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.SetDefaultAuthorizer(ctx, &iot.SetDefaultAuthorizerInput{AuthorizerName: aws.String("signed")})
	wantCode(t, err, "ResourceAlreadyExistsException")
	def, err := c.DescribeDefaultAuthorizer(ctx, &iot.DescribeDefaultAuthorizerInput{})
	if err != nil || aws.ToString(def.AuthorizerDescription.AuthorizerName) != "open" {
		t.Fatalf("default = %+v, %v", def, err)
	}

	byDefault, err := dialMQTT(t, mqttURL(s)+"?token=let-me-in")
	if err != nil {
		t.Fatal(err)
	}

	if code := byDefault.connect("custom-4"); code != 0 {
		t.Fatalf("default connect: reason 0x%x", code)
	}

	// An active authorizer, or the default one, cannot be deleted.
	_, err = c.DeleteAuthorizer(ctx, &iot.DeleteAuthorizerInput{AuthorizerName: aws.String("signed")})
	wantCode(t, err, "DeleteConflictException")
	if _, err = c.ClearDefaultAuthorizer(ctx, &iot.ClearDefaultAuthorizerInput{}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeDefaultAuthorizer(ctx, &iot.DescribeDefaultAuthorizerInput{})
	wantCode(t, err, "ResourceNotFoundException")
	if _, err = c.UpdateAuthorizer(ctx, &iot.UpdateAuthorizerInput{
		AuthorizerName: aws.String("signed"), Status: iottypes.AuthorizerStatusInactive,
	}); err != nil {
		t.Fatal(err)
	}

	// An inactive authorizer lets nobody in.
	inactive, err := dialMQTT(t, mqttURL(s)+"?"+query.Encode())
	if err != nil {
		t.Fatal(err)
	}

	if code := inactive.connect("custom-5"); code != packets.ErrBadUsernameOrPassword.Code {
		t.Fatalf("inactive connect: reason 0x%x", code)
	}

	if _, err = c.DeleteAuthorizer(ctx, &iot.DeleteAuthorizerInput{AuthorizerName: aws.String("signed")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeAuthorizer(ctx, &iot.DescribeAuthorizerInput{AuthorizerName: aws.String("signed")})
	wantCode(t, err, "ResourceNotFoundException")
}
