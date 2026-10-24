package mountaddr

import (
	"errors"
	"net/netip"
	"testing"
)

func TestPick(t *testing.T) {
	t.Parallel()
	block := netip.MustParsePrefix("10.0.0.0/29")
	used := func(addr netip.Addr) bool { return addr.String() == "10.0.0.4" }
	got, err := Pick(block, netip.Addr{}, used)
	if err != nil || got.String() != "10.0.0.5" {
		t.Fatalf("automatic address: %s %v", got, err)
	}

	for _, tc := range []struct {
		want    error
		address string
	}{
		{ErrOutsideSubnet, "10.0.0.1"},
		{ErrOutsideSubnet, "10.0.0.7"},
		{ErrOutsideSubnet, "10.0.1.4"},
		{ErrInUse, "10.0.0.4"},
	} {
		if _, pickErr := Pick(block, netip.MustParseAddr(tc.address), used); !errors.Is(pickErr, tc.want) {
			t.Errorf("%s: %v, want %v", tc.address, pickErr, tc.want)
		}
	}

	if _, pickErr := Pick(
		block,
		netip.Addr{},
		func(netip.Addr) bool { return true },
	); !errors.Is(
		pickErr,
		ErrExhausted,
	) {
		t.Fatalf("exhaustion: %v", pickErr)
	}

	v6, err := Pick(netip.MustParsePrefix("2001:db8::/64"), netip.Addr{}, func(netip.Addr) bool { return false })
	if err != nil || v6.String() != "2001:db8::4" {
		t.Fatalf("IPv6 address: %s %v", v6, err)
	}
}
