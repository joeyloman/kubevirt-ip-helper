package dhcp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	log "github.com/sirupsen/logrus"
)

// recordingPacketConn implements net.PacketConn without any real network
// interaction, capturing every payload written to it.
type recordingPacketConn struct {
	mu       sync.Mutex
	payloads [][]byte
	peers    []net.Addr
	writeErr error
	closed   bool
}

func (r *recordingPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	return 0, nil, errors.New("ReadFrom not supported in tests")
}

func (r *recordingPacketConn) WriteTo(b []byte, a net.Addr) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloads = append(r.payloads, append([]byte(nil), b...))
	r.peers = append(r.peers, a)
	return len(b), r.writeErr
}

func (r *recordingPacketConn) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *recordingPacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 67}
}

func (r *recordingPacketConn) SetDeadline(t time.Time) error      { return nil }
func (r *recordingPacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (r *recordingPacketConn) SetWriteDeadline(t time.Time) error { return nil }

func (r *recordingPacketConn) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.payloads)
}

func mustHWAddr(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	hw, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return hw
}

func newTestPooledAllocator(t *testing.T) *DHCPAllocator {
	t.Helper()
	a := NewDHCPAllocator()
	if err := a.AddPool(
		"pool1",
		"192.168.0.1",
		"255.255.255.0",
		"192.168.0.254",
		[]string{"1.1.1.1", "8.8.8.8"},
		"example.com",
		[]string{"example.com"},
		[]string{"10.0.0.53"},
		3600,
		"eth0",
	); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	if err := a.AddLease(
		"aa:bb:cc:dd:ee:01",
		"pool1",
		"192.168.0.50",
		"ref-1",
	); err != nil {
		t.Fatalf("AddLease: %v", err)
	}
	return a
}

func newBootRequest(t *testing.T, hwAddr net.HardwareAddr, msgType dhcpv4.MessageType) *dhcpv4.DHCPv4 {
	t.Helper()
	m, err := dhcpv4.New(
		dhcpv4.WithHwAddr(hwAddr),
		dhcpv4.WithMessageType(msgType),
	)
	if err != nil {
		t.Fatalf("dhcpv4.New: %v", err)
	}
	return m
}

func testPeer() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 68}
}

func TestDHCPHandlerOfferForDiscover(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}
	peer := testPeer()

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler(conn, peer, req)

	if conn.len() != 1 {
		t.Fatalf("expected 1 reply, got %d", conn.len())
	}

	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}

	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeOffer {
		t.Errorf("got message type %v, want Offer", mt)
	}
	if !resp.YourIPAddr.Equal(net.ParseIP("192.168.0.50")) {
		t.Errorf("YourIPAddr = %s, want 192.168.0.50", resp.YourIPAddr)
	}
	if !resp.ServerIPAddr.Equal(net.ParseIP("192.168.0.1")) {
		t.Errorf("ServerIPAddr = %s, want 192.168.0.1", resp.ServerIPAddr)
	}
	if !resp.ClientIPAddr.IsUnspecified() {
		t.Errorf("ClientIPAddr = %s, want 0.0.0.0; rfc 2131 requires a zero ciaddr in an offer for an initial discover", resp.ClientIPAddr)
	}
	if resp.TransactionID != req.TransactionID {
		t.Errorf("transaction id = %s, want %s", resp.TransactionID, req.TransactionID)
	}
	if !resp.GatewayIPAddr.Equal(req.GatewayIPAddr) {
		t.Errorf("gateway = %s, want %s", resp.GatewayIPAddr, req.GatewayIPAddr)
	}
	if got := resp.SubnetMask(); !bytes.Equal(got, net.IPMask{255, 255, 255, 0}) {
		t.Errorf("subnet mask = %v, want 255.255.255.0", got)
	}
	if got := resp.Router(); len(got) != 1 || !got[0].Equal(net.ParseIP("192.168.0.254")) {
		t.Errorf("router = %v, want [192.168.0.254]", got)
	}
	if got := resp.DNS(); len(got) != 2 || !got[0].Equal(net.ParseIP("1.1.1.1")) || !got[1].Equal(net.ParseIP("8.8.8.8")) {
		t.Errorf("dns = %v, want [1.1.1.1 8.8.8.8]", got)
	}
	if got := resp.DomainName(); got != "example.com" {
		t.Errorf("domain name = %q, want example.com", got)
	}
	if got := resp.DomainSearch(); got == nil || len(got.Labels) != 1 || got.Labels[0] != "example.com" {
		t.Errorf("domain search = %v, want [example.com]", got)
	}
	if got := resp.NTPServers(); len(got) != 1 || !got[0].Equal(net.ParseIP("10.0.0.53")) {
		t.Errorf("ntp = %v, want [10.0.0.53]", got)
	}
	if got := resp.IPAddressLeaseTime(0); got != 3600*time.Second {
		t.Errorf("lease time = %v, want 3600s", got)
	}
	if !bytes.Equal(resp.ClientHWAddr, req.ClientHWAddr) {
		t.Errorf("client hw addr = %s, want %s", resp.ClientHWAddr, req.ClientHWAddr)
	}
	if got := conn.peers[0]; got.String() != peer.String() {
		t.Errorf("reply peer = %v, want %v", got, peer)
	}
}

