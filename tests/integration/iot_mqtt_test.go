package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	ci "github.com/aws/aws-sdk-go-v2/service/cognitoidentity"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
	"github.com/coder/websocket"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/amjadjibon/tinystack/internal/config"
)

// emptyPayloadHash is the SHA-256 of an empty body, which MQTT over
// WebSocket URLs are presigned with.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func dataClient(s *httptest.Server, key string) *iotdataplane.Client {
	return iotdataplane.New(iotdataplane.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// mqttURL is the unsigned MQTT over WebSocket URL of a test server.
func mqttURL(s *httptest.Server) string { return "ws" + strings.TrimPrefix(s.URL, "http") + "/mqtt" }

// presignMQTT presigns the server's MQTT URL with credentials for the
// iotdevicegateway service. A session token is appended after signing, as
// the AWS IoT device SDKs do.
func presignMQTT(t *testing.T, s *httptest.Server, creds *aws.Credentials) string {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.URL+"/mqtt", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	token := creds.SessionToken
	unsigned := *creds
	unsigned.SessionToken = ""
	signed, _, err := v4.NewSigner().PresignHTTP(context.Background(), unsigned, r, emptyPayloadHash,
		"iotdevicegateway", "us-east-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if token != "" {
		signed += "&X-Amz-Security-Token=" + url.QueryEscape(token)
	}

	return "ws" + strings.TrimPrefix(signed, "http")
}

// mqttClient is a minimal MQTT 5 client over WebSocket, built on the broker's
// own packet codec.
type mqttClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	ids  uint16
}

func dialMQTT(t *testing.T, rawURL string) (*mqttClient, error) {
	t.Helper()
	ws, resp, err := websocket.Dial(
		context.Background(),
		rawURL,
		&websocket.DialOptions{Subprotocols: []string{"mqtt"}},
	)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		return nil, err
	}

	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	t.Cleanup(func() { _ = conn.Close() })
	return &mqttClient{t: t, conn: conn, r: bufio.NewReader(conn)}, nil
}

// connected dials rawURL and connects as id, failing the test unless the
// broker accepts.
func connected(t *testing.T, rawURL, id string) *mqttClient {
	t.Helper()
	c, err := dialMQTT(t, rawURL)
	if err != nil {
		t.Fatal(err)
	}

	if code := c.connect(id); code != 0 {
		t.Fatalf("connect %s: reason 0x%x", id, code)
	}

	return c
}

func (c *mqttClient) send(pk *packets.Packet) {
	c.t.Helper()
	pk.ProtocolVersion = 5
	var buf bytes.Buffer
	var err error
	switch pk.FixedHeader.Type {
	case packets.Connect:
		err = pk.ConnectEncode(&buf)
	case packets.Publish:
		err = pk.PublishEncode(&buf)
	case packets.Puback:
		err = pk.PubackEncode(&buf)
	case packets.Subscribe:
		err = pk.SubscribeEncode(&buf)
	case packets.Disconnect:
		err = pk.DisconnectEncode(&buf)
	}

	if err == nil {
		_, err = c.conn.Write(buf.Bytes())
	}

	if err != nil {
		c.t.Fatal(err)
	}
}

// receive reads the next packet, or fails with the read error.
func (c *mqttClient) receive() (packets.Packet, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, err := c.r.ReadByte()
	if err != nil {
		return packets.Packet{}, err
	}

	pk := packets.Packet{ProtocolVersion: 5}
	if headerErr := pk.FixedHeader.Decode(first); headerErr != nil {
		return pk, headerErr
	}

	n, _, err := packets.DecodeLength(c.r)
	if err != nil {
		return pk, err
	}

	pk.FixedHeader.Remaining = n
	body := make([]byte, n)
	if _, err = io.ReadFull(c.r, body); err != nil {
		return pk, err
	}

	switch pk.FixedHeader.Type {
	case packets.Connack:
		err = pk.ConnackDecode(body)
	case packets.Publish:
		err = pk.PublishDecode(body)
	case packets.Puback:
		err = pk.PubackDecode(body)
	case packets.Suback:
		err = pk.SubackDecode(body)
	case packets.Disconnect:
		err = pk.DisconnectDecode(body)
	}

	return pk, err
}

