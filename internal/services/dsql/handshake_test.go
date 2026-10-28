package dsql

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/localtls"
)

func TestStartupRequiresTLS(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()
	if err := server.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := startup(context.Background(), server, nil)
		done <- err
	}()
	packet := make([]byte, 8)
	binary.BigEndian.PutUint32(packet, 8)
	binary.BigEndian.PutUint32(packet[4:], gssRequest)
	if _, err := client.Write(packet); err != nil {
		t.Fatal(err)
	}

	var answer [1]byte
	if _, err := io.ReadFull(client, answer[:]); err != nil || answer[0] != 'N' {
		t.Fatal(answer, err)
	}

	binary.BigEndian.PutUint32(packet[4:], protocolVersion)
	if _, err := client.Write(packet); err != nil {
		t.Fatal(err)
	}

	if err := <-done; err == nil || err.Error() != "TLS is required" {
		t.Fatal(err)
	}
}

func TestStartupPacketBounds(t *testing.T) {
	for _, length := range []uint32{0, 4, 7, maxStartupPacket + 1} {
		packet := make([]byte, 4)
		binary.BigEndian.PutUint32(packet, length)
		if _, err := startupPacket(bytes.NewReader(packet)); err == nil {
			t.Errorf("accepted length %d", length)
		}
	}
}

func TestAuthenticationError(t *testing.T) {
	var wire bytes.Buffer
	if err := authenticationError(&wire, "28P01", "invalid token"); err != nil {
		t.Fatal(err)
	}

	packet := wire.Bytes()
	if packet[0] != 'E' || int(binary.BigEndian.Uint32(packet[1:5])) != len(packet)-1 ||
		!bytes.Contains(packet, []byte("C28P01\x00")) || packet[len(packet)-1] != 0 {
		t.Fatal(packet)
	}
}

func TestStartupTLS(t *testing.T) {
	ca, err := localtls.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const endpoint = "test.dsql.us-east-1.on.aws"
	config, err := ca.Config([]string{endpoint})
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.PEM) {
		t.Fatal("invalid local CA")
	}

	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, params, err := startup(ctx, server, config)
		if err == nil && (params["user"] != "admin" || params["database"] != "postgres") {
			err = errors.New("incorrect startup parameters")
		}

		result <- err
	}()
	packet := make([]byte, 8)
	binary.BigEndian.PutUint32(packet, 8)
	binary.BigEndian.PutUint32(packet[4:], sslRequest)
	if _, err := client.Write(packet); err != nil {
		t.Fatal(err)
	}

	var answer [1]byte
	if _, err := io.ReadFull(client, answer[:]); err != nil || answer[0] != 'S' {
		t.Fatal(answer, err)
	}

	secure := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: endpoint})
	if err := secure.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}

	fields := []byte("user\x00admin\x00database\x00postgres\x00\x00")
	packet = make([]byte, 8)
	binary.BigEndian.PutUint32(packet, uint32(8+len(fields)))
	binary.BigEndian.PutUint32(packet[4:], protocolVersion)
	if _, err := secure.Write(append(packet, fields...)); err != nil {
		t.Fatal(err)
	}

	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