func TestDHCPHandlerRequestAddressing(t *testing.T) {
	leaseIP := net.ParseIP("192.168.0.50")
	serverIP := net.ParseIP("192.168.0.1")

	t.Run("selecting request with matching options is acked", func(t *testing.T) {
		a := newTestPooledAllocator(t)
		conn := &recordingPacketConn{}

		req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
		req.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))
		req.UpdateOption(dhcpv4.OptRequestedIPAddress(leaseIP))
		a.dhcpHandler(conn, testPeer(), req)

		if conn.len() != 1 {
			t.Fatalf("expected 1 reply, got %d", conn.len())
		}
		resp, err := dhcpv4.FromBytes(conn.payloads[0])
		if err != nil {
			t.Fatalf("parsing reply: %v", err)
		}
		if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
			t.Errorf("got message type %v, want Ack", mt)
		}
		if !resp.YourIPAddr.Equal(leaseIP) {
			t.Errorf("YourIPAddr = %s, want 192.168.0.50", resp.YourIPAddr)
		}
		if !resp.ClientIPAddr.IsUnspecified() {
			t.Errorf("ClientIPAddr = %s, want 0.0.0.0 for a selecting request", resp.ClientIPAddr)
		}
	})

	t.Run("renewal through ciaddr is acked", func(t *testing.T) {
		a := newTestPooledAllocator(t)
		conn := &recordingPacketConn{}

		req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
		req.ClientIPAddr = leaseIP
		a.dhcpHandler(conn, testPeer(), req)

		if conn.len() != 1 {
			t.Fatalf("expected 1 reply, got %d", conn.len())
		}
		resp, err := dhcpv4.FromBytes(conn.payloads[0])
		if err != nil {
			t.Fatalf("parsing reply: %v", err)
		}
		if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
			t.Errorf("got message type %v, want Ack", mt)
		}
		if !resp.ClientIPAddr.Equal(leaseIP) {
			t.Errorf("ClientIPAddr = %s, want the renewed ciaddr 192.168.0.50", resp.ClientIPAddr)
		}
		if !resp.YourIPAddr.Equal(leaseIP) {
			t.Errorf("YourIPAddr = %s, want 192.168.0.50", resp.YourIPAddr)
		}
	})
	t.Run("mismatched requested ip is nacked to the broadcast address", func(t *testing.T) {
		a := newTestPooledAllocator(t)
		conn := &recordingPacketConn{}

		req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
		req.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))
		req.UpdateOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("192.168.0.99")))
		// the client requires a broadcast reply: the non-relayed nak must
		// preserve that bit, not force or clear flags on its own
		req.Flags = 0x8000
		a.dhcpHandler(conn, testPeer(), req)

		if conn.len() != 1 {
			t.Fatalf("expected 1 reply, got %d", conn.len())
		}
		resp, err := dhcpv4.FromBytes(conn.payloads[0])
		if err != nil {
			t.Fatalf("parsing reply: %v", err)
		}
		if mt := resp.MessageType(); mt != dhcpv4.MessageTypeNak {
			t.Errorf("got message type %v, want Nak for a mismatched address", mt)
		}
		if !resp.YourIPAddr.IsUnspecified() {
			t.Errorf("YourIPAddr = %s, want 0.0.0.0 for a nak", resp.YourIPAddr)
		}
		if !resp.IsBroadcast() {
			t.Errorf("nak flags = %#x, want the client's broadcast bit preserved", resp.Flags)
		}

		// rfc 2131 section 3.2: without a relay address the nak must be
		// broadcast, never unicast to the rejected claim address, because
		// the client may not hold a valid address and may not answer arp
		dst, ok := conn.peers[0].(*net.UDPAddr)
		if !ok || dst.IP.String() != "255.255.255.255" || dst.Port != dhcpv4.ClientPort {
			t.Errorf("nak destination = %v, want 255.255.255.255:%d", conn.peers[0], dhcpv4.ClientPort)
		}

		// the complementary case: a client which sent no broadcast bit must
		// not have one forced onto the non-relayed nak (only relayed naks
		// set the bit, rfc 2131 section 4.3.2)
		conn2 := &recordingPacketConn{}
		req.Flags = 0
		a.dhcpHandler(conn2, testPeer(), req)
		if conn2.len() != 1 {
			t.Fatalf("expected 1 reply, got %d", conn2.len())
		}
		resp2, err := dhcpv4.FromBytes(conn2.payloads[0])
		if err != nil {
			t.Fatalf("parsing reply: %v", err)
		}
		if resp2.IsBroadcast() {
			t.Errorf("nak flags = %#x, want the client's zero flags echoed", resp2.Flags)
		}
	})

	t.Run("mismatched request through a relay is nacked to the relay agent", func(t *testing.T) {
		a := newTestPooledAllocator(t)
		conn := &recordingPacketConn{}

		req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
		req.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))
		req.UpdateOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("192.168.0.99")))
		// the client cannot receive unicast yet (init-reboot), but the
		// request itself carries no broadcast bit: the server must set it
		req.GatewayIPAddr = net.ParseIP("203.0.113.1")
		req.Flags = 0
		a.dhcpHandler(conn, testPeer(), req)

		if conn.len() != 1 {
			t.Fatalf("expected 1 reply, got %d", conn.len())
		}
		resp, err := dhcpv4.FromBytes(conn.payloads[0])
		if err != nil {
			t.Fatalf("parsing reply: %v", err)
		}
		if mt := resp.MessageType(); mt != dhcpv4.MessageTypeNak {
			t.Errorf("got message type %v, want Nak for a mismatched address", mt)
		}

		// rfc 2131 section 4.3.2: a relayed init-reboot client may not
		// hold a valid address and may not answer arp, so the nak must set
		// the broadcast bit for the relay agent to broadcast it
		if !resp.IsBroadcast() {
			t.Errorf("nak flags = %#x, want the broadcast bit set", resp.Flags)
		}

		// rfc 2131 section 3.2: a relayed request gets the nak sent to the
		// bootp relay agent, which forwards it to the client's hardware
		// address
		dst, ok := conn.peers[0].(*net.UDPAddr)
		if !ok || !dst.IP.Equal(net.ParseIP("203.0.113.1")) || dst.Port != dhcpv4.ServerPort {
			t.Errorf("nak destination = %v, want 203.0.113.1:%d", conn.peers[0], dhcpv4.ServerPort)
		}
	})

	t.Run("request for another server is ignored", func(t *testing.T) {
		a := newTestPooledAllocator(t)
		conn := &recordingPacketConn{}

		req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
		req.UpdateOption(dhcpv4.OptServerIdentifier(net.ParseIP("203.0.113.9")))
		req.UpdateOption(dhcpv4.OptRequestedIPAddress(leaseIP))
		a.dhcpHandler(conn, testPeer(), req)

		if conn.len() != 0 {
			t.Errorf("expected no reply for a foreign server id, got %d", conn.len())
		}
	})
}