// expect reads packets until one of type kind arrives, acknowledging the
// QoS 1 messages it passes.
func (c *mqttClient) expect(kind byte) packets.Packet {
	c.t.Helper()
	for {
		pk, err := c.receive()
		if err != nil {
			c.t.Fatalf("waiting for packet type %d: %v", kind, err)
		}

		if pk.FixedHeader.Type == packets.Publish && pk.FixedHeader.Qos == 1 {
			c.send(&packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Puback}, PacketID: pk.PacketID})
		}

		if pk.FixedHeader.Type == kind {
			return pk
		}
	}
}

// message waits for a message published to topic.
func (c *mqttClient) message(topic string) packets.Packet {
	c.t.Helper()
	for {
		if pk := c.expect(packets.Publish); pk.TopicName == topic {
			return pk
		}
	}
}

func (c *mqttClient) connect(id string) byte {
	c.t.Helper()
	c.send(&packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Connect},
		Connect: packets.ConnectParams{
			ProtocolName: []byte("MQTT"), ClientIdentifier: id, Clean: true, Keepalive: 60,
		},
	})
	pk, err := c.receive()
	if err != nil {
		c.t.Fatal(err)
	}

	return pk.ReasonCode
}

func (c *mqttClient) subscribe(filters ...string) []byte {
	c.t.Helper()
	c.ids++
	pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Subscribe, Qos: 1}, PacketID: c.ids}
	for _, f := range filters {
		pk.Filters = append(pk.Filters, packets.Subscription{Filter: f, Qos: 1})
	}

	c.send(&pk)
	return c.expect(packets.Suback).ReasonCodes
}

// publish publishes at QoS 1 and returns the PUBACK's reason code.
func (c *mqttClient) publish(topic, payload string, retain bool) byte {
	c.t.Helper()
	c.ids++
	c.send(&packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 1, Retain: retain},
		TopicName:   topic, Payload: []byte(payload), PacketID: c.ids,
	})
	return c.expect(packets.Puback).ReasonCode
}

