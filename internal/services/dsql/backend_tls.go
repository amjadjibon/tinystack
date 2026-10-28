package dsql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
)

const bootstrapRole = "tinystack_owner"

func (s *Service) secureBackend(ctx context.Context, b *backend) error {
	if s.ca == nil {
		return errors.New("DSQL certificate authority is not configured")
	}

	config, err := s.ca.Config(nil)
	if err != nil {
		return err
	}

	cert, err := config.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		return err
	}

	private, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return err
	}

	_, markerErr := dockercli.Run(ctx, nil, "exec", b.Name, "test", "-f", "/var/lib/postgresql/data/proxy-tls-ready")
	if markerErr != nil {
		files := map[string][]byte{
			"proxy.key":    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}),
			"proxy.crt":    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
			"proxy-ca.crt": s.ca.PEM,
			"pg_hba.conf":  []byte("local all all trust\nhostssl all all all cert\nhost all all all reject\n"),
		}

		for name, data := range files {
			if _, err = dockercli.Run(ctx, strings.NewReader(string(data)), "exec", "-i", "--user", "postgres", b.Name,
				"sh", "-c", "umask 077; cat > /var/lib/postgresql/data/"+name); err != nil {
				return err
			}
		}

		if _, err = dockercli.Run(ctx, nil, "exec", "--user", "postgres", b.Name,
			"touch", "/var/lib/postgresql/data/proxy-tls-ready"); err != nil {
			return err
		}
	}

	sql := `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'admin') THEN
 CREATE ROLE admin WITH LOGIN CREATEROLE CREATEDB; END IF; END $$;
 GRANT ALL ON DATABASE postgres TO admin; GRANT ALL ON SCHEMA public TO admin;
 ALTER SYSTEM SET ssl = 'on'; ALTER SYSTEM SET ssl_cert_file = 'proxy.crt';
 ALTER SYSTEM SET ssl_key_file = 'proxy.key'; ALTER SYSTEM SET ssl_ca_file = 'proxy-ca.crt';
 SELECT pg_reload_conf();`
	_, err = dockercli.Run(ctx, strings.NewReader(sql), "exec", "-i", b.Name, "psql", "-v", "ON_ERROR_STOP=1",
		"-U", bootstrapRole, "-d", "postgres")
	return err
}

func (s *Service) backendTLS(ctx context.Context, conn net.Conn, user string) (net.Conn, error) {
	if _, err := conn.Write([]byte{0, 0, 0, 8, 4, 210, 22, 47}); err != nil {
		return nil, err
	}

	var response [1]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		return nil, err
	}

	if response[0] != 'S' {
		return nil, errors.New("backend requires TLS")
	}

	if user == bootstrapRole {
		return nil, errors.New("bootstrap role cannot authenticate through DSQL")
	}

	cert, err := s.ca.ClientCertificate(user)
	if err != nil {
		return nil, err
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(s.ca.PEM)
	secure := tls.Client(conn, &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: roots,
		ServerName: "localhost", Certificates: []tls.Certificate{cert},
	})
	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, err
	}

	return secure, nil
}

func (s *Service) probeBackend(ctx context.Context, b *backend) error {
	conn, err := new(net.Dialer).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", b.Port))
	if err != nil {
		return err
	}

	defer func() { _ = conn.Close() }()
	if deadlineErr := conn.SetDeadline(time.Now().Add(15 * time.Second)); deadlineErr != nil {
		return deadlineErr
	}

	secure, err := s.backendTLS(ctx, conn, "admin")
	if err != nil {
		return err
	}

	return backendLogin(secure, map[string]string{"user": "admin", "database": "postgres"})
}