func TestDHCPHandlerReleaseGetsNoReply(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRelease)
	req.ClientIPAddr = net.ParseIP("192.168.0.50")
	a.dhcpHandler(conn, testPeer(), req)

	// rfc 2131 4.3.4: a release is a one-way notification; the server
	// must not write any bootreply back to the client
	if conn.len() != 0 {
		t.Errorf("expected 0 writes for DHCPRELEASE, got %d", conn.len())
	}
}

func TestDHCPHandlerUnhandledMessageTypeNoReply(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeInform)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 0 {
		t.Errorf("expected no reply for unhandled message type, got %d", conn.len())
	}
}

func TestDHCPHandlerMissingLeaseNoReply(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "00:11:22:33:44:55"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 0 {
		t.Errorf("expected no reply without a lease, got %d", conn.len())
	}
}

func TestDHCPHandlerMissingPoolNoReply(t *testing.T) {
	a := NewDHCPAllocator()
	if err := a.AddLease("aa:bb:cc:dd:ee:02", "ghost-pool", "192.168.0.60", ""); err != nil {
		t.Fatalf("AddLease: %v", err)
	}
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:02"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 0 {
		t.Errorf("expected no reply without a matching pool, got %d", conn.len())
	}
}

func TestDHCPHandlerNilPacketNoReply(t *testing.T) {
	a := NewDHCPAllocator()
	conn := &recordingPacketConn{}

	a.dhcpHandler(conn, testPeer(), nil)

	if conn.len() != 0 {
		t.Errorf("expected no reply for a nil packet, got %d", conn.len())
	}
}

