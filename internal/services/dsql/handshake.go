package dsql

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
)

const (
	sslRequest      = 80877103
	gssRequest      = 80877104
	protocolVersion = 196608
	// TinyStack bounds pre-authentication packets to 1 MiB to bound unauthenticated memory use.
	maxStartupPacket = 1 << 20
)

func startup(ctx context.Context, conn net.Conn, config *tls.Config) (net.Conn, map[string]string, error) {
	encrypted := false
	refusedGSS := false
	for {
		packet, err := startupPacket(conn)
		if err != nil {
			return conn, nil, err
		}

		code := binary.BigEndian.Uint32(packet)
		switch code {
		case gssRequest:
			if len(packet) != 4 || encrypted || refusedGSS {
				return conn, nil, errors.New("invalid GSS negotiation")
			}

			refusedGSS = true
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return conn, nil, err
			}
		case sslRequest:
			if len(packet) != 4 || encrypted || config == nil {
				return conn, nil, errors.New("invalid TLS negotiation")
			}

			if _, err := conn.Write([]byte{'S'}); err != nil {
				return conn, nil, err
			}

			secure := tls.Server(conn, config)
			if err := secure.HandshakeContext(ctx); err != nil {
				return conn, nil, err
			}

			conn = secure
			encrypted = true
		case protocolVersion:
			if !encrypted {
				return conn, nil, errors.New("TLS is required")
			}

			raw := string(packet[4:])
			if !strings.HasSuffix(raw, "\x00\x00") {
				return conn, nil, errors.New("invalid startup parameters")
			}

			fields := strings.Split(strings.TrimSuffix(raw, "\x00\x00"), "\x00")
			if len(fields)%2 != 0 {
				return conn, nil, errors.New("invalid startup parameters")
			}

			params := map[string]string{}
			for i := 0; i < len(fields); i += 2 {
				if fields[i] == "" {
					return conn, nil, errors.New("empty startup parameter")
				}

				if _, exists := params[fields[i]]; exists {
					return conn, nil, errors.New("duplicate startup parameter")
				}

				params[fields[i]] = fields[i+1]
			}

			return conn, params, nil
		default:
			return conn, nil, errors.New("unsupported PostgreSQL protocol")
		}
	}
}

func startupPacket(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}

	size := binary.BigEndian.Uint32(header[:])
	if size < 8 || size > maxStartupPacket {
		return nil, errors.New("invalid startup packet length")
	}

	packet := make([]byte, size-4)
	_, err := io.ReadFull(r, packet)
	return packet, err
}

func password(conn net.Conn) (string, error) {
	if _, err := conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 3}); err != nil {
		return "", err
	}

	var kind [1]byte
	if _, err := io.ReadFull(conn, kind[:]); err != nil {
		return "", err
	}

	if kind[0] != 'p' {
		return "", errors.New("expected password message")
	}

	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return "", err
	}

	size := binary.BigEndian.Uint32(header[:])
	if size < 5 || size > maxStartupPacket {
		return "", errors.New("invalid password packet length")
	}

	packet := make([]byte, size-4)
	if _, err := io.ReadFull(conn, packet); err != nil {
		return "", err
	}

	if packet[len(packet)-1] != 0 || strings.ContainsRune(string(packet[:len(packet)-1]), 0) {
		return "", errors.New("invalid password message")
	}

	return string(packet[:len(packet)-1]), nil
}

func authenticationError(w io.Writer, code, message string) error {
	fields := []byte("SFATAL\x00VFATAL\x00C" + code + "\x00M" + message + "\x00\x00")
	packet := make([]byte, 5, 5+len(fields))
	packet[0] = 'E'
	// #nosec G115 -- fields is a bounded, server-generated error message.
	binary.BigEndian.PutUint32(packet[1:], uint32(len(fields)+4))
	_, err := w.Write(append(packet, fields...))
	return err
}
