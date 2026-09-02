package dhcp

import (
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// capturePacketConn implements net.PacketConn and records the packets which
// the dhcp handler answers instead of running a udp socket.
type capturePacketConn struct {
	written [][]byte
}

func (c *capturePacketConn) ReadFrom(_ []byte) (int, net.Addr, error) {
	// the handler must never read from this conn
	select {}
}

func (c *capturePacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	packet := make([]byte, len(b))
	copy(packet, b)
	c.written = append(c.written, packet)

	return len(b), nil
}

func (c *capturePacketConn) Close() error                       { return nil }
func (c *capturePacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (c *capturePacketConn) SetDeadline(_ time.Time) error      { return nil }
func (c *capturePacketConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *capturePacketConn) SetWriteDeadline(_ time.Time) error { return nil }

func (c *capturePacketConn) singleReply(t *testing.T) *dhcpv4.DHCPv4 {
	t.Helper()

	if len(c.written) != 1 {
		t.Fatalf("got %d writes, want exactly 1", len(c.written))
	}

	reply, err := dhcpv4.FromBytes(c.written[0])
	if err != nil {
		t.Fatalf("decoding the captured reply failed: %s", err)
	}

	return reply
}

func newDHCPTarget(t *testing.T) (*DHCPAllocator, net.IP) {
	t.Helper()

	handler := New()
	if err := handler.AddPool(
		"net-a",
		"10.10.10.1",
		"255.255.255.0",
		"10.10.10.254",
		[]string{"10.10.10.2", "10.10.10.3"},
		"example.local",
		[]string{"example.local"},
		[]string{"10.10.10.4"},
		3600,
		"eth-test",
	); err != nil {
		t.Fatalf("failed to seed dhcp pool: %s", err)
	}

	return handler, net.ParseIP("10.10.10.1")
}

func TestDHCPHandlerOfferLeavesCiaddrZero(t *testing.T) {
	handler, _ := newDHCPTarget(t)
	if err := handler.AddLease("aa:bb:cc:dd:ee:ff", "net-a", "10.10.10.20", "default/testvm"); err != nil {
		t.Fatalf("failed to seed lease: %s", err)
	}

	request, err := dhcpv4.New(dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover), dhcpv4.WithHwAddr(mustHardwareAddr(t, "aa:bb:cc:dd:ee:ff")))
	if err != nil {
		t.Fatalf("failed to build discovery: %s", err)
	}

	conn := &capturePacketConn{}
	handler.dhcpHandler(conn, &net.UDPAddr{IP: net.ParseIP("10.10.10.20"), Port: 68}, request)

	reply := conn.singleReply(t)
	if reply.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("reply message type = %s, want OFFER", reply.MessageType())
	}
	if !reply.ClientIPAddr.IsUnspecified() {
		t.Errorf("reply ciaddr = %s, want 0.0.0.0 for an offer", reply.ClientIPAddr)
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.10.10.20")) {
		t.Errorf("reply yiaddr = %s, want the offered lease address", reply.YourIPAddr)
	}
}

func TestDHCPHandlerReleaseGetsNoReply(t *testing.T) {
	handler, _ := newDHCPTarget(t)
	if err := handler.AddLease("aa:bb:cc:dd:ee:ff", "net-a", "10.10.10.20", "default/testvm"); err != nil {
		t.Fatalf("failed to seed lease: %s", err)
	}

	request, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRelease),
		dhcpv4.WithHwAddr(mustHardwareAddr(t, "aa:bb:cc:dd:ee:ff")),
		dhcpv4.WithClientIP(net.ParseIP("10.10.10.20")),
	)
	if err != nil {
		t.Fatalf("failed to build release: %s", err)
	}

	conn := &capturePacketConn{}
	handler.dhcpHandler(conn, &net.UDPAddr{IP: net.ParseIP("10.10.10.20"), Port: 68}, request)

	if len(conn.written) != 0 {
		t.Errorf("got %d writes for a release, want 0 (release is a one-way notification)", len(conn.written))
	}
}