func TestDHCPHandlerBootReplyOpcodeNoReply(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeDiscover)
	req.OpCode = dhcpv4.OpcodeBootReply
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 0 {
		t.Errorf("expected no reply for a bootreply opcode, got %d", conn.len())
	}
}

func TestDHCPHandlerDefaultLeaseTime(t *testing.T) {
	a := NewDHCPAllocator()
	if err := a.AddPool(
		"pool0",
		"192.168.0.1",
		"255.255.255.0",
		"192.168.0.254",
		nil,
		"",
		nil,
		nil,
		0,
		"eth0",
	); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	if err := a.AddLease("aa:bb:cc:dd:ee:03", "pool0", "192.168.0.50", ""); err != nil {
		t.Fatalf("AddLease: %v", err)
	}
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:03"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 1 {
		t.Fatalf("expected 1 reply, got %d", conn.len())
	}
	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	if got := resp.IPAddressLeaseTime(0); got != 31536000*time.Second {
		t.Errorf("lease time = %v, want 31536000s", got)
	}
}

func TestAddPoolOverwritesExistingPool(t *testing.T) {
	a := New()
	if err := a.AddPool("v1", "192.168.0.1", "255.255.255.0", "192.168.0.254", nil, "", nil, nil, 300, "eth0"); err != nil {
		t.Fatalf("first AddPool: %v", err)
	}
	// Re-adding the same pool name replaces the previous pool with no error.
	if err := a.AddPool("v1", "192.168.0.1", "255.255.255.0", "192.168.0.1", nil, "", nil, nil, 600, "eth0"); err != nil {
		t.Fatalf("second AddPool: %v", err)
	}

	pool := a.GetPool("v1")
	if !pool.Router.Equal(net.ParseIP("192.168.0.1")) {
		t.Errorf("router = %s, want 192.168.0.1", pool.Router)
	}
	if pool.LeaseTime != 600 {
		t.Errorf("leasetime = %d, want 600", pool.LeaseTime)
	}
}

