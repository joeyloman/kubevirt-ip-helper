package network

import (
	"github.com/vishvananda/netlink"
)

func AddIpToNic(nic string, ip4 string) (err error) {
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

func RemoveIpFromNic(nic string, ip4 string) (err error) {
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
