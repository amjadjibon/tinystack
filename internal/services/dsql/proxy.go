package dsql

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/localtls"
	"github.com/amjadjibon/tinystack/internal/services/background"
)

type proxy struct {
	net.Listener
	conns  map[net.Conn]bool
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

type TokenAuthorizer func(context.Context, request.Scope, string, string, string, string) error

func (s *Service) UseProxy(ca *localtls.Authority, authorize TokenAuthorizer) {
	s.ca, s.authorize = ca, authorize
	s.workers = background.New()
	s.workers.Every(100*time.Millisecond, s.advance)
}

func (s *Service) listen(ctx context.Context, scope request.Scope, r *record) error {
	if s.ca == nil || s.authorize == nil {
		return errors.New("DSQL proxy authentication is not configured")
	}

	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	arn := common.Value(r.Arn)
	if s.proxies[arn] != nil {
		return nil
	}

	config, err := s.ca.Config([]string{common.Value(r.Endpoint)})
	if err != nil {
		return err
	}

	l, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:"+strconv.FormatInt(r.ProxyPort, 10))
	if err != nil {
		return err
	}

	r.ProxyPort = int64(l.Addr().(*net.TCPAddr).Port)
	p := &proxy{Listener: l, conns: map[net.Conn]bool{}}
	s.proxies[arn] = p
	p.wg.Go(func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}

			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				_ = conn.Close()
				return
			}

			p.conns[conn] = true
			p.mu.Unlock()
			p.wg.Go(func() {
				defer func() {
					_ = conn.Close()
					p.mu.Lock()
					delete(p.conns, conn)
					p.mu.Unlock()
				}()
				s.session(ctx, conn, config, scope, r)
			})
		}
	})
	return nil
}

func (s *Service) session(parent context.Context, raw net.Conn, config *tls.Config, scope request.Scope, r *record) {
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	conn, params, err := startup(parent, raw, config)
	if err != nil {
		_ = authenticationError(conn, "28000", err.Error())
		return
	}

	token, err := password(conn)
	if err != nil {
		_ = authenticationError(conn, "28P01", "invalid password message")
		return
	}

	user := params["user"]
	signed, _, err := databaseToken(token, common.Value(r.Endpoint), scope.Region, user, time.Now())
	if err != nil {
		_ = authenticationError(conn, "28P01", "invalid database token")
		return
	}

	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	if err = s.authorize(ctx, scope, common.Value(r.Arn), user, token, signed.URL.Host); err != nil {
		_ = authenticationError(conn, "28000", "database access denied")
		return
	}

	if params["database"] != "" && params["database"] != "postgres" {
		_ = authenticationError(conn, "28000", "only database postgres is supported")
		return
	}

	backend, err := new(net.Dialer).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", r.Backend.Port))
	if err == nil {
		secure, tlsErr := s.backendTLS(ctx, backend, user)
		if tlsErr != nil {
			err = tlsErr
		} else {
			backend = secure
			err = backendLogin(backend, params)
		}
	}

	if err != nil {
		if backend != nil {
			_ = backend.Close()
		}

		_ = authenticationError(conn, "28P01", "backend role authentication failed")
		return
	}

	defer func() { _ = backend.Close() }()
	if _, err := conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}); err != nil {
		return
	}

	_ = conn.SetDeadline(time.Time{})
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(backend, conn)
		_ = backend.Close()
		close(done)
	}()
	_, _ = io.Copy(conn, backend)
	_ = conn.Close()
	<-done
}

func backendLogin(conn net.Conn, params map[string]string) error {
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, protocolVersion)
	for k, v := range params {
		payload = append(payload, []byte(k+"\x00"+v+"\x00")...)
	}

	payload = append(payload, 0)
	packet := make([]byte, 4)
	// #nosec G115 -- payload comes from the bounded startup packet.
	binary.BigEndian.PutUint32(packet, uint32(len(payload)+4))
	if _, err := conn.Write(append(packet, payload...)); err != nil {
		return err
	}

	for {
		var header [5]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return err
		}

		size := binary.BigEndian.Uint32(header[1:])
		if size < 4 || size > maxStartupPacket {
			return errors.New("invalid backend authentication frame")
		}

		body := make([]byte, size-4)
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}

		if header[0] != 'R' || len(body) != 4 {
			return errors.New("backend rejected authentication")
		}

		switch binary.BigEndian.Uint32(body) {
		case 0:
			// The caller forwards AuthenticationOk before relaying server parameters and ReadyForQuery.
			return conn.SetDeadline(time.Time{})
		default:
			return errors.New("unsupported backend authentication")
		}
	}
}

func (s *Service) closeProxies() {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	for arn := range s.proxies {
		s.closeProxyLocked(arn)
	}
}

func (s *Service) closeProxy(arn string) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	s.closeProxyLocked(arn)
}

// closeProxyLocked stops a cluster's listener and its sessions; the caller holds proxyMu.
func (s *Service) closeProxyLocked(arn string) {
	p := s.proxies[arn]
	if p == nil {
		return
	}

	_ = p.Close()
	p.mu.Lock()
	p.closed = true
	for conn := range p.conns {
		_ = conn.Close()
	}

	p.mu.Unlock()
	p.wg.Wait()
	delete(s.proxies, arn)
}