// mac addresses are stored under the canonical colon form, so every
// spelling of the same address must resolve to the same lease and a
// duplicate spelling must be rejected instead of creating a second
// independent identity. the spellings stay within the delimiter forms
// every toolchain parses: the delimiter-free form is only accepted by
// standard libraries newer than the declared go version.
func TestAddLeaseCanonicalizesIdentity(t *testing.T) {
	a := New()
	for _, hw := range []string{"aa-bb-cc-dd-ee-01", "aabb.ccdd.ee02"} {
		if err := a.AddLease(hw, "pool1", "192.168.0.50", "ref"); err != nil {
			t.Fatalf("AddLease(%q): %v", hw, err)
		}
	}

	// every spelling resolves to the canonical colon key
	if !a.CheckLease("aa:bb:cc:dd:ee:01") {
		t.Error("hyphen-form lease not resolvable in canonical colon form")
	}
	if !a.CheckLease("aabb.ccdd.ee02") {
		t.Error("cisco-form lease not resolvable in canonical colon form")
	}
	if !a.CheckLease("AA-BB-CC-DD-EE-01") {
		t.Error("uppercase hyphen-form lease not resolvable in canonical colon form")
	}

	if got := a.GetLease("AA:BB:CC:DD:EE:01"); got.ClientIP == nil || got.ClientIP.String() != "192.168.0.50" || got.Reference != "ref" {
		t.Errorf("lease queried through an uppercase spelling = %+v, want the original allocation", got)
	}

	// the second spelling of the first address is rejected as duplicate
	if err := a.AddLease("aabb.ccdd.ee01", "pool1", "192.168.0.51", "other-ref"); err == nil {
		t.Fatal("a duplicate spelling of an existing lease was accepted")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate error = %q, want already-exists message", err)
	}

	lease := a.GetLease("aa:bb:cc:dd:ee:01")
	if !lease.ClientIP.Equal(net.ParseIP("192.168.0.50")) || lease.Reference != "ref" {
		t.Errorf("lease = %+v, want the original allocation preserved", lease)
	}

	// deleting through another spelling must free the canonical identity
	if err := a.DeleteLease("AA-BB-CC-DD-EE-01"); err != nil {
		t.Fatalf("the canonical lease must be deletable via another spelling: %v", err)
	}
	if a.CheckLease("aa:bb:cc:dd:ee:01") {
		t.Error("lease still exists after deleting it through another spelling")
	}
}

// a lease deletion which validates the owner under the same lock is the
// primitive the vmnetcfg cleanup uses to decide against a concurrent
// reassignment: a foreign owner must not delete, and a missing lease must
// converge.
func TestDeleteLeaseOwnedBy(t *testing.T) {
	a := New()
	if err := a.AddLease("aa:bb:cc:dd:ee:01", "pool1", "192.168.0.50", "ns1/vm1"); err != nil {
		t.Fatalf("AddLease: %v", err)
	}

	if err := a.DeleteLeaseOwnedBy("aa-bb-cc-dd-ee-01", "ns1/other-vm"); !errors.Is(err, ErrLeaseForeignOwner) {
		t.Errorf("foreign-owner deletion = %v, want ErrLeaseForeignOwner", err)
	}
	if !a.CheckLease("aa:bb:cc:dd:ee:01") {
		t.Error("the foreign owner must not delete the lease")
	}

	// the owner itself may delete through an alternative spelling
	if err := a.DeleteLeaseOwnedBy("aa:bb:cc:dd:ee:01", "ns1/vm1"); err != nil {
		t.Errorf("owner deletion = %v, want nil", err)
	}
	if a.CheckLease("aa:bb:cc:dd:ee:01") {
		t.Error("lease must be gone after the owner deleted it")
	}

	if err := a.DeleteLeaseOwnedBy("aa:bb:cc:dd:ee:02", "ns1/vm1"); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("deletion without a lease = %v, want ErrLeaseNotFound", err)
	}
}

func captureLogrus(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	origOut := log.StandardLogger().Out
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)
	fn()
	return buf.String()
}

func TestUsageLogsLeaseDetails(t *testing.T) {
	a := newTestPooledAllocator(t)

	out := captureLogrus(t, a.Usage)

	for _, want := range []string{
		"(dhcp.Usage)",
		"hwaddr=aa:bb:cc:dd:ee:01",
		"pool=pool1",
		"clientip=192.168.0.50",
		"netmask=ffffff00",
		"router=192.168.0.254",
		"domain=example.com",
		"leasetime=3600",
		"ref=ref-1",
		"nic=eth0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Usage log missing %q", want)
		}
	}
}