func TestIoTMQTT(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	data := dataClient(s, "test")

	watcher := connected(t, mqttURL(s), "watcher")
	codes := watcher.subscribe("sensors/+/temp", "$aws/events/presence/+/+", "$SYS/#")
	if len(codes) != 3 || codes[0] != 1 || codes[1] != 1 || codes[2] < 0x80 {
		t.Fatalf("suback codes = %v", codes)
	}

	device := connected(t, mqttURL(s), "device-1")
	var event map[string]any
	if err := json.Unmarshal(watcher.message("$aws/events/presence/connected/device-1").Payload, &event); err != nil ||
		event["clientId"] != "device-1" || event["eventType"] != "connected" || event["versionNumber"] != 1.0 {
		t.Fatalf("connected event = %v, %v", event, err)
	}

	if code := device.publish("sensors/a/temp", "21", false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	if got := watcher.message("sensors/a/temp"); string(got.Payload) != "21" {
		t.Fatalf("message = %+v", got)
	}

	// Reserved topics refuse clients' messages.
	if code := device.publish("$aws/events/presence/connected/fake", "{}", false); code != 0x87 {
		t.Fatalf("puback to a reserved topic 0x%x", code)
	}

	// Retained messages are kept for later subscribers and the data plane.
	if code := device.publish("sensors/b/temp", "19", true); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	got, err := data.GetRetainedMessage(ctx, &iotdataplane.GetRetainedMessageInput{Topic: aws.String("sensors/b/temp")})
	if err != nil || string(got.Payload) != "19" || got.Qos != 1 || got.LastModifiedTime == 0 {
		t.Fatalf("%+v %v", got, err)
	}

	listed, err := data.ListRetainedMessages(ctx, &iotdataplane.ListRetainedMessagesInput{})
	if err != nil || len(listed.RetainedTopics) != 1 || listed.RetainedTopics[0].PayloadSize != 2 {
		t.Fatalf("%+v %v", listed, err)
	}

	late := connected(t, mqttURL(s), "late")
	late.subscribe("sensors/b/temp")
	if msg := late.message("sensors/b/temp"); string(msg.Payload) != "19" || !msg.FixedHeader.Retain {
		t.Fatalf("retained = %+v", msg)
	}

	// The data plane publishes with MQTT 5 properties.
	if _, err = data.Publish(ctx, &iotdataplane.PublishInput{
		Topic: aws.String("sensors/c/temp"), Qos: 1, Payload: []byte(`{"c":1}`),
		ContentType: aws.String("application/json"), ResponseTopic: aws.String("replies/c"),
	}); err != nil {
		t.Fatal(err)
	}

	msg := watcher.message("sensors/c/temp")
	if string(msg.Payload) != `{"c":1}` || msg.Properties.ContentType != "application/json" ||
		msg.Properties.ResponseTopic != "replies/c" {
		t.Fatalf("message = %+v", msg)
	}

	_, err = data.Publish(ctx, &iotdataplane.PublishInput{
		Topic: aws.String("sensors/d/temp"), UserProperties: aws.String(`[{"a":"b"}]`),
	})
	wantCode(t, err, "NotImplemented")
	_, err = data.Publish(ctx, &iotdataplane.PublishInput{Topic: aws.String("$aws/things/t/shadow/update")})
	wantCode(t, err, "InvalidRequestException")

	conn, err := data.GetConnection(ctx, &iotdataplane.GetConnectionInput{
		ClientId: aws.String("device-1"), IncludeSocketInformation: true,
	})
	if err != nil || !conn.Connected || conn.ConnectedSince == 0 || aws.ToString(conn.SourceIp) != "127.0.0.1" ||
		!conn.CleanSession {
		t.Fatalf("%+v %v", conn, err)
	}

	subs, err := data.ListSubscriptions(ctx, &iotdataplane.ListSubscriptionsInput{ClientId: aws.String("watcher")})
	if err != nil || len(subs.Subscriptions) != 2 {
		t.Fatalf("%+v %v", subs, err)
	}

	// DeleteConnection disconnects the client, and the event says why.
	if _, err = data.DeleteConnection(ctx, &iotdataplane.DeleteConnectionInput{
		ClientId: aws.String("device-1"),
	}); err != nil {
		t.Fatal(err)
	}

	if err = json.Unmarshal(
		watcher.message("$aws/events/presence/disconnected/device-1").Payload,
		&event,
	); err != nil ||
		event["disconnectReason"] != "API_INITIATED_DISCONNECT" {
		t.Fatalf("disconnected event = %v, %v", event, err)
	}

	waitFor(t, "device-1's session to end", func() bool {
		_, getErr := data.GetConnection(ctx, &iotdataplane.GetConnectionInput{ClientId: aws.String("device-1")})
		return errorCode(getErr) == "ResourceNotFoundException"
	})

	// A presigned connection belongs to its signer's account, whose topics
	// and retained messages are its own.
	other := connected(t, presignMQTT(t, s, &aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "x"}), "watcher")
	other.subscribe("sensors/b/temp")
	theirs, err := dataClient(s, "team-b").ListRetainedMessages(ctx, &iotdataplane.ListRetainedMessagesInput{})
	if err != nil || len(theirs.RetainedTopics) != 0 {
		t.Fatalf("%+v %v", theirs, err)
	}

	watcher.publish("sensors/b/temp", "", true)
	waitFor(t, "the retained message to be cleared", func() bool {
		_, getErr := data.GetRetainedMessage(ctx, &iotdataplane.GetRetainedMessageInput{
			Topic: aws.String("sensors/b/temp"),
		})
		return errorCode(getErr) == "ResourceNotFoundException"
	})
}

