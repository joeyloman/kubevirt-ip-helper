package network

import (
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

	return netlink.AddrAdd(linkName, addr)
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