func TestUsageWithMissingPoolDoesNotPanic(t *testing.T) {
	a := newTestPooledAllocator(t)
	if err := a.DeletePool("pool1"); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}

	out := captureLogrus(t, a.Usage)

	// Leases whose pool was deleted are still reported (with nil pool
	// fields) instead of panicking on nil addresses.
	if !strings.Contains(out, "hwaddr=aa:bb:cc:dd:ee:01") {
		t.Errorf("Usage log missing lease line after pool deletion: %s", out)
	}
}

// Hostname resolution in AddPool is intentionally not covered: without a
// resolver seam the results depend on the host configuration (localhost may
// map to several v4/v6 addresses, name lookups may delay or vary). Only
// literal addresses are tested here.
func TestAddPoolAcceptsLiteralNTPAddresses(t *testing.T) {
	a := New()
	if err := a.AddPool("p", "192.168.0.1", "255.255.255.0", "192.168.0.254", nil, "", nil, []string{"10.0.0.53"}, 3600, "eth0"); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	pool := a.GetPool("p")
	if len(pool.NTP) != 1 {
		t.Fatalf("NTP entries = %d, want 1", len(pool.NTP))
	}
	if !pool.NTP[0].Equal(net.ParseIP("10.0.0.53")) {
		t.Errorf("NTP = %v, want 10.0.0.53", pool.NTP)
	}
}

func TestDHCPHandlerOmitsUnsetPoolOptions(t *testing.T) {
	a := NewDHCPAllocator()
	if err := a.AddPool("p", "192.168.0.1", "255.255.255.0", "192.168.0.254", nil, "", nil, nil, 7200, "eth0"); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	if err := a.AddLease("aa:bb:cc:dd:ee:04", "p", "192.168.0.50", ""); err != nil {
		t.Fatalf("AddLease: %v", err)
	}
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:04"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 1 {
		t.Fatalf("expected 1 reply, got %d", conn.len())
	}
	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	if got := resp.DNS(); len(got) != 0 {
		t.Errorf("dns = %v, want none", got)
	}
	if got := resp.DomainName(); got != "" {
		t.Errorf("domain name = %q, want empty", got)
	}
	if got := resp.DomainSearch(); got != nil {
		t.Errorf("domain search = %v, want nil", got)
	}
	if got := resp.NTPServers(); len(got) != 0 {
		t.Errorf("ntp = %v, want none", got)
	}
	if got := resp.IPAddressLeaseTime(0); got != 7200*time.Second {
		t.Errorf("lease time = %v, want 7200s", got)
	}
}

// pool lookups from the packet handlers must share the map safely with the
// control-plane writers: ippool reloads and cleanups delete and recreate
// pools while the dhcp server goroutines answer requests. this test runs
// both call paths concurrently and asserts only the synchronization; reply
// outcomes are covered by the single-threaded handler tests.
func TestDHCPPoolAccessesStaySynchronized(t *testing.T) {
	a := NewDHCPAllocator()
	if err := a.AddPool(
		"pool1",
		"192.168.0.1",
		"255.255.255.0",
		"192.168.0.254",
		nil,
		"",
		nil,
		nil,
		3600,
		"eth0",
	); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	if err := a.AddLease("aa:bb:cc:dd:ee:01", "pool1", "192.168.0.50", "ref"); err != nil {
		t.Fatalf("AddLease: %v", err)
	}

	level := log.GetLevel()
	log.SetLevel(log.PanicLevel)
	defer log.SetLevel(level)

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeDiscover)
	conn := &recordingPacketConn{}
	peer := testPeer()

	const workers = 4
	const rounds = 50

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			_ = a.DeletePool("pool1")

			if err := a.AddPool(
				"pool1",
				"192.168.0.1",
				"255.255.255.0",
				"192.168.0.254",
				nil,
				"",
				nil,
				nil,
				3600,
				"eth0",
			); err != nil {
				t.Errorf("recreating the pool: %v", err)

				return
			}
		}
	}()

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				a.dhcpHandler(conn, peer, req)
				_ = a.CheckPool("pool1")
				_ = a.GetPool("pool1")
				a.Usage()
			}
		}()
	}

	wg.Wait()
}

