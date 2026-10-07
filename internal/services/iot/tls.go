package iot

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/pki"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// handshakeTimeout bounds a TLS handshake, so a stalled client does not hold
// its connection open.
const handshakeTimeout = 10 * time.Second

// endpointName matches the data endpoint names DescribeEndpoint reports,
// capturing the account's prefix and the Region.
var endpointName = regexp.MustCompile(
	`^([0-9a-z]+)(?:-ats)?\.(?:credentials\.)?iot\.([a-z]{2}(?:-[a-z]+)+-\d)\.localhost$`)

// ListenMQTTS serves MQTT over TLS from l, until l closes or Stop runs. A
// client's active device certificate decides its account and Region and is
// the principal whose policies apply; a client without one needs a custom
// authorizer. A connection that starts with an HTTP request is the
// credentials provider's instead.
func (s *Service) ListenMQTTS(l net.Listener) {
	s.accept(l, func(ctx context.Context, conn net.Conn) {
		c, err := s.handshake(ctx, conn)
		if err != nil {
			s.logger.Debug("mqtts handshake failed", "error", err)
			_ = conn.Close()
			return
		}

		r := bufio.NewReader(c.Conn)
		_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
		first, err := r.Peek(1)
		_ = conn.SetReadDeadline(time.Time{})
		switch {
		case err != nil:
			_ = c.Close()
			return
		case isHTTP(first):
			s.serveCredentials(ctx, c, r)
			return
		}

		c.Conn = &bufferedConn{Conn: c.Conn, r: r}
		if err := s.serve(c, listenerTLS); err != nil {
			s.logger.Debug("mqtts connection ended", "error", err)
		}
	})
}

// handshake completes a TLS handshake that requires a client certificate,
// refusing certificates that are not registered and active.
func (s *Service) handshake(ctx context.Context, conn net.Conn) (*connection, error) {
	c := &connection{}
	config := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.serverCertificate,
		// The certificate need not chain to a trusted CA: it must be one IoT
		// registered, and the handshake proves its key. A client without one
		// needs a custom authorizer.
		ClientAuth: tls.RequestClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			c.serverName = cs.ServerName
			if len(cs.PeerCertificates) == 0 {
				c.anonymous, c.caller = true, anonymousCaller
				return s.store.View(ctx, func(tx storage.ReadTx) error {
					scope, found, err := endpointScope(tx, cs.ServerName)
					c.scope = scope
					if !found {
						c.scope = s.defaults
					}

					return err
				})
			}

			cert := cs.PeerCertificates[0]
			id := certificateID(cert)
			scope, err := s.certificateScope(ctx, id, cs.ServerName)
			if err != nil {
				return err
			}

			c.cert, c.scope, c.caller, c.principal = cert, scope, id, arn(scope, "cert", id)
			return nil
		},
	}

	tlsConn := tls.Server(conn, config)
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return nil, err
	}

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, err
	}

	c.Conn = tlsConn
	return c, conn.SetDeadline(time.Time{})
}

// certificateScope returns the account and Region of the active certificate
// id. A server name that is an account's data endpoint picks that account
// and Region; otherwise the partition-wide index says which account holds the
// certificate active.
func (s *Service) certificateScope(ctx context.Context, id, serverName string) (request.Scope, error) {
	var scope request.Scope
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var err error
		if scope, found, err = endpointScope(tx, serverName); err != nil {
			return err
		}

		if !found {
			indexed, ok, readErr := storage.Read[request.Scope](tx, certificateIndexKey(id))
			if readErr != nil {
				return readErr
			}

			if !ok {
				return errors.New("the certificate is not registered or not active")
			}

			scope = *indexed
		}

		c, err := loadCertificate(tx, scope, id)
		if err != nil {
			return err
		}

		if c.Status != statusActive {
			return errors.New("the certificate is not active")
		}

		if c.CA == "" {
			return nil
		}

		ca, err := loadCA(tx, scope, c.CA)
		if err != nil {
			return err
		}

		if ca.Status != statusActive {
			return errors.New("the certificate's CA certificate is not active")
		}

		return nil
	})

	return scope, err
}

// endpointScope returns the account and Region of a server name that is an
// account's data endpoint.
func endpointScope(tx storage.ReadTx, serverName string) (scope request.Scope, found bool, err error) {
	m := endpointName.FindStringSubmatch(serverName)
	if m == nil {
		return request.Scope{}, false, nil
	}

	account, ok, err := storage.Read[string](tx, prefixIndex+m[1])
	if err != nil || !ok {
		return request.Scope{}, false, err
	}

	return request.Scope{AccountID: *account, Region: m[2]}, true, nil
}

// serverCertificate returns the listener's certificate for a client's server
// name, signed by the IoT certificate authority: one per Region for the data
// and credentials endpoints (*.iot.<region>.localhost and
// *.credentials.iot.<region>.localhost), and one for localhost and the
// loopback addresses otherwise.
func (s *Service) serverCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := "localhost"
	if m := endpointName.FindStringSubmatch(hello.ServerName); m != nil {
		name = "*.iot." + m[2] + ".localhost"
	}

	s.serverCertsMu.Lock()
	defer s.serverCertsMu.Unlock()
	if cert, ok := s.serverCerts[name]; ok {
		return cert, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	now := s.now()
	ca, err := s.ca.Get(hello.Context(), now)
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: pki.SerialNumber(),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if name == "localhost" {
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	} else {
		tmpl.DNSNames = append(tmpl.DNSNames, strings.Replace(name, "*.iot.", "*.credentials.iot.", 1))
	}

	cert, _, err := ca.Issue(tmpl, key.Public())
	if err != nil {
		return nil, err
	}

	s.serverCerts[name] = &tls.Certificate{Certificate: [][]byte{cert.Raw, ca.Certificate.Raw}, PrivateKey: key}
	return s.serverCerts[name], nil
}

// RootCA serves the PEM certificate of the IoT certificate authority, which
// signs the MQTT over TLS listener's certificates (Amazon Root CA 1's role).
func (s *Service) RootCA() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		ca, err := s.ca.Get(r.Context(), s.now())
		if err != nil {
			s.logger.Error("iot root ca", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write([]byte(ca.PEM))
	})
}
