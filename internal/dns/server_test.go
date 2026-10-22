package dns

import (
	"context"
	"errors"
	"net"
	"testing"

	wire "github.com/miekg/dns"
)

func TestListener(t *testing.T) {
	upstream, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	fake := &wire.Server{
		PacketConn: upstream, NotifyStartedFunc: func() { close(ready) },
		Handler: wire.HandlerFunc(func(w wire.ResponseWriter, q *wire.Msg) {
			reply := new(wire.Msg)
			reply.SetReply(q)
			reply.Rcode = wire.RcodeNameError
			if q.Question[0].Name == "failure.example." {
				reply.Rcode = wire.RcodeServerFailure
			}

			reply.Extra = []wire.RR{&wire.TXT{Hdr: wire.RR_Header{
				Name: "marker.example.", Rrtype: wire.TypeTXT,
				Class: wire.ClassINET,
			}, Txt: []string{"unchanged"}}}
			_ = w.WriteMsg(reply)
		}),
	}

	go func() { _ = fake.ActivateAndServe() }()
	<-ready
	t.Cleanup(func() { _ = fake.Shutdown() })
	s, err := Start(context.Background(), "127.0.0.1:0", "127.0.0.2", []string{upstream.LocalAddr().String()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	for _, transport := range []string{"udp", "tcp"} {
		for _, name := range []string{
			"s3.amazonaws.com.", "bucket.s3.us-east-1.amazonaws.com.",
			"ec2.cn-north-1.amazonaws.com.cn.", "s3.us-east-1.api.aws.", "a.localhost.localstack.cloud.",
			"a.lambda-url.us-east-1.on.aws.",
		} {
			q := new(wire.Msg)
			q.SetQuestion(name, wire.TypeA)
			reply, _, queryErr := (&wire.Client{Net: transport}).Exchange(q, s.Address())
			if queryErr != nil {
				t.Fatal(queryErr)
			}

			if len(reply.Answer) != 1 || reply.Answer[0].(*wire.A).A.String() != "127.0.0.2" {
				t.Fatalf("%s %s: %v", transport, name, reply)
			}
		}
	}

	for name, code := range map[string]int{
		"outside.example.": wire.RcodeNameError,
		"failure.example.": wire.RcodeServerFailure, "notamazonaws.com.": wire.RcodeNameError,
	} {
		q := new(wire.Msg)
		q.SetQuestion(name, wire.TypeA)
		reply, _, queryErr := (&wire.Client{}).Exchange(q, s.Address())
		if queryErr != nil {
			t.Fatal(queryErr)
		}

		if reply.Rcode != code || len(reply.Extra) != 1 {
			t.Fatalf("forwarded response: %v", reply)
		}
	}
}

func TestIPv6AndValidation(t *testing.T) {
	s, err := Start(context.Background(), "127.0.0.1:0", "::1", []string{"127.0.0.1:1"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	for kind, count := range map[uint16]int{wire.TypeAAAA: 1, wire.TypeA: 0} {
		q := new(wire.Msg)
		q.SetQuestion("s3.amazonaws.com.", kind)
		reply, _, queryErr := (&wire.Client{}).Exchange(q, s.Address())
		if queryErr != nil {
			t.Fatal(queryErr)
		}

		if len(reply.Answer) != count {
			t.Fatalf("wrong address family: %v", reply)
		}
	}

	if _, err = Start(context.Background(), "127.0.0.1:0", "localhost", []string{"127.0.0.1:1"}, nil); err == nil {
		t.Fatal("accepted hostname")
	}
}

func TestLocalLookup(t *testing.T) {
	lookup := func(_ context.Context, name string, _ uint16) ([]wire.RR, bool, error) {
		switch name {
		case "missing.example.":
			return nil, true, ErrNameNotFound
		case "broken.example.":
			return nil, true, errors.New("unsupported record")
		default:
			return []wire.RR{&wire.A{Hdr: wire.RR_Header{
				Name: name, Rrtype: wire.TypeA,
				Class: wire.ClassINET, Ttl: 42,
			}, A: net.ParseIP("192.0.2.1")}}, true, nil
		}
	}
	s, err := Start(context.Background(), "127.0.0.1:0", "127.0.0.1", []string{"127.0.0.1:1"}, lookup)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	for name, code := range map[string]int{
		"record.example.":  wire.RcodeSuccess,
		"missing.example.": wire.RcodeNameError, "broken.example.": wire.RcodeServerFailure,
	} {
		q := new(wire.Msg)
		q.SetQuestion(name, wire.TypeA)
		reply, _, queryErr := (&wire.Client{}).Exchange(q, s.Address())
		if queryErr != nil {
			t.Fatal(queryErr)
		}

		if reply.Rcode != code || !reply.Authoritative {
			t.Fatalf("local reply: %v", reply)
		}

		if name == "record.example." && (len(reply.Answer) != 1 || reply.Answer[0].Header().Ttl != 42) {
			t.Fatalf("lost local record: %v", reply)
		}
	}
}
