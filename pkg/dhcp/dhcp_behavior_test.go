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
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
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

// TestDHCPHandlerRelayedReplyDestination: rfc 2131 section 4.1 requires a
// request which arrived through a bootp relay agent to get its successful
// reply unicast to giaddr:67 - the relay agent forwards it towards the
// client - never to the udp peer the relayed packet arrived from (which is
// the relay's own socket, not the client). the broadcast bit stays unset in
// successful replies: the relay unicasts towards the client. a directly
// received discover keeps the peer destination.
func TestDHCPHandlerRelayedReplyDestination(t *testing.T) {
	t.Run("relayed discover is offered to the relay agent", func(t *testing.T) {
		a := newTestPooledAllocator(t)
		conn := &recordingPacketConn{}
		relayPeer := &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: dhcpv4.ClientPort}

		req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeDiscover)
		req.GatewayIPAddr = net.ParseIP("192.0.2.1")
		req.Flags = 0
		a.dhcpHandler(conn, relayPeer, req)

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
		if resp.IsBroadcast() {
			t.Errorf("offer flags = %#x, want no broadcast bit: the relay unicasts a successful reply towards the client", resp.Flags)
		}
		dst, ok := conn.peers[0].(*net.UDPAddr)
		if !ok || !dst.IP.Equal(net.ParseIP("192.0.2.1")) || dst.Port != dhcpv4.ServerPort {
			t.Errorf("offer destination = %v, want 192.0.2.1:%d (giaddr), not the relay peer %v", conn.peers[0], dhcpv4.ServerPort, relayPeer)
		}
	})

	t.Run("relayed request is acked to the relay agent", func(t *testing.T) {
		a := newTestPooledAllocator(t)
		conn := &recordingPacketConn{}
		relayPeer := &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: dhcpv4.ClientPort}

		req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
		req.UpdateOption(dhcpv4.OptServerIdentifier(net.ParseIP("192.168.0.1")))
		req.UpdateOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("192.168.0.50")))
		req.GatewayIPAddr = net.ParseIP("192.0.2.1")
		req.Flags = 0
		a.dhcpHandler(conn, relayPeer, req)

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
		if resp.IsBroadcast() {
			t.Errorf("ack flags = %#x, want no broadcast bit: the relay unicasts a successful reply towards the client", resp.Flags)
		}
		dst, ok := conn.peers[0].(*net.UDPAddr)
		if !ok || !dst.IP.Equal(net.ParseIP("192.0.2.1")) || dst.Port != dhcpv4.ServerPort {
			t.Errorf("ack destination = %v, want 192.0.2.1:%d (giaddr), not the relay peer %v", conn.peers[0], dhcpv4.ServerPort, relayPeer)
		}
	})

	t.Run("direct discover is offered to the peer", func(t *testing.T) {
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
		if got := conn.peers[0].String(); got != peer.String() {
			t.Errorf("offer destination = %v, want the udp peer %v", conn.peers[0], peer)
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

func TestDHCPHandlerInformAckWithoutLeaseTime(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}

	// rfc 2131 4.3.5: an inform is acked with the configuration options
	// only - no yiaddr and no lease time
	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeInform)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 1 {
		t.Fatalf("expected 1 inform ack, got %d", conn.len())
	}
	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Errorf("got message type %v, want Ack for an inform", mt)
	}
	if !resp.YourIPAddr.IsUnspecified() {
		t.Errorf("YourIPAddr = %s, want 0.0.0.0 for an inform ack", resp.YourIPAddr)
	}
	if resp.GetOneOption(dhcpv4.OptionIPAddressLeaseTime) != nil {
		t.Error("inform ack carries the lease time option, rfc 2131 4.3.5 forbids it")
	}
	if resp.GetOneOption(dhcpv4.OptionSubnetMask) == nil {
		t.Error("inform ack must carry the configuration options (subnet mask)")
	}
}

