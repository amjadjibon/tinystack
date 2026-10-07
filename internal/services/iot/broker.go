package iot

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const (
	// maxPayload is the largest message payload IoT Core accepts.
	maxPayload = 128 << 10
	// maxClientID and maxTopic bound client IDs and topic names in bytes.
	maxClientID = 128
	maxTopic    = 256
	// maxSlashes is how many slashes a topic may have, besides a reserved
	// topic's prefix.
	maxSlashes = 7
	// maxSessionExpiry is how long a persistent session outlives its
	// connection, IoT Core's default of one hour.
	maxSessionExpiry  = 3600
	listenerWebSocket = "websocket"
	listenerTCP       = "tcp"
	listenerTLS       = "tls"
	// anonymousCaller identifies connections without an identity in
	// lifecycle events.
	anonymousCaller = "anonymous"
)

// errStopped is returned for connections that arrive once Stop has run.
var errStopped = errors.New("the IoT broker is stopped")

// broker is the MQTT message broker of one account and Region, so topics
// never cross them.
type broker struct {
	server *mqtt.Server
	// data publishes the data plane's messages with their MQTT 5 properties.
	data *mqtt.Client
	// connections are guarded by mu.
	connections map[string]*connectionInfo
	scope       request.Scope
	mu          sync.Mutex
	// closing stops lifecycle events once the broker is shutting down.
	closing atomic.Bool
}

// connectionInfo is what GetConnection reports about a client ID beyond the
// broker's own session.
type connectionInfo struct {
	connected    time.Time
	disconnected time.Time
	reason       string
	thing        string
	// version counts the client ID's connections, as lifecycle events'
	// versionNumber does.
	version int64
}

// connection is an MQTT network connection with the identity it
// authenticated as, which the hook reads.
type connection struct {
	net.Conn
	// subscribing records the filters of the SUBSCRIBE being processed and
	// whether each was authorized, so the hook can tell its ACL checks from
	// deliveries' (mochi checks both with write false). mu guards it.
	subscribing map[string]bool
	// cert is the client certificate presented over mutual TLS, if any, and
	// certID its IoT certificate ID.
	cert   *x509.Certificate
	certID string
	// header and query are an unsigned WebSocket request's, which can carry
	// a custom authorizer's name and token.
	header http.Header
	query  url.Values
	// expiry disconnects a custom-authorized connection when the
	// authorizer's disconnectAfterInSeconds passes.
	expiry *time.Timer
	scope  request.Scope
	// principal is the IoT principal whose policies apply: a certificate
	// ARN, a Cognito identity ID, or "" for a caller whose policies are IAM's
	// alone.
	principal string
	// caller identifies the connection in lifecycle events.
	caller string
	// session is the ID lifecycle events give this connection.
	session string
	// thing is the thing the connection is, when its client ID names a
	// thing attached to its principal.
	thing string
	// serverName is the TLS server name the client asked for.
	serverName string
	// custom are the policies a custom authorizer returned for the
	// connection, which govern it alone; nil if no authorizer let it in.
	custom []*policyDocument
	mu     sync.Mutex
	// anonymous is a connection that presented no identity (an unsigned
	// WebSocket, plain MQTT, or TLS without a certificate), which a custom
	// authorizer can let in.
	anonymous bool
}

// brokers returns the scope's broker, starting it on first use.
func (s *Service) broker(scope request.Scope) (*broker, error) {
	s.brokersMu.Lock()
	defer s.brokersMu.Unlock()
	if s.brokers == nil {
		return nil, errStopped
	}

	if b, ok := s.brokers[scope]; ok {
		return b, nil
	}

	capabilities := mqtt.NewDefaultServerCapabilities()
	capabilities.MaximumQos = 1
	capabilities.MaximumPacketSize = maxPayload + 1024
	capabilities.MaximumSessionExpiryInterval = maxSessionExpiry
	capabilities.TopicAliasMaximum = 8
	capabilities.ReceiveMaximum = 100
	server := mqtt.New(&mqtt.Options{
		InlineClient: true, Capabilities: capabilities, Logger: slog.New(quiet{s.logger.Handler()}),
		// AWS has no $SYS topics; the hook refuses subscriptions to them.
		SysTopicResendInterval: 1 << 30,
	})
	b := &broker{server: server, scope: scope, connections: map[string]*connectionInfo{}}
	if err := server.AddHook(&hook{service: s, broker: b}, nil); err != nil {
		return nil, err
	}

	if err := server.Serve(); err != nil {
		return nil, err
	}

	b.data = server.NewClient(nil, mqtt.LocalListener, "", true)
	b.data.Properties.ProtocolVersion = 5
	s.brokers[scope] = b
	return b, nil
}

