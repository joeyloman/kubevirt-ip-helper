package dhcp

import (
	"bytes"
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
	if !resp.ClientIPAddr.Equal(net.ParseIP("192.168.0.50")) {
		t.Errorf("ClientIPAddr = %s, want 192.168.0.50", resp.ClientIPAddr)
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

func TestDHCPHandlerAckForRequest(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
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
	if !resp.YourIPAddr.Equal(net.ParseIP("192.168.0.50")) {
		t.Errorf("YourIPAddr = %s, want 192.168.0.50", resp.YourIPAddr)
	}
}

func TestDHCPHandlerReplyForRelease(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRelease)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 1 {
		t.Fatalf("expected 1 reply for DHCPRELEASE, got %d", conn.len())
	}
	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	// The RELEASE branch is informational: the server still answers with
	// the lease-based reply, but leaves the DHCP message type unset
	// instead of turning it into an OFFER or ACK.
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeNone {
		t.Errorf("got message type %v, want none for DHCPRELEASE", mt)
	}
	if resp.OpCode != dhcpv4.OpcodeBootReply {
		t.Errorf("reply opcode = %v, want %v", resp.OpCode, dhcpv4.OpcodeBootReply)
	}
	if !resp.YourIPAddr.Equal(net.ParseIP("192.168.0.50")) {
		t.Errorf("YourIPAddr = %s, want 192.168.0.50", resp.YourIPAddr)
	}
	if resp.TransactionID != req.TransactionID {
		t.Errorf("transaction id = %s, want %s", resp.TransactionID, req.TransactionID)
	}
	if sip := resp.ServerIdentifier(); !sip.Equal(net.ParseIP("192.168.0.1")) {
		t.Errorf("server identifier = %s, want 192.168.0.1", sip)
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
		t.Errorf("expected no reply for a BootReply opcode, got %d", conn.len())
	}
}

func TestDHCPHandlerWriteErrorNoPanic(t *testing.T) {
	a := newTestPooledAllocator(t)
	conn := &recordingPacketConn{writeErr: errors.New("connection reset")}

	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler(conn, testPeer(), req)

	if conn.len() != 1 {
		t.Errorf("expected 1 attempted reply, got %d", conn.len())
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

func TestAddLeaseKeysByProvidedString(t *testing.T) {
	a := New()
	for _, hw := range []string{"aa:bb:cc:dd:ee:01", "aabbccddee01"} {
		if err := a.AddLease(hw, "pool1", "192.168.0.50", "ref"); err != nil {
			t.Fatalf("AddLease(%q): %v", hw, err)
		}
	}

	// Both textual forms are valid MACs but each is stored under the exact
	// string that was supplied.
	if !a.CheckLease("aa:bb:cc:dd:ee:01") {
		t.Error("colon-form key not found")
	}
	if !a.CheckLease("aabbccddee01") {
		t.Error("bare-form key not found")
	}
	lease := a.GetLease("aa:bb:cc:dd:ee:01")
	if !lease.ClientIP.Equal(net.ParseIP("192.168.0.50")) {
		t.Errorf("lease client ip = %v, want 192.168.0.50", lease.ClientIP)
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

func TestAddPoolResolvesNTPHostnameToIPv4(t *testing.T) {
	a := New()
	if err := a.AddPool("p", "192.168.0.1", "255.255.255.0", "192.168.0.254", nil, "", nil, []string{"localhost"}, 3600, "eth0"); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	pool := a.GetPool("p")
	if len(pool.NTP) != 1 {
		t.Fatalf("NTP entries = %d, want 1 (resolved localhost)", len(pool.NTP))
	}
	if !pool.NTP[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("NTP = %v, want 127.0.0.1", pool.NTP)
	}
}

func TestAddPoolUnresolvableNTPHostnameSkipped(t *testing.T) {
	a := New()
	if err := a.AddPool("p", "192.168.0.1", "255.255.255.0", "192.168.0.254", nil, "", nil, []string{"not a valid hostname"}, 3600, "eth0"); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	pool := a.GetPool("p")
	if len(pool.NTP) != 0 {
		t.Errorf("NTP entries = %v, want none for unresolvable hostname", pool.NTP)
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