func TestDHCPHandlerDeclineNoReplyKeepsLease(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}

	// rfc 2131 4.3.3: a decline is a one-way notification; the server
	// must not write a reply. the pre-allocated model keeps the lease so
	// the binding's resync re-serves the same address by design
	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeDecline)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 0 {
		t.Errorf("expected 0 writes for DHCPDECLINE, got %d", conn.len())
	}
	if !a.CheckLease("aa:bb:cc:dd:ee:01") {
		t.Error("the decline must not drop the pre-allocated lease")
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

// The guarded lease update runs its callback only while the lease still
// matches the full binding identity - the owner reference, the pool name
// and the client ip: a concurrent cleanup cannot slip between the
// validation and the callback's allocator mutation, and a replacement
// lease registered for the same owner under another network or address
// never reaches the callback of a stale snapshot.
func TestWithOwnedLease(t *testing.T) {
	a := New()
	if err := a.AddLease("aa:bb:cc:dd:ee:01", "pool1", "192.168.0.50", "ns1/vm1"); err != nil {
		t.Fatalf("AddLease: %v", err)
	}

	called := false
	if err := a.WithOwnedLease("aa-bb-cc-dd-ee-01", "ns1/vm1", "pool1", "192.168.0.50", func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithOwnedLease: %v", err)
	}
	if !called {
		t.Error("the callback must run for the matching identity")
	}

	// a foreign owner reference never reaches the callback
	called = false
	if err := a.WithOwnedLease("aa:bb:cc:dd:ee:01", "ns1/other-vm", "pool1", "192.168.0.50", func() error {
		called = true
		return nil
	}); !errors.Is(err, ErrLeaseForeignOwner) {
		t.Errorf("foreign-owner guard = %v, want ErrLeaseForeignOwner", err)
	}
	if called {
		t.Error("the callback must not run for a foreign owner")
	}

	// a lease of the right owner which serves another network never
	// reaches the callback: the stale snapshot must not adopt the
	// identity of a replacement lease
	called = false
	if err := a.WithOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool-other", "192.168.0.50", func() error {
		called = true
		return nil
	}); !errors.Is(err, ErrLeaseIdentityMismatch) {
		t.Errorf("wrong-pool guard = %v, want ErrLeaseIdentityMismatch", err)
	}
	if called {
		t.Error("the callback must not run for a replacement lease of another network")
	}

	// a lease of the right owner which serves another address never
	// reaches the callback either
	called = false
	if err := a.WithOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool1", "192.168.0.99", func() error {
		called = true
		return nil
	}); !errors.Is(err, ErrLeaseIdentityMismatch) {
		t.Errorf("wrong-ip guard = %v, want ErrLeaseIdentityMismatch", err)
	}
	if called {
		t.Error("the callback must not run for a replacement lease of another address")
	}

	// a vanished lease never reaches the callback
	if err := a.DeleteLeaseOwnedBy("aa:bb:cc:dd:ee:01", "ns1/vm1"); err != nil {
		t.Fatalf("DeleteLeaseOwnedBy: %v", err)
	}
	called = false
	if err := a.WithOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool1", "192.168.0.50", func() error {
		called = true
		return nil
	}); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("missing-lease guard = %v, want ErrLeaseNotFound", err)
	}
	if called {
		t.Error("the callback must not run without the lease")
	}

	// an invalid macaddress is rejected before the lock
	if err := a.WithOwnedLease("not-a-mac", "ns1/vm1", "pool1", "192.168.0.50", func() error { return nil }); err == nil {
		t.Error("an invalid macaddress must fail the guarded update")
	}
}

// The single-snapshot identity check backs the re-validation of a
// snapshot lease around a durable ownership write: it must accept the
// full four-part identity and reject every divergence of one part -
// including a replacement lease of the same owner reference.
func TestHasOwnedLease(t *testing.T) {
	a := New()
	if err := a.AddLease("aa:bb:cc:dd:ee:01", "pool1", "192.168.0.50", "ns1/vm1"); err != nil {
		t.Fatalf("AddLease: %v", err)
	}

	if !a.HasOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool1", "192.168.0.50") {
		t.Error("the matching identity must be reported as owned")
	}
	if a.HasOwnedLease("aa:bb:cc:dd:ee:01", "ns1/other-vm", "pool1", "192.168.0.50") {
		t.Error("a foreign owner reference must not match")
	}
	if a.HasOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool-other", "192.168.0.50") {
		t.Error("another network must not match")
	}
	if a.HasOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool1", "192.168.0.99") {
		t.Error("another client ip must not match")
	}
	if a.HasOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool1", "") {
		t.Error("an empty client ip must not match")
	}
	// the hyphen spelling canonicalizes to the same lease key, like every
	// other lease operation
	if !a.HasOwnedLease("aa-bb-cc-dd-ee-01", "ns1/vm1", "pool1", "192.168.0.50") {
		t.Error("a legacy mac spelling must match the canonical lease key")
	}

	if err := a.DeleteLeaseOwnedBy("aa:bb:cc:dd:ee:01", "ns1/vm1"); err != nil {
		t.Fatalf("DeleteLeaseOwnedBy: %v", err)
	}
	if a.HasOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool1", "192.168.0.50") {
		t.Error("a vanished lease must not match")
	}
	if a.HasOwnedLease("not-a-mac", "ns1/vm1", "pool1", "192.168.0.50") {
		t.Error("an invalid macaddress must not match")
	}
}