func TestIoTMQTTPolicies(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()

	// Unsigned connections are refused; IAM callers without IoT policies are
	// allowed, as IAM policies are not evaluated.
	if _, err := dialMQTT(t, mqttURL(s)); err == nil {
		t.Fatal("an unsigned connection was accepted")
	}

	admin := connected(t, presignMQTT(t, s, &aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}), "admin")
	if code := admin.publish("anywhere/at/all", "x", false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	if _, err := dialMQTT(
		t,
		presignMQTT(t, s, &aws.Credentials{AccessKeyID: "test", SecretAccessKey: "bad"}),
	); err == nil {
		t.Fatal("a connection with a bad signature was accepted")
	}

	// A Cognito identity's attached IoT policy limits its client ID and topics.
	identities := identityClient(s)
	pool, err := identities.CreateIdentityPool(ctx, &ci.CreateIdentityPoolInput{
		IdentityPoolName: aws.String("devices"), AllowUnauthenticatedIdentities: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	poolID := aws.ToString(pool.IdentityPoolId)
	if _, err = identities.SetIdentityPoolRoles(ctx, &ci.SetIdentityPoolRolesInput{
		IdentityPoolId: &poolID,
		Roles:          map[string]string{"unauthenticated": cognitoRole(t, s, "guest", poolID, "unauthenticated")},
	}); err != nil {
		t.Fatal(err)
	}

	guest, err := identities.GetId(ctx, &ci.GetIdInput{IdentityPoolId: &poolID})
	if err != nil {
		t.Fatal(err)
	}

	creds, err := identities.GetCredentialsForIdentity(ctx, &ci.GetCredentialsForIdentityInput{
		IdentityId: guest.IdentityId,
	})
	if err != nil {
		t.Fatal(err)
	}

	devices := iotClient(s, "test")
	sub := "${cognito-identity.amazonaws.com:sub}"
	if _, err = devices.CreatePolicy(ctx, &iot.CreatePolicyInput{
		PolicyName: aws.String("own-topics"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[` +
			`{"Effect":"Allow","Action":"iot:Connect","Resource":"arn:aws:iot:us-east-1:000000000000:client/` + sub + `"},` +
			`{"Effect":"Allow","Action":["iot:Publish","iot:Receive"],` +
			`"Resource":"arn:aws:iot:us-east-1:000000000000:topic/devices/` + sub + `/*"},` +
			`{"Effect":"Allow","Action":"iot:Subscribe",` +
			`"Resource":"arn:aws:iot:us-east-1:000000000000:topicfilter/devices/` + sub + `/*"}]}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = devices.AttachPolicy(ctx, &iot.AttachPolicyInput{
		PolicyName: aws.String("own-topics"), Target: guest.IdentityId,
	}); err != nil {
		t.Fatal(err)
	}

	signed := presignMQTT(t, s, &aws.Credentials{
		AccessKeyID: aws.ToString(
			creds.Credentials.AccessKeyId,
		),
		SecretAccessKey: aws.ToString(creds.Credentials.SecretKey),
		SessionToken:    aws.ToString(creds.Credentials.SessionToken),
	})
	refused, err := dialMQTT(t, signed)
	if err != nil {
		t.Fatal(err)
	}

	if code := refused.connect("someone-else"); code != packets.ErrBadUsernameOrPassword.Code {
		t.Fatalf("connect as another client: reason 0x%x", code)
	}

	id := aws.ToString(guest.IdentityId)
	device := connected(t, signed, id)
	if codes := device.subscribe("devices/"+id+"/*", "devices/other/*"); len(codes) != 2 || codes[0] != 1 ||
		codes[1] != packets.ErrNotAuthorized.Code {
		t.Fatalf("suback codes = %v", codes)
	}

	if code := device.publish("devices/"+id+"/*", "mine", false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	if msg := device.message("devices/" + id + "/*"); string(msg.Payload) != "mine" {
		t.Fatalf("message = %+v", msg)
	}

	if code := device.publish("devices/other/x", "theirs", false); code != packets.ErrNotAuthorized.Code {
		t.Fatalf("puback 0x%x", code)
	}

	// Detaching the policy leaves IAM alone in charge.
	if _, err = devices.DetachPolicy(ctx, &iot.DetachPolicyInput{
		PolicyName: aws.String("own-topics"), Target: guest.IdentityId,
	}); err != nil {
		t.Fatal(err)
	}

	if code := device.publish("devices/other/x", "theirs", false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}
}
