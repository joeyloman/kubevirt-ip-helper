package network

import (
	"net"

	"github.com/vishvananda/netlink"
)

// AddIpToNic and RemoveIpFromNic are package-level indirections over the
// netlink mutations: the controllers call them through the variables, and
// tests can substitute them to exercise the controller flows which sit
// between the netlink operations without privileged host access.
var (
	AddIpToNic      = addIpToNic
	RemoveIpFromNic = removeIpFromNic
)

func addIpToNic(nic string, ip4 string) (err error) {
	linkName, err := netlink.LinkByName(nic)
	if err != nil {
		return
	}

	addr, err := netlink.ParseAddr(ip4)
	if err != nil {
		return
	}

	// adding an address which already exists is the converged state, not
	// a failure: a server ip which is already present on the bind
	// interface (a stale leftover of a failed cleanup, or a second pool
	// sharing the same server ip) must not wedge the registration retry
	// loop, which has no recovery path for a rejected add.
	addrs, err := netlink.AddrList(linkName, netlink.FAMILY_V4)
	if err != nil {
		return
	}
	for _, existing := range addrs {
		if existing.IP.Equal(addr.IP) && ipMaskEqual(existing.Mask, addr.Mask) {
			return nil
		}
	}

	return netlink.AddrAdd(linkName, addr)
}

// ipMaskEqual compares the prefix lengths of two address masks. the
// netlink address list reports the mask either as the 4-byte IPv4 form
// or as a 16-byte one, and the 16-byte spellings come in two shapes: the
// leading-ones form (which Size() reports) and the zero-extended form
// whose IPv4 mask sits in the last four bytes (which Size() cannot
// report, because the trailing-zero shape is not canonical). both are
// normalized to the IPv4 prefix length they describe, so the comparison
// survives every producible spelling instead of only the canonical ones.
func ipMaskEqual(a net.IPMask, b net.IPMask) bool {
	aOnes, aOK := v4MaskPrefixLength(a)
	bOnes, bOK := v4MaskPrefixLength(b)

	return aOK && bOK && aOnes == bOnes
}

// v4MaskPrefixLength reports the IPv4 prefix length of a mask in any of
// its producible spellings: the 4-byte form, the leading-ones 16-byte
// form and the zero-extended 16-byte form. masks which describe no IPv4
// prefix at all report ok=false.
func v4MaskPrefixLength(mask net.IPMask) (ones int, ok bool) {
	switch len(mask) {
	case 4:
		ones, bits := mask.Size()

		return ones, bits == 32
	case 16:
		if ones, bits := mask.Size(); bits == 128 {
			return ones, true
		}

		// the zero-extended spelling: the first twelve bytes must be
		// zero and the IPv4 mask sits in the last four
		for _, b := range mask[:12] {
			if b != 0 {
				return 0, false
			}
		}

		ones, bits := net.IPMask(mask[12:]).Size()

		return ones, bits == 32
	}

	return 0, false
}

func removeIpFromNic(nic string, ip4 string) (err error) {
	linkName, err := netlink.LinkByName(nic)
	if err != nil {
		return
	}

	addr, err := netlink.ParseAddr(ip4)
	if err != nil {
		return
	}

	return netlink.AddrDel(linkName, addr)
}
