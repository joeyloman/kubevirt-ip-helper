package dhcp

// F11 regression tests: the packet handler resolved the inform pool by
// scanning every registered pool for the client address, and it resolved
// the lease of a known client without checking which network's listener
// received the packet. two isolated interfaces may legitimately serve
// the same subnet: once the second pool was registered, an inform of the
// shared subnet matched both pools globally and was dropped on either
// listener - the handler had discarded the ingress network identity that
// could disambiguate it, and adding, deleting or reloading one network's
// pool changed what the other network's listener answered.
//
// The listener callback of Run captures its registered network identity
// and every resolution happens inside that network: the receiving
// network's pool answers an inform of its own subnet, a client lease of
// another network is not the receiving network's client, and an address
// the receiving pool does not serve stays unanswered.

import (
	"context"
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// informScopeFixture installs the two isolated same-subnet pools of the
// review scenario: net-a serves 192.168.0.0/24 on eth0 with its options,
// net-b serves the same subnet on eth1 with different options, so the
// numeric client address cannot tell the two networks apart.
func informScopeFixture(t *testing.T) *DHCPAllocator {
	t.Helper()

	a := NewDHCPAllocator()
	if err := a.AddPool(
		context.Background(),
		"net-a",
		"192.168.0.1",
		"255.255.255.0",
		"192.168.0.254",
		[]string{"1.1.1.1"},
		"example.com",
		[]string{"example.com"},
		nil,
		3600,
		"eth0",
	); err != nil {
		t.Fatalf("AddPool net-a: %v", err)
	}
	if err := a.AddPool(
		context.Background(),
		"net-b",
		"192.168.0.2",
		"255.255.255.0",
		"",
		nil,
		"",
		nil,
		nil,
		3600,
		"eth1",
	); err != nil {
		t.Fatalf("AddPool net-b: %v", err)
	}

	return a
}

// informAckFor drives the same DHCPINFORM of the shared subnet through
// the callback of the named receiving network and returns the parsed
// reply.
func informAckFor(t *testing.T, a *DHCPAllocator, networkName string) *dhcpv4.DHCPv4 {
	t.Helper()

	conn := &recordingPacketConn{}
	req := newBootRequest(t, mustHWAddr(t, "00:11:22:33:44:55"), dhcpv4.MessageTypeInform)
	req.ClientIPAddr = net.ParseIP("192.168.0.50")
	a.dhcpHandler(networkName, conn, testPeer(), req)

	if conn.len() != 1 {
		t.Fatalf("network %s: expected 1 inform ack, got %d", networkName, conn.len())
	}

	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("network %s: parsing reply: %v", networkName, err)
	}

	return resp
}

// TestInformAnswersTheReceivingNetworksConfiguration: the same inform of
// the shared subnet is delivered to both networks' callbacks, and each
// answers with its own configuration - the server identifier, the dns
// servers and the domain name of its own pool - instead of being dropped
// as globally ambiguous or answered with the other network's options.
func TestInformAnswersTheReceivingNetworksConfiguration(t *testing.T) {
	a := informScopeFixture(t)

	respA := informAckFor(t, a, "net-a")
	if mt := respA.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Errorf("net-a: got message type %v, want Ack", mt)
	}
	if sid := respA.ServerIdentifier(); sid == nil || !sid.Equal(net.ParseIP("192.168.0.1")) {
		t.Errorf("net-a: server identifier = %v, want its own pool server 192.168.0.1", sid)
	}
	if respA.GetOneOption(dhcpv4.OptionDomainNameServer) == nil {
		t.Error("net-a: the inform ack must carry its own pool's dns servers")
	}
	if respA.GetOneOption(dhcpv4.OptionDomainName) == nil {
		t.Error("net-a: the inform ack must carry its own pool's domain name example.com")
	}

	respB := informAckFor(t, a, "net-b")
	if mt := respB.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Errorf("net-b: got message type %v, want Ack", mt)
	}
	if sid := respB.ServerIdentifier(); sid == nil || !sid.Equal(net.ParseIP("192.168.0.2")) {
		t.Errorf("net-b: server identifier = %v, want its own pool server 192.168.0.2", sid)
	}
	if respB.GetOneOption(dhcpv4.OptionDomainNameServer) != nil {
		t.Error("net-b: the inform ack carries dns servers, want net-b's own unset options")
	}
	if respB.GetOneOption(dhcpv4.OptionDomainName) != nil {
		t.Error("net-b: the inform ack carries a domain name, want net-b's own unset options")
	}
}