// serve runs an MQTT connection in the scope's broker until it closes.
func (s *Service) serve(c *connection, listener string) error {
	b, err := s.broker(c.scope)
	if err != nil {
		_ = c.Close()
		return err
	}

	c.session = uuid.NewV4().String()
	err = b.server.EstablishConnection(listener, c)
	if errors.Is(err, packets.ErrBadUsernameOrPassword) || errors.Is(err, net.ErrClosed) {
		// A refused or closed connection is the client's business.
		return nil
	}

	return err
}

// ServeMQTT serves an MQTT connection that arrived over WebSocket until it
// closes, in the account and Region of the caller that signed it, or the
// default ones for an unsigned connection, which only a custom authorizer
// lets in under identity.mode verify.
func (s *Service) ServeMQTT(_ context.Context, r *execute.Request, conn net.Conn) error {
	c := &connection{Conn: conn, scope: s.defaults, caller: anonymousCaller, anonymous: r.Caller == nil}
	if c.anonymous {
		c.header, c.query = r.Header, r.Query
	}

	if r.Caller != nil {
		c.scope = request.Scope{AccountID: r.Caller.AccountID, Region: r.Caller.Region}
		c.principal, c.caller = r.Caller.CognitoIdentity, r.Caller.AccessKey
		if c.principal != "" {
			c.caller = c.principal
		}
	}

	return s.serve(c, listenerWebSocket)
}

// ListenMQTT serves plain MQTT connections from l, without authentication,
// in the default account and Region, until l closes or Stop runs.
func (s *Service) ListenMQTT(l net.Listener) {
	s.accept(l, func(_ context.Context, conn net.Conn) {
		c := &connection{Conn: conn, scope: s.defaults, caller: anonymousCaller, anonymous: true}
		if err := s.serve(c, listenerTCP); err != nil {
			s.logger.Debug("mqtt connection ended", "error", err)
		}
	})
}

// accept hands each connection l accepts to serve, in the background, until
// l closes or Stop runs.
func (s *Service) accept(l net.Listener, serve func(context.Context, net.Conn)) {
	s.bg.Go(func(ctx context.Context) {
		go func() {
			<-ctx.Done()
			_ = l.Close()
		}()

		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}

			if !s.bg.Go(func(ctx context.Context) { serve(ctx, conn) }) {
				_ = conn.Close()
			}
		}
	})
}

// closeBrokers stops every broker, disconnecting its clients.
func (s *Service) closeBrokers() {
	s.brokersMu.Lock()
	brokers := s.brokers
	s.brokers = nil
	s.brokersMu.Unlock()
	for _, b := range brokers {
		b.closing.Store(true)
		for _, cl := range b.server.Clients.GetAll() {
			if !cl.Net.Inline {
				_ = b.server.DisconnectClient(cl, packets.ErrServerShuttingDown)
			}
		}

		_ = b.server.Close()
	}
}

// reasonAuthError is the disconnect reason IoT Core reports for a client that
// authentication or authorization no longer admits.
const reasonAuthError = "AUTH_ERROR"

// disconnect records reason for a client's disconnection and disconnects it.
func (b *broker) disconnect(cl *mqtt.Client, reason string, cause packets.Code) {
	b.mu.Lock()
	if info := b.connections[cl.ID]; info != nil {
		info.reason = reason
	}
	b.mu.Unlock()
	if !cl.Closed() {
		_ = b.server.DisconnectClient(cl, cause)
	}
}

// disconnectCertificates disconnects the scope's live clients that
// authenticated with one of the device certificates ids, reporting
// AUTH_ERROR. It runs after the change that stopped the certificates being
// usable has committed.
func (s *Service) disconnectCertificates(scope request.Scope, ids ...string) {
	b := s.existing(scope)
	if b == nil || len(ids) == 0 {
		return
	}

	for _, cl := range b.server.Clients.GetAll() {
		if cl.Net.Inline {
			continue
		}

		if c := connectionOf(cl); c != nil && c.certID != "" && slices.Contains(ids, c.certID) {
			b.disconnect(cl, reasonAuthError, packets.ErrNotAuthorized)
		}
	}
}

// existing returns the scope's broker if it has been started.
func (s *Service) existing(scope request.Scope) *broker {
	s.brokersMu.Lock()
	defer s.brokersMu.Unlock()
	return s.brokers[scope]
}

// quiet passes on only warnings and errors, since the broker logs every
// start and connection at the info level.
type quiet struct{ slog.Handler }

func (q quiet) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn && q.Handler.Enabled(ctx, level)
}

func (q quiet) WithAttrs(attrs []slog.Attr) slog.Handler { return quiet{q.Handler.WithAttrs(attrs)} }

func (q quiet) WithGroup(name string) slog.Handler { return quiet{q.Handler.WithGroup(name)} }