func TestDHCPHandlerRequestAddressValidation(t *testing.T) {
	t.Run("selecting with matching requested ip is acked", func(t *testing.T) {
		handler, serverIP := newDHCPTarget(t)
		if err := handler.AddLease("aa:bb:cc:dd:ee:ff", "net-a", "10.10.10.20", "default/testvm"); err != nil {
			t.Fatalf("failed to seed lease: %s", err)
		}

		request, err := dhcpv4.New(
			dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
			dhcpv4.WithHwAddr(mustHardwareAddr(t, "aa:bb:cc:dd:ee:ff")),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverIP)),
			dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("10.10.10.20"))),
		)
		if err != nil {
			t.Fatalf("failed to build request: %s", err)
		}

		conn := &capturePacketConn{}
		handler.dhcpHandler(conn, &net.UDPAddr{IP: net.ParseIP("10.10.10.20"), Port: 68}, request)

		reply := conn.singleReply(t)
		if reply.MessageType() != dhcpv4.MessageTypeAck {
			t.Errorf("reply message type = %s, want ACK", reply.MessageType())
		}
		if !reply.ClientIPAddr.IsUnspecified() {
			t.Errorf("reply ciaddr = %s, want 0.0.0.0 for a selecting request", reply.ClientIPAddr)
		}
		if !reply.YourIPAddr.Equal(net.ParseIP("10.10.10.20")) {
			t.Errorf("reply yiaddr = %s, want the leased address", reply.YourIPAddr)
		}
	})

	t.Run("renewal with matching ciaddr is acked", func(t *testing.T) {
		handler, _ := newDHCPTarget(t)
		if err := handler.AddLease("aa:bb:cc:dd:ee:ff", "net-a", "10.10.10.20", "default/testvm"); err != nil {
			t.Fatalf("failed to seed lease: %s", err)
		}

		request, err := dhcpv4.New(
			dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
			dhcpv4.WithHwAddr(mustHardwareAddr(t, "aa:bb:cc:dd:ee:ff")),
			dhcpv4.WithClientIP(net.ParseIP("10.10.10.20")),
		)
		if err != nil {
			t.Fatalf("failed to build request: %s", err)
		}

		conn := &capturePacketConn{}
		handler.dhcpHandler(conn, &net.UDPAddr{IP: net.ParseIP("10.10.10.20"), Port: 68}, request)

		reply := conn.singleReply(t)
		if reply.MessageType() != dhcpv4.MessageTypeAck {
			t.Errorf("reply message type = %s, want ACK", reply.MessageType())
		}
		if !reply.ClientIPAddr.Equal(net.ParseIP("10.10.10.20")) {
			t.Errorf("reply ciaddr = %s, want the renewed client address", reply.ClientIPAddr)
		}
		if !reply.YourIPAddr.Equal(net.ParseIP("10.10.10.20")) {
			t.Errorf("reply yiaddr = %s, want the leased address", reply.YourIPAddr)
		}
	})

	t.Run("mismatched requested ip is nacked", func(t *testing.T) {
		handler, serverIP := newDHCPTarget(t)
		if err := handler.AddLease("aa:bb:cc:dd:ee:ff", "net-a", "10.10.10.20", "default/testvm"); err != nil {
			t.Fatalf("failed to seed lease: %s", err)
		}

		request, err := dhcpv4.New(
			dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
			dhcpv4.WithHwAddr(mustHardwareAddr(t, "aa:bb:cc:dd:ee:ff")),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverIP)),
			dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("10.10.10.99"))),
		)
		if err != nil {
			t.Fatalf("failed to build request: %s", err)
		}

		conn := &capturePacketConn{}
		handler.dhcpHandler(conn, &net.UDPAddr{IP: net.ParseIP("10.10.10.20"), Port: 68}, request)

		reply := conn.singleReply(t)
		if reply.MessageType() != dhcpv4.MessageTypeNak {
			t.Errorf("reply message type = %s, want NAK", reply.MessageType())
		}
		if !reply.YourIPAddr.IsUnspecified() {
			t.Errorf("reply yiaddr = %s, want 0.0.0.0 for a nak", reply.YourIPAddr)
		}
	})

	t.Run("request for another server is ignored", func(t *testing.T) {
		handler, _ := newDHCPTarget(t)
		if err := handler.AddLease("aa:bb:cc:dd:ee:ff", "net-a", "10.10.10.20", "default/testvm"); err != nil {
			t.Fatalf("failed to seed lease: %s", err)
		}

		request, err := dhcpv4.New(
			dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
			dhcpv4.WithHwAddr(mustHardwareAddr(t, "aa:bb:cc:dd:ee:ff")),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("203.0.113.5"))),
			dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("10.10.10.20"))),
		)
		if err != nil {
			t.Fatalf("failed to build request: %s", err)
		}

		conn := &capturePacketConn{}
		handler.dhcpHandler(conn, &net.UDPAddr{IP: net.ParseIP("10.10.10.20"), Port: 68}, request)

		if len(conn.written) != 0 {
			t.Errorf("got %d writes for a foreign server request, want 0", len(conn.written))
		}
	})
}

func mustHardwareAddr(t *testing.T, hwAddr string) net.HardwareAddr {
	t.Helper()

	hw, err := net.ParseMAC(hwAddr)
	if err != nil {
		t.Fatalf("invalid hardware address %q: %s", hwAddr, err)
	}

	return hw
}