// the ownership guard before a release compares leases within one
// network only: identical numeric addresses served by separate networks
// are different allocations and must not collide with each other
func TestGetLeaseByIPAndNetworkIsNetworkScoped(t *testing.T) {
	a := New()
	if err := a.AddLease("aa:bb:cc:dd:ee:01", "net-a", "192.168.0.50", "ns/vm-a"); err != nil {
		t.Fatalf("AddLease net-a: %v", err)
	}
	if err := a.AddLease("aa:bb:cc:dd:ee:02", "net-b", "192.168.0.50", "ns/vm-b"); err != nil {
		t.Fatalf("AddLease net-b: %v", err)
	}

	hw, lease, found := a.GetLeaseByIPAndNetwork("net-a", "192.168.0.50")
	if !found {
		t.Fatal("want net-a's lease for the address")
	}
	if hw != "aa:bb:cc:dd:ee:01" || lease.Reference != "ns/vm-a" {
		t.Errorf("net-a lookup = (%s, %+v), want net-a's own lease", hw, lease)
	}

	hw, lease, found = a.GetLeaseByIPAndNetwork("net-b", "192.168.0.50")
	if !found {
		t.Fatal("want net-b's lease for the address")
	}
	if hw != "aa:bb:cc:dd:ee:02" || lease.Reference != "ns/vm-b" {
		t.Errorf("net-b lookup = (%s, %+v), want net-b's own lease", hw, lease)
	}

	if _, _, found := a.GetLeaseByIPAndNetwork("net-c", "192.168.0.50"); found {
		t.Error("a lookup under an unregistered networkname must find nothing")
	}
}

// ntp hostnames must be resolved outside the allocator lock: the dhcp
// packet handler reads the pools and leases under the same mutex, so
// resolving inside the lock lets one pool with a slow or broken ntp
// hostname stall renewals on every other pool.
func TestAddPoolResolvesNTPHostnamesOutsideTheAllocatorLock(t *testing.T) {
	a := New()

	// the resolver dials from concurrent goroutines (one per dns server),
	// so the observations of the allocator lock must be collected under
	// their own mutex: only the allocator lock itself is under test
	var observationsMutex sync.Mutex
	var lockFreeDuringResolve []bool
	a.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// the probe runs serialized with itself: two concurrent dial
			// goroutines probing the allocator lock at the same time would
			// contend on it and report a held lock which nobody holds
			observationsMutex.Lock()

			// the resolver runs on the AddPool goroutine: if the allocator
			// lock is already held there, the resolution happens under it
			free := a.mutex.TryLock()
			if free {
				a.mutex.Unlock()
			}
			lockFreeDuringResolve = append(lockFreeDuringResolve, free)

			observationsMutex.Unlock()

			// fail fast: an unresolvable hostname is logged and skipped, so
			// the pool registers without ntp servers like before
			return nil, errors.New("no dns server in tests")
		},
	}

	if err := a.AddPool("net-a", "192.168.0.1", "255.255.255.0", "192.168.0.1", nil, "", nil, []string{"192.168.0.10", "ntp.example.invalid"}, 60, "lo"); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	if len(lockFreeDuringResolve) == 0 {
		t.Fatal("the ntp hostname was not resolved, want the resolver to run during AddPool")
	}
	for _, free := range lockFreeDuringResolve {
		if !free {
			t.Error("the ntp hostname was resolved while the allocator lock was held: packet processing would stall behind the resolver")
		}
	}

	// literal ips register without resolution, the unresolvable entry is
	// skipped and the pool still comes up
	pool := a.GetPool("net-a")
	if len(pool.NTP) != 1 || !pool.NTP[0].Equal(net.ParseIP("192.168.0.10")) {
		t.Errorf("pool ntp = %v, want only the literal 192.168.0.10", pool.NTP)
	}
}
