// Package dns provides TinyStack's optional split DNS listener.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	wire "github.com/miekg/dns"
)

// ErrNameNotFound signals that a local zone has no such name.
var ErrNameNotFound = errors.New("DNS name does not exist")

// Lookup returns records and whether a local hosted zone owns the name.
type Lookup func(context.Context, string, uint16) ([]wire.RR, bool, error)

// Server owns UDP and TCP listeners on the same address.
type Server struct {
	udp, tcp  *wire.Server
	lookup    Lookup
	ip        net.IP
	upstreams []string
}

// Start binds both transports before returning. An ephemeral address is supported.
func Start(ctx context.Context, address, host string, upstreams []string, lookup Lookup) (*Server, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("DNS requires a literal server.host IP address")
	}

	if ip.IsUnspecified() {
		if ip.To4() != nil {
			ip = net.ParseIP("127.0.0.1")
		} else {
			ip = net.ParseIP("::1")
		}
	}

	if len(upstreams) == 0 {
		cfg, err := wire.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			return nil, err
		}

		for _, server := range cfg.Servers {
			upstreams = append(upstreams, net.JoinHostPort(server, cfg.Port))
		}
	}

	if len(upstreams) == 0 {
		return nil, fmt.Errorf("DNS needs at least one upstream resolver")
	}

	for _, upstream := range upstreams {
		if _, _, err := net.SplitHostPort(upstream); err != nil {
			return nil, fmt.Errorf("DNS upstream must be host:port: %w", err)
		}
	}

	var lc net.ListenConfig
	tcp, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	udp, err := lc.ListenPacket(ctx, "udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}

	s := &Server{ip: ip, upstreams: upstreams, lookup: lookup}
	tcpReady, udpReady := make(chan struct{}), make(chan struct{})
	s.tcp = &wire.Server{Listener: tcp, Handler: s, NotifyStartedFunc: func() { close(tcpReady) }}
	s.udp = &wire.Server{PacketConn: udp, Handler: s, NotifyStartedFunc: func() { close(udpReady) }}
	go func() { _ = s.tcp.ActivateAndServe() }()
	go func() { _ = s.udp.ActivateAndServe() }()
	<-tcpReady
	<-udpReady
	return s, nil
}

// Address returns the bound address, including the selected ephemeral port.
func (s *Server) Address() string { return s.tcp.Listener.Addr().String() }

// Stop closes both transports and waits for active queries.
func (s *Server) Stop() {
	_ = s.udp.Shutdown()
	_ = s.tcp.Shutdown()
}

func intercepted(name string) bool {
	for _, suffix := range []string{"amazonaws.com.", "amazonaws.com.cn.", "api.aws.", "localhost.localstack.cloud."} {
		if strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}

	if strings.HasSuffix(name, ".localhost.") {
		for _, marker := range []string{".elb.", ".execute-api.", ".appsync-api.", ".appsync-realtime-api.", ".lambda-url."} {
			if strings.Contains(name, marker) {
				return true
			}
		}
	}

	return strings.Contains(name, ".lambda-url.") && strings.HasSuffix(name, ".on.aws.")
}

// ServeDNS answers local names and forwards all other messages without rewriting them.
func (s *Server) ServeDNS(w wire.ResponseWriter, query *wire.Msg) {
	response := new(wire.Msg)
	response.SetReply(query)
	response.RecursionAvailable = true
	if query.Opcode != wire.OpcodeQuery || len(query.Question) != 1 {
		response.Rcode = wire.RcodeNotImplemented
		_ = w.WriteMsg(response)
		return
	}

	q := query.Question[0]
	name := strings.ToLower(q.Name)
	if s.lookup != nil && q.Qclass == wire.ClassINET {
		records, owned, err := s.lookup(context.Background(), name, q.Qtype)
		if err != nil {
			response.Authoritative = owned
			response.Rcode = wire.RcodeServerFailure
			if errors.Is(err, ErrNameNotFound) {
				response.Rcode = wire.RcodeNameError
			}

			_ = w.WriteMsg(response)
			return
		}

		if owned {
			response.Authoritative = true
			response.Answer = records
			_ = w.WriteMsg(response)
			return
		}
	}

	if intercepted(name) && q.Qclass == wire.ClassINET && (q.Qtype == wire.TypeA || q.Qtype == wire.TypeAAAA) {
		response.Authoritative = true
		header := wire.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: wire.ClassINET, Ttl: 60}
		if q.Qtype == wire.TypeA && s.ip.To4() != nil {
			response.Answer = []wire.RR{&wire.A{Hdr: header, A: s.ip.To4()}}
		}

		if q.Qtype == wire.TypeAAAA && s.ip.To4() == nil {
			response.Answer = []wire.RR{&wire.AAAA{Hdr: header, AAAA: s.ip}}
		}

		_ = w.WriteMsg(response)
		return
	}

	transport := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		transport = "tcp"
	}

	client := &wire.Client{Net: transport, Timeout: 2 * time.Second}
	for _, upstream := range s.upstreams {
		reply, _, err := client.Exchange(query, upstream)
		if err == nil {
			_ = w.WriteMsg(reply)
			return
		}
	}

	response.Rcode = wire.RcodeServerFailure
	_ = w.WriteMsg(response)
}
