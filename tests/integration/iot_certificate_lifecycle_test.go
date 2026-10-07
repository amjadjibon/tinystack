package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
	"github.com/mochi-mqtt/server/v2/packets"
)

// expectDropped waits for the broker to end the client's connection, with a
// DISCONNECT packet or by closing it.
func (c *mqttClient) expectDropped() {
	c.t.Helper()
	for {
		pk, err := c.receive()
		if err != nil || pk.FixedHeader.Type == packets.Disconnect {
			return
		}
	}
}

// expectDisconnectReason waits for the lifecycle event of clientID's
// disconnection on the watcher and checks its reason, then checks that
// GetConnection reports the same one.
func expectDisconnectReason(
	t *testing.T, watcher *mqttClient, data *iotdataplane.Client, clientID, want string,
) {
	t.Helper()
	var event map[string]any
	payload := watcher.message("$aws/events/presence/disconnected/" + clientID).Payload
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}

	if event["disconnectReason"] != want || event["clientInitiatedDisconnect"] != false {
		t.Fatalf("disconnected event = %v, want reason %s", event, want)
	}

	got, err := data.GetConnection(context.Background(),
		&iotdataplane.GetConnectionInput{ClientId: aws.String(clientID)})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(got.DisconnectReason) != want {
		t.Fatalf("GetConnection reason = %s, want %s", aws.ToString(got.DisconnectReason), want)
	}
}

// deviceConnection connects a device with a TLS client certificate.
func deviceConnection(t *testing.T, s *httptest.Server, cert *tls.Certificate, id string) *mqttClient {
	t.Helper()
	cl, err := dialMQTTS(t, mqttsAddr(t, s), &tls.Config{
		RootCAs: rootCA(t, s), Certificates: []tls.Certificate{*cert}, ServerName: "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A persistent session outlives the connection, so GetConnection can
	// report why it ended.
	cl.send(&packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Connect},
		Connect:     packets.ConnectParams{ProtocolName: []byte("MQTT"), ClientIdentifier: id, Keepalive: 60},
		Properties:  packets.Properties{SessionExpiryInterval: 600, SessionExpiryIntervalFlag: true},
	})
	if pk, err := cl.receive(); err != nil || pk.ReasonCode != 0 {
		t.Fatalf("connect %s: reason 0x%x, %v", id, pk.ReasonCode, err)
	}

	return cl
}

func TestIoTCertificateStatusDisconnectsClients(t *testing.T) {
	for _, status := range []iottypes.CertificateStatus{
		iottypes.CertificateStatusRevoked,
		iottypes.CertificateStatusInactive,
	} {
		t.Run(string(status), func(t *testing.T) {
			cfg := testConfig()
			cfg.IoT.MQTTSPort = 8883
			s := serverConfig(t, cfg, &fakeRuntime{})
			ctx := context.Background()
			iotCli := iotClient(s, "test")
			data := dataClient(s, "test")

			var certs [2]tls.Certificate
			var ids [2]string
			for i := range certs {
				out, err := iotCli.CreateKeysAndCertificate(ctx, &iot.CreateKeysAndCertificateInput{SetAsActive: true})
				if err != nil {
					t.Fatal(err)
				}

				ids[i] = aws.ToString(out.CertificateId)
				if certs[i], err = tls.X509KeyPair(
					[]byte(aws.ToString(out.CertificatePem)), []byte(aws.ToString(out.KeyPair.PrivateKey)),
				); err != nil {
					t.Fatal(err)
				}
			}

			watcher := connected(t, mqttURL(s), "watcher")
			watcher.subscribe("$aws/events/presence/disconnected/+", "keep/alive")
			doomed := deviceConnection(t, s, &certs[0], "doomed")
			survivor := deviceConnection(t, s, &certs[1], "survivor")

			if _, err := iotCli.UpdateCertificate(ctx, &iot.UpdateCertificateInput{
				CertificateId: &ids[0], NewStatus: status,
			}); err != nil {
				t.Fatal(err)
			}

			doomed.expectDropped()
			expectDisconnectReason(t, watcher, data, "doomed", "AUTH_ERROR")
			if code := survivor.publish("keep/alive", "still here", false); code != 0 {
				t.Fatalf("survivor puback 0x%x", code)
			}

			if got := string(watcher.message("keep/alive").Payload); got != "still here" {
				t.Fatalf("survivor message = %s", got)
			}
		})
	}
}

func TestIoTCACertificateInactiveDisconnectsClients(t *testing.T) {
	cfg := testConfig()
	cfg.IoT.MQTTSPort = 8883
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	iotCli := iotClient(s, "test")
	data := dataClient(s, "test")

	ca := newIoTCA(t)
	regCode, err := iotCli.GetRegistrationCode(ctx, &iot.GetRegistrationCodeInput{})
	if err != nil {
		t.Fatal(err)
	}

	registeredCA, err := iotCli.RegisterCACertificate(ctx, &iot.RegisterCACertificateInput{
		CaCertificate:           &ca.pem,
		VerificationCertificate: aws.String(ca.sign(t, aws.ToString(regCode.RegistrationCode))),
		SetAsActive:             true,
	})
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "ca-device"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = iotCli.RegisterCertificate(ctx, &iot.RegisterCertificateInput{
		CertificatePem: aws.String(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))),
		Status:         iottypes.CertificateStatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	other, err := iotCli.CreateKeysAndCertificate(ctx, &iot.CreateKeysAndCertificateInput{SetAsActive: true})
	if err != nil {
		t.Fatal(err)
	}

	otherCert, err := tls.X509KeyPair(
		[]byte(aws.ToString(other.CertificatePem)), []byte(aws.ToString(other.KeyPair.PrivateKey)))
	if err != nil {
		t.Fatal(err)
	}

	watcher := connected(t, mqttURL(s), "watcher")
	watcher.subscribe("$aws/events/presence/disconnected/+", "keep/alive")
	doomed := deviceConnection(t, s, &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, "ca-device")
	survivor := deviceConnection(t, s, &otherCert, "survivor")

	if _, err = iotCli.UpdateCACertificate(ctx, &iot.UpdateCACertificateInput{
		CertificateId: registeredCA.CertificateId, NewStatus: iottypes.CACertificateStatusInactive,
	}); err != nil {
		t.Fatal(err)
	}

	doomed.expectDropped()
	expectDisconnectReason(t, watcher, data, "ca-device", "AUTH_ERROR")
	if code := survivor.publish("keep/alive", "still here", false); code != 0 {
		t.Fatalf("survivor puback 0x%x", code)
	}
}
