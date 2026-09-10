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

// ipMaskEqual compares the prefix lengths of two address masks: the
// netlink address list reports the mask either as the 4-byte IPv4 form or
// as a zero-extended 16-byte one, so the byte representations are compared
// through their prefix length instead of directly.
func ipMaskEqual(a net.IPMask, b net.IPMask) bool {
	aOnes, aBits := a.Size()
	bOnes, bBits := b.Size()

	if aBits != bBits {
		// only the v4-native and the v4-in-v6 zero-extended widths can
		// describe the same ipv4 prefix
		if !v4MaskWidth(aBits) || !v4MaskWidth(bBits) {
			return false
		}
	}

	return aOnes == bOnes
}

// v4MaskWidth reports whether the mask width can describe an ipv4
// address: the native 32-bit width or the zero-extended 16-byte width of
// the ipv4-in-ipv6 representation.
func v4MaskWidth(bits int) bool {
	return bits == 32 || bits == 128
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