// TestReceivingNetworkAnswerIgnoresTheOtherPoolsLifetime: the answer of
// one network's callback must not depend on the other same-subnet pool.
// pre-fix the reply changed with the other pool's mere existence (the
// inform was dropped while both pools were registered); deleting the
// other pool must leave the answer unchanged.
func TestReceivingNetworkAnswerIgnoresTheOtherPoolsLifetime(t *testing.T) {
	a := informScopeFixture(t)

	before := informAckFor(t, a, "net-a")

	if err := a.DeletePool("net-b"); err != nil {
		t.Fatalf("DeletePool net-b: %v", err)
	}

	after := informAckFor(t, a, "net-a")

	if sid := after.ServerIdentifier(); sid == nil || !sid.Equal(net.ParseIP("192.168.0.1")) {
		t.Errorf("server identifier after the other pool's deletion = %v, want the unchanged 192.168.0.1", sid)
	}
	if (after.GetOneOption(dhcpv4.OptionDomainNameServer) == nil) != (before.GetOneOption(dhcpv4.OptionDomainNameServer) == nil) {
		t.Error("the dns option changed with the other pool's deletion")
	}
	if (after.GetOneOption(dhcpv4.OptionDomainName) == nil) != (before.GetOneOption(dhcpv4.OptionDomainName) == nil) {
		t.Error("the domain name option changed with the other pool's deletion")
	}
}