// The callback's error escapes classifiable: the caller inspects it for
// the allocator outcomes (a foreign owner, a missing subnet).
func TestWithOwnedLeasePropagatesTheCallbackError(t *testing.T) {
	a := New()
	if err := a.AddLease("aa:bb:cc:dd:ee:01", "pool1", "192.168.0.50", "ns1/vm1"); err != nil {
		t.Fatalf("AddLease: %v", err)
	}

	sentinel := errors.New("boom")
	if err := a.WithOwnedLease("aa:bb:cc:dd:ee:01", "ns1/vm1", "pool1", "192.168.0.50", func() error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Errorf("callback error = %v, want the wrapped sentinel", err)
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
	// their own mutex: only the allocator lock itself is under test. the
	// assertions read a snapshot taken under the same mutex - a dial which
	// has not run yet appends into a set nobody is reading, which is fine:
	// the assertion is that every observed resolution ran outside the
	// allocator lock, not that the resolution was complete.
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

	// every access to the observations is mutex-guarded: the assertions
	// read a snapshot taken under the same lock the dials append under
	observationsMutex.Lock()
	observed := append([]bool(nil), lockFreeDuringResolve...)
	observationsMutex.Unlock()

	if len(observed) == 0 {
		t.Fatal("the ntp hostname was not resolved, want the resolver to run during AddPool")
	}
	for _, free := range observed {
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

// TestRemoveLeasesForNetwork pins the pool-deletion lease sweep: deleting
// a pool must not leave its leases behind, which would blackhole the
// renewals of still-running vms against a pool which can never serve them
// again. the sweep is network-scoped and leaves the leases of other
// networks untouched.
func TestRemoveLeasesForNetwork(t *testing.T) {
	a := New()
	if err := a.AddLease("aa:bb:cc:dd:ee:01", "pool-a", "192.168.0.50", "ns1/vm1"); err != nil {
		t.Fatalf("AddLease pool-a: %v", err)
	}
	if err := a.AddLease("aa:bb:cc:dd:ee:02", "pool-a", "192.168.0.51", "ns1/vm2"); err != nil {
		t.Fatalf("AddLease pool-a #2: %v", err)
	}
	if err := a.AddLease("aa:bb:cc:dd:ee:03", "pool-b", "10.0.0.5", "ns1/vm3"); err != nil {
		t.Fatalf("AddLease pool-b: %v", err)
	}

	a.RemoveLeasesForNetwork("pool-a")

	if a.CheckLease("aa:bb:cc:dd:ee:01") {
		t.Error("pool-a lease 1 must be swept")
	}
	if a.CheckLease("aa:bb:cc:dd:ee:02") {
		t.Error("pool-a lease 2 must be swept")
	}
	if !a.CheckLease("aa:bb:cc:dd:ee:03") {
		t.Error("a lease of another network must survive the sweep")
	}
}

// TestDHCPHandlerNoMatchedPoolNaksRequest: a request whose lease's pool is
// gone (a deleted pool whose cleanup is still in flight, or the brief
// reload window) must fail the client fast with a nak instead of dropping
// the packet silently - a silent drop leaves the client retransmitting
// against a server which can never serve the address again.
func TestDHCPHandlerNoMatchedPoolNaksRequest(t *testing.T) {
	a := newTestPooledAllocator(t)
	if err := a.DeletePool("pool1"); err != nil {
		t.Fatalf("deleting the pool: %v", err)
	}
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
	req.UpdateOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("192.168.0.50")))
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 1 {
		t.Fatalf("expected a nak for the request of a vanished pool, got %d", conn.len())
	}
	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeNak {
		t.Errorf("message type = %v, want Nak", mt)
	}
}

// TestDHCPHandlerNoMatchedPoolDiscoverDropped: a discover against a
// vanished pool has nothing to offer - the packet is dropped without a
// reply (never nacked, a nak on a discover is not defined by rfc 2131).
func TestDHCPHandlerNoMatchedPoolDiscoverDropped(t *testing.T) {
	a := newTestPooledAllocator(t)
	if err := a.DeletePool("pool1"); err != nil {
		t.Fatalf("deleting the pool: %v", err)
	}
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 0 {
		t.Errorf("expected no reply for a discover against a vanished pool, got %d", conn.len())
	}
}

// newLoopbackServer builds a server bound to an ephemeral loopback port:
// the production Run binds 0.0.0.0:67, which is not bindable in the test
// environment, so the listener lifecycle tests register the servers
// through the registry directly (the same state Run publishes).
func newLoopbackServer(t *testing.T) *server4.Server {
	t.Helper()

	server, err := server4.NewServer("", &net.UDPAddr{Port: 0}, func(net.PacketConn, net.Addr, *dhcpv4.DHCPv4) {})
	if err != nil {
		t.Fatalf("creating a loopback server: %v", err)
	}

	return server
}

// TestServeAndDeregisterKeepsTheReplacementRegistered pins the identity
// check of the serve-exit wrapper: a Stop followed by a re-Run (the
// restart flow, or a pool re-created after its deletion) can register the
// replacement server before the stopped server's wrapper wakes up, and a
// key-only deregistration would delete the live replacement's registry
// entry - leaving it serving but unreachable for Stop and reported as not
// running by IsRunning.
func TestServeAndDeregisterKeepsTheReplacementRegistered(t *testing.T) {
	a := NewDHCPAllocator()

	// the first listener is registered and stopped like the pool restart
	// flow does: Stop removes the registry entry and closes the socket
	first := newLoopbackServer(t)
	a.servers["net-repair"] = first
	a.serverNics["net-repair"] = "lo"
	if err := a.Stop("net-repair"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// the listener repair re-registers the replacement before the stopped
	// server's serve loop has returned
	second := newLoopbackServer(t)
	a.servers["net-repair"] = second
	a.serverNics["net-repair"] = "lo"

	// the stopped server's wrapper runs late: it must not take the
	// replacement's registration down with it
	a.serveAndDeregister("net-repair", first)

	if !a.IsRunning("net-repair") {
		t.Fatal("the replacement listener must stay registered after the stopped server's wrapper ran")
	}
	if current, running := a.servers["net-repair"]; !running || current != second {
		t.Errorf("the registry must still hold the replacement server, running=%v, own=%v", running, current == second)
	}

	// the replacement's own wrapper deregisters exactly it
	if err := second.Close(); err != nil {
		t.Fatalf("closing the replacement: %v", err)
	}
	a.serveAndDeregister("net-repair", second)

	if a.IsRunning("net-repair") {
		t.Error("the replacement must be deregistered after its own serve loop exited")
	}
}

// TestStopAllDrainsEveryRegisteredServer pins the shutdown sweep: the
// process teardown must stop every listener through the registry itself,
// because it must not depend on the kubernetes api. every registry entry
// is removed and every socket is closed, and a second sweep is a
// converged no-op.
func TestStopAllDrainsEveryRegisteredServer(t *testing.T) {
	a := NewDHCPAllocator()

	servers := map[string]*server4.Server{}
	for _, networkName := range []string{"net-a", "net-b"} {
		server := newLoopbackServer(t)
		a.servers[networkName] = server
		a.serverNics[networkName] = "lo"
		servers[networkName] = server
	}

	a.StopAll()

	for networkName := range servers {
		if a.IsRunning(networkName) {
			t.Errorf("the listener of %s must be deregistered by the sweep", networkName)
		}
	}

	for networkName, server := range servers {
		if err := server.Serve(); err == nil {
			t.Errorf("the socket of %s must be closed by the sweep", networkName)
		}
	}

	// a second sweep has nothing left to drain and must stay a no-op
	a.StopAll()
}

// TestStopAllClosesTheAllocatorAgainstDrainingWorkers pins the shutdown
// fence of the handover ordering: the application stops the dhcp
// listeners at era-cancel time, before the controller drain joins, so a
// draining worker which reaches its listener repair (or a registration)
// after the sweep must be refused instead of re-opening a listener behind
// the teardown - while the leadership lease may already have passed to
// the standby, a re-opened listener would put a second server on the
// segment. the refusal is an error like ErrServerAlreadyRunning (the
// sync fails and the dying era abandons the item), never a silent
// no-op which would report the pool as served.
func TestStopAllClosesTheAllocatorAgainstDrainingWorkers(t *testing.T) {
	a := NewDHCPAllocator()

	a.StopAll()

	// the closed check runs before any socket work: a refused Run must
	// not even try to bind (the test process has no permission for :67)
	err := a.Run("net-a", "lo")
	if !errors.Is(err, ErrAllocatorClosed) {
		t.Fatalf("Run after StopAll = %v, want the ErrAllocatorClosed classification", err)
	}
	if a.IsRunning("net-a") {
		t.Error("a refused Run must not register the network as running")
	}

	// the second sweep stays the converged no-op and the refusal is
	// stable for the rest of the allocator's lifetime
	a.StopAll()
	if err := a.Run("net-b", "lo"); !errors.Is(err, ErrAllocatorClosed) {
		t.Fatalf("Run after a second StopAll = %v, want the ErrAllocatorClosed classification", err)
	}
}
