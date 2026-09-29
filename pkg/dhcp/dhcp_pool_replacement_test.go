package dhcp

// F07 regression tests at the wire level: replacing a pool's options must
// keep the network continuously served. the replacement resolves its ntp
// hostnames before it takes the allocator lock and swaps the pool entry in
// one atomic step, so a valid request which arrives while the resolution
// is still running must be acked with the old options, a replacement which
// AddPool rejects must leave the old pool answering, and only a published
// replacement changes the served options.

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// requestForLease issues a valid selecting DHCPREQUEST for the fixture
// lease through the packet handler and returns the parsed reply.
func requestForLease(t *testing.T, a *DHCPAllocator) *dhcpv4.DHCPv4 {
	t.Helper()

	conn := &recordingPacketConn{}
	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
	req.UpdateOption(dhcpv4.OptServerIdentifier(net.ParseIP("192.168.0.1")))
	req.UpdateOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("192.168.0.50")))
	a.dhcpHandler("pool1", conn, testPeer(), req)

	if conn.len() != 1 {
		t.Fatalf("expected 1 reply, got %d", conn.len())
	}
	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}

	return resp
}

// TestReplacementKeepsServingWhileTheNTPResolutionBlocks is the review's
// F07 acceptance at the packet level: a valid DHCPREQUEST which arrives
// while the replacement is still resolving must be ACKed with the old
// options, and the same request must be ACKed with the new options once
// the replacement is published.
func TestReplacementKeepsServingWhileTheNTPResolutionBlocks(t *testing.T) {
	a := newTestPooledAllocator(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	a.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			once.Do(func() { close(entered) })
			<-release

			return nil, errors.New("the blocked resolver was released without a connection")
		},
	}

	// the replacement of pool1: same addresses, a hostname ntp entry which
	// parks in the blocked resolution, and a new lease time
	done := make(chan error, 1)
	go func() {
		done <- a.AddPool(
			context.Background(),
			"pool1",
			"192.168.0.1",
			"255.255.255.0",
			"192.168.0.254",
			[]string{"1.1.1.1", "8.8.8.8"},
			"example.com",
			[]string{"example.com"},
			[]string{"replacement-ntp.invalid"},
			120,
			"eth0",
		)
	}()
	<-entered

	// a valid renewal arrives while the replacement is still resolving:
	// it must be acked with the old options, not nacked against a
	// pool-absent registry
	resp := requestForLease(t, a)
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Fatalf("message type = %v while the replacement resolves, want Ack", mt)
	}
	if got := resp.IPAddressLeaseTime(0); got != 3600*time.Second {
		t.Errorf("lease time = %v while the replacement resolves, want the old 3600s", got)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the blocked replacement did not converge: %v", err)
	}

	// the replacement is published: the same request is acked with the
	// new options
	resp = requestForLease(t, a)
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Fatalf("message type = %v after the replacement was published, want Ack", mt)
	}
	if got := resp.IPAddressLeaseTime(0); got != 120*time.Second {
		t.Errorf("lease time = %v after the replacement was published, want the new 120s", got)
	}
}

// TestRejectedReplacementKeepsTheOldPoolServing pins the F07 rejection
// continuity at the packet level: a replacement which fails AddPool's own
// validation takes no state, so the old pool must still answer requests
// with its options.
func TestRejectedReplacementKeepsTheOldPoolServing(t *testing.T) {
	a := newTestPooledAllocator(t)

	// the rejected replacement: a non-literal dns entry fails AddPool's
	// own validation before any state is taken
	if err := a.AddPool(
		context.Background(),
		"pool1",
		"192.168.0.1",
		"255.255.255.0",
		"192.168.0.254",
		[]string{"dns.example.invalid"},
		"example.com",
		[]string{"example.com"},
		[]string{"10.0.0.53"},
		120,
		"eth0",
	); err == nil {
		t.Fatal("AddPool accepted a non-literal dns entry, want the rejection")
	}

	// the old pool must still answer with its options
	if !a.CheckPool("pool1") {
		t.Fatal("the rejected replacement unregistered the serving pool")
	}
	resp := requestForLease(t, a)
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Fatalf("message type = %v after the rejected replacement, want Ack", mt)
	}
	if got := resp.IPAddressLeaseTime(0); got != 3600*time.Second {
		t.Errorf("lease time = %v after the rejected replacement, want the old 3600s", got)
	}
	if got := resp.DNS(); len(got) != 2 || !got[0].Equal(net.ParseIP("1.1.1.1")) || !got[1].Equal(net.ParseIP("8.8.8.8")) {
		t.Errorf("dns = %v after the rejected replacement, want [1.1.1.1 8.8.8.8]", got)
	}
}
