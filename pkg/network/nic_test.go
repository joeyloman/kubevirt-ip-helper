package network

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestParseAddrAcceptsValidCidrs(t *testing.T) {
	addr, err := netlink.ParseAddr("10.0.0.1/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	if addr == nil || addr.IPNet == nil {
		t.Fatalf("ParseAddr returned empty address")
	}
	if !addr.IPNet.IP.Equal(net.ParseIP("10.0.0.1")) {
		t.Errorf("addr ip = %v, want 10.0.0.1", addr.IPNet.IP)
	}
	ones, bits := addr.IPNet.Mask.Size()
	if ones != 24 || bits != 32 {
		t.Errorf("mask = %d/%d, want 24/32", ones, bits)
	}

	for _, s := range []string{"192.168.1.2/16", "10.0.0.1/32"} {
		if _, err := netlink.ParseAddr(s); err != nil {
			t.Errorf("ParseAddr(%q) returned error: %v", s, err)
		}
	}
	// The vendored netlink accepts trailing text and treats it as a label;
	// ParseAddr keeps the host address in IPNet, so the string form is
	// the host CIDR, not the network base.
	labeled, err := netlink.ParseAddr("10.0.0.1/24 junk")
	if err != nil {
		t.Errorf("ParseAddr with trailing text: %v", err)
	} else if labeled.IPNet == nil || labeled.IPNet.String() != "10.0.0.1/24" {
		t.Errorf("parsed ipnet = %v, want 10.0.0.1/24", labeled.IPNet)
	}
}

func TestParseAddrRejectsInvalidInputs(t *testing.T) {
	invalid := []string{
		"",            // empty
		"10.0.0.1",    // missing mask
		"10.0.0.1/",   // empty mask
		"10.0.0.1/33", // mask out of range
		"not-an-ip",   // garbage
	}
	for _, s := range invalid {
		if _, err := netlink.ParseAddr(s); err == nil {
			t.Errorf("ParseAddr(%q) expected error, got nil", s)
		}
	}
}

func TestAddIpToNicMissingLink(t *testing.T) {
	err := AddIpToNic("kubevirt-ip-helper-test-link-9f3a2c11", "10.0.0.1/24")
	if err == nil {
		t.Fatal("expected error for a missing link")
	}
}

func TestRemoveIpFromNicMissingLink(t *testing.T) {
	err := RemoveIpFromNic("kubevirt-ip-helper-test-link-8c1b7d44", "10.0.0.1/24")
	if err == nil {
		t.Fatal("expected error for a missing link")
	}
}
