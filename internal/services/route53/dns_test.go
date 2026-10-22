package route53

import (
	"context"
	"errors"
	"testing"

	"github.com/miekg/dns"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	resolver "github.com/amjadjibon/tinystack/internal/dns"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestLookupDNS(t *testing.T) {
	store := &memory.Store{}
	ctx := context.Background()
	err := store.Update(ctx, func(tx storage.WriteTx) error {
		for id, name := range map[string]string{"parent": "example.test.", "child": "sub.example.test."} {
			z := zone{Zone: api.HostedZone{Id: new(id), Name: new(name)}}
			if writeErr := storage.Write(tx, key("account", kindZone, id), z); writeErr != nil {
				return writeErr
			}

			rs := api.ResourceRecordSet{
				Name: new("www.sub.example.test."), Type: new("A"), TTL: new(int64(42)),
				ResourceRecords: []api.ResourceRecord{{Value: new("192.0.2.1")}},
			}
			if id == "child" {
				rs.ResourceRecords[0].Value = new("192.0.2.2")
			}

			if writeErr := storage.Write(tx, recordKey("account", id, &rs), rs); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	service := New(store)
	answer, owned, err := service.LookupDNS(ctx, "account", "www.sub.example.test.", dns.TypeA)
	if err != nil || !owned || len(answer) != 1 {
		t.Fatalf("lookup: %v %v %v", answer, owned, err)
	}

	if a := answer[0].(*dns.A); a.A.String() != "192.0.2.2" || a.Hdr.Ttl != 42 {
		t.Fatal(a)
	}

	answer, owned, err = service.LookupDNS(ctx, "other", "www.sub.example.test.", dns.TypeA)
	if err != nil || owned || len(answer) != 0 {
		t.Fatalf("account leak: %v %v %v", answer, owned, err)
	}
}

func TestMissingDNSName(t *testing.T) {
	store := &memory.Store{}
	ctx := context.Background()
	err := store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key("account", kindZone, "zone"), zone{
			Zone: api.HostedZone{Id: new("zone"), Name: new("example.test.")},
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	_, owned, err := New(store).LookupDNS(ctx, "account", "missing.example.test.", dns.TypeA)
	if !owned || !errors.Is(err, resolver.ErrNameNotFound) {
		t.Fatalf("missing: %v %v", owned, err)
	}
}
