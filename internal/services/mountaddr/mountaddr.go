// Package mountaddr picks the address a mount target record reports. The address is metadata only: nothing
// allocates it in EC2 or binds it on a network interface.
package mountaddr

import (
	"errors"
	"net/netip"
)

var (
	// ErrOutsideSubnet reports a requested address that the subnet cannot assign.
	ErrOutsideSubnet = errors.New("address is not a usable address in the subnet CIDR")
	// ErrInUse reports a requested address another record already reports.
	ErrInUse = errors.New("address is in use")
	// ErrExhausted reports a subnet whose usable addresses are all reported.
	ErrExhausted = errors.New("no unused addresses in the subnet")
)

// Pick returns want, or the lowest usable address of block when want is invalid, that used does not report.
// AWS reserves the first four addresses of a subnet and the last one is never handed out.
func Pick(block netip.Prefix, want netip.Addr, used func(netip.Addr) bool) (netip.Addr, error) {
	first := block.Masked().Addr().Next().Next().Next().Next()
	usable := func(addr netip.Addr) bool {
		return block.Contains(addr) && addr.Compare(first) >= 0 && block.Contains(addr.Next())
	}

	if want.IsValid() {
		if !usable(want) {
			return netip.Addr{}, ErrOutsideSubnet
		}

		if used(want) {
			return netip.Addr{}, ErrInUse
		}

		return want, nil
	}

	for addr := first; usable(addr); addr = addr.Next() {
		if !used(addr) {
			return addr, nil
		}
	}

	return netip.Addr{}, ErrExhausted
}