// TestLeaseOfAnotherNetworkIsNotTheReceivingNetworksClient: a known
// client of net-b sends its discovery through net-a's callback. pre-fix
// the global lease lookup answered it with net-b's pool configuration on
// net-a's listener; the receiving network does not know this client, so
// its callback stays silent and net-b's own callback offers the lease.
func TestLeaseOfAnotherNetworkIsNotTheReceivingNetworksClient(t *testing.T) {
	a := informScopeFixture(t)
	if err := a.AddLease("aa:bb:cc:dd:ee:02", "net-b", "192.168.0.60", "ref-b"); err != nil {
		t.Fatalf("AddLease: %v", err)
	}

	// the receiving network net-a does not hold this client's lease
	connA := &recordingPacketConn{}
	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:02"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler("net-a", connA, testPeer(), req)
	if connA.len() != 0 {
		t.Errorf("net-a answered a client of net-b: got %d replies, want 0", connA.len())
	}

	// net-b's own callback offers the lease of its own client
	connB := &recordingPacketConn{}
	req = newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:02"), dhcpv4.MessageTypeDiscover)
	a.dhcpHandler("net-b", connB, testPeer(), req)
	if connB.len() != 1 {
		t.Fatalf("net-b: expected its own client's discovery to be offered, got %d", connB.len())
	}
	resp, err := dhcpv4.FromBytes(connB.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeOffer {
		t.Errorf("net-b: got message type %v, want Offer", mt)
	}
	if !resp.YourIPAddr.Equal(net.ParseIP("192.168.0.60")) {
		t.Errorf("net-b: offer yiaddr = %s, want the lease address 192.168.0.60", resp.YourIPAddr)
	}
}

// TestInformOfAnUnservedAddressIsDroppedInTheReceivingNetwork keeps the
// fail-closed outcome of the scoped resolution: an address the receiving
// pool's own subnet does not contain is not answered.
func TestInformOfAnUnservedAddressIsDroppedInTheReceivingNetwork(t *testing.T) {
	a := informScopeFixture(t)
	conn := &recordingPacketConn{}

	req := newBootRequest(t, mustHWAddr(t, "00:11:22:33:44:55"), dhcpv4.MessageTypeInform)
	req.ClientIPAddr = net.ParseIP("10.99.0.9")
	a.dhcpHandler("net-a", conn, testPeer(), req)

	if conn.len() != 0 {
		t.Errorf("expected no reply for an inform the receiving pool does not serve, got %d", conn.len())
	}
}

// TestRelayedInformIsAnsweredByTheRelayedNetwork pins the relay policy of
// the scoped resolution: a bootp relay agent unicasts its clients'
// informs to the server address of the network it relays for, and that
// address lives on the network's bind interface, so the packet arrives
// on that network's listener. the inform of the receiving network's
// subnet is answered with the receiving network's options, unicast to
// the relay agent (rfc 2131 4.1).
func TestRelayedInformIsAnsweredByTheRelayedNetwork(t *testing.T) {
	a := informScopeFixture(t)
	conn := &recordingPacketConn{}
	relayPeer := &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: dhcpv4.ClientPort}

	req := newBootRequest(t, mustHWAddr(t, "00:11:22:33:44:55"), dhcpv4.MessageTypeInform)
	req.ClientIPAddr = net.ParseIP("192.168.0.50")
	req.GatewayIPAddr = net.ParseIP("192.168.0.250")
	req.Flags = 0
	a.dhcpHandler("net-a", conn, relayPeer, req)

	if conn.len() != 1 {
		t.Fatalf("expected 1 relayed inform ack, got %d", conn.len())
	}
	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Errorf("got message type %v, want Ack", mt)
	}
	if sid := resp.ServerIdentifier(); sid == nil || !sid.Equal(net.ParseIP("192.168.0.1")) {
		t.Errorf("server identifier = %v, want the receiving network's pool server 192.168.0.1", sid)
	}
	if resp.GetOneOption(dhcpv4.OptionDomainNameServer) == nil {
		t.Error("the relayed inform ack must carry the receiving network's dns servers")
	}
	dst, ok := conn.peers[0].(*net.UDPAddr)
	if !ok || !dst.IP.Equal(net.ParseIP("192.168.0.250")) || dst.Port != dhcpv4.ServerPort {
		t.Errorf("ack destination = %v, want the relay agent 192.168.0.250:%d (giaddr)", conn.peers[0], dhcpv4.ServerPort)
	}
}

// TestRelayedInformOfAnotherSegmentsSubnetIsDropped: an inform relayed
// for a client of a subnet the receiving network does not serve stays
// unanswered. pre-fix the global numeric resolution answered it with
// whichever registered pool contained the address - the exact
// cross-network choice by numeric address F11 forbids, reachable again
// through the relay path. the relay must target the server address of
// the network it relays for, whose listener then receives the packet.
func TestRelayedInformOfAnotherSegmentsSubnetIsDropped(t *testing.T) {
	a := informScopeFixture(t)
	// a third registered network serves its own subnet on its own
	// interface: pre-fix an inform of its addresses was answered
	// through any listener
	if err := a.AddPool(
		context.Background(),
		"net-c",
		"10.99.0.1",
		"255.255.255.0",
		"",
		nil,
		"",
		nil,
		nil,
		3600,
		"eth2",
	); err != nil {
		t.Fatalf("AddPool net-c: %v", err)
	}
	conn := &recordingPacketConn{}
	relayPeer := &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: dhcpv4.ClientPort}

	req := newBootRequest(t, mustHWAddr(t, "00:11:22:33:44:55"), dhcpv4.MessageTypeInform)
	req.ClientIPAddr = net.ParseIP("10.99.0.9")
	req.GatewayIPAddr = net.ParseIP("10.99.0.250")
	req.Flags = 0
	a.dhcpHandler("net-a", conn, relayPeer, req)

	if conn.len() != 0 {
		t.Errorf("expected no reply for a relayed inform of a subnet the receiving network does not serve, got %d", conn.len())
	}
}
