package ipam

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"

	log "github.com/sirupsen/logrus"
)

func mustAddTwoAddressSubnet(t *testing.T, a *IPAllocator, name string) {
	t.Helper()
	if err := a.NewSubnet(name, "192.168.99.0/30", "192.168.99.1", "192.168.99.2"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}
}

func TestIPAMBoundarySingleAddress(t *testing.T) {
	a := New()
	// A /31 pool whose start and end are the same single address. The
	// end must not equal the broadcast address (.65 here), so it is valid.
	if err := a.NewSubnet("one", "192.168.10.64/31", "192.168.10.64", "192.168.10.64"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	ip, err := a.GetIP("one", "")
	if err != nil {
		t.Fatalf("GetIP: %v", err)
	}
	if ip != "192.168.10.64" {
		t.Errorf("got %s, want 192.168.10.64", ip)
	}
	if got := a.Used("one"); got != 1 {
		t.Errorf("Used = %d, want 1", got)
	}

	_, err = a.GetIP("one", "")
	if err == nil {
		t.Fatal("expected exhaustion error for a single-address subnet")
	} else if want := "no more ips left in network one"; err.Error() != want {
		t.Errorf("got error %q, want %q", err, want)
	}
}

func TestIPAMExhaustionAndReuse(t *testing.T) {
	a := New()
	mustAddTwoAddressSubnet(t, a, "net")

	if got := a.Available("net"); got != 2 {
		t.Fatalf("Available = %d, want 2", got)
	}

	seen := make(map[string]bool)
	for i := range 3 {
		ip, err := a.GetIP("net", "")
		if i < 2 {
			if err != nil {
				t.Fatalf("GetIP %d: %v", i, err)
			}
			if seen[ip] {
				t.Errorf("duplicate allocation %s", ip)
			}
			seen[ip] = true
			continue
		}
		if err == nil {
			t.Fatalf("expected exhaustion error, got ip %s", ip)
		} else if want := "no more ips left in network net"; err.Error() != want {
			t.Errorf("got error %q, want %q", err, want)
		}
	}

	if got := a.Used("net"); got != 2 {
		t.Errorf("Used = %d, want 2", got)
	}
	if got := a.Available("net"); got != 0 {
		t.Errorf("Available = %d, want 0", got)
	}

	// Releasing one address makes it available again; the next allocation
	// must hand out the released address (it is the only free one).
	if err := a.ReleaseIP("net", "192.168.99.1"); err != nil {
		t.Fatalf("ReleaseIP: %v", err)
	}
	if got := a.Used("net"); got != 1 {
		t.Errorf("Used after release = %d, want 1", got)
	}
	ip, err := a.GetIP("net", "")
	if err != nil {
		t.Fatalf("GetIP after release: %v", err)
	}
	if ip != "192.168.99.1" {
		t.Errorf("released ip was not reused: got %s", ip)
	}
}

func TestIPAMGivenIPOutsidePoolRange(t *testing.T) {
	a := New()
	mustAddTwoAddressSubnet(t, a, "net")

	// The network address is within the cidr and is not the broadcast,
	// but is outside the allocated pool range, so allocation fails with
	// the exhaustion error.
	if _, err := a.GetIP("net", "192.168.99.0"); err == nil {
		t.Fatal("expected error for in-cidr but out-of-pool address")
	} else if want := "no more ips left in network net"; err.Error() != want {
		t.Errorf("got error %q, want %q", err, want)
	}
}

func TestIPAMGivenIPInvalidSyntax(t *testing.T) {
	a := New()
	mustAddTwoAddressSubnet(t, a, "net")

	if _, err := a.GetIP("net", "not-an-ip"); err == nil {
		t.Fatal("expected parse error for invalid given ip")
	}
	if err := a.ReleaseIP("net", "not-an-ip"); err == nil {
		t.Fatal("expected parse error for invalid release ip")
	}
}

func TestIPAMNewSubnetValidationErrors(t *testing.T) {
	a := New()
	cases := []struct {
		name   string
		subnet string
		start  string
		end    string
	}{
		{"bad-cidr", "not-a-cidr", "10.0.0.1", "10.0.0.2"},
		{"bad-start", "10.0.0.0/24", "not-an-ip", "10.0.0.2"},
		{"bad-end", "10.0.0.0/24", "10.0.0.1", "not-an-ip"},
		{"start-outside", "10.0.0.0/24", "10.0.1.1", "10.0.1.2"},
		{"end-outside", "10.0.0.0/24", "10.0.0.1", "10.0.1.2"},
		{"reversed-range", "10.0.0.0/24", "10.0.0.50", "10.0.0.1"},
		{"broadcast-end", "10.0.0.0/29", "10.0.0.1", "10.0.0.7"},
		// an ipv6 prefix with <= 32 bits is not caught by a bits-only
		// family gate; it must still be classified as unregistrable before
		// the 16-byte address reaches the broadcast computation
		{"ipv6-prefix", "2001:db8::/32", "2001:db8::1", "2001:db8::2"},
		{"ipv6-prefix-zero-bits", "2001:db8::/0", "2001:db8::1", "2001:db8::2"},
	}
	for _, tc := range cases {
		err := a.NewSubnet(tc.name, tc.subnet, tc.start, tc.end)
		if err == nil {
			t.Errorf("NewSubnet(%s) expected error, got nil", tc.name)

			continue
		}
		if !errors.Is(err, ErrSubnetInvalid) {
			t.Errorf("NewSubnet(%s) = %v, want the ErrSubnetInvalid classification", tc.name, err)
		}
	}
}

// A repeated subnet name must be rejected while keeping the allocation
// bitmap of the original registration. Silent replacement drops live
// addresses from accounting, so they could be reissued to other clients.
func TestIPAMRejectsDuplicateSubnetName(t *testing.T) {
	a := New()
	if err := a.NewSubnet("net", "192.168.1.0/30", "192.168.1.1", "192.168.1.2"); err != nil {
		t.Fatalf("first NewSubnet: %v", err)
	}
	occupied, err := a.GetIP("net", "192.168.1.1")
	if err != nil {
		t.Fatalf("GetIP to occupy an address: %v", err)
	}
	// Re-registering the same name must fail with the state kept.
	if err := a.NewSubnet("net", "192.168.2.0/30", "192.168.2.1", "192.168.2.2"); err == nil {
		t.Fatal("second NewSubnet must be rejected as duplicate")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate error = %q, want already-exists message", err)
	} else if errors.Is(err, ErrSubnetInvalid) {
		t.Errorf("duplicate conflict = %v, must stay a retryable plain error (not ErrSubnetInvalid)", err)
	}

	if occupied != "192.168.1.1" {
		t.Errorf("occupied = %s, want the first allocation to remain", occupied)
	}
	if _, err := a.GetIP("net", "192.168.1.1"); !strings.Contains(err.Error(), "already allocated") {
		t.Errorf("original allocation must still be held, got %q", err)
	}

	// repeated auto-allocation must not reissue the occupied address
	followUpIP, err := a.GetIP("net", "")
	if err != nil {
		t.Fatalf("allocation after the rejected duplicate failed: %v", err)
	}
	if followUpIP == "192.168.1.1" {
		t.Errorf("allocation %q reissued the already occupied first address", followUpIP)
	}
	if got := a.Used("net"); got != 2 {
		t.Errorf("Used = %d, want 2 (the original allocation is kept, none added)", got)
	}
}

func TestIPAMUnknownNetworkCounts(t *testing.T) {
	a := New()

	if got := a.Used("missing"); got != 0 {
		t.Errorf("Used = %d, want 0", got)
	}
	if got := a.Available("missing"); got != 0 {
		t.Errorf("Available = %d, want 0", got)
	}
	a.DeleteSubnet("missing") // must not panic
}

func TestIPAMDeleteSubnetRemovesState(t *testing.T) {
	a := New()
	mustAddTwoAddressSubnet(t, a, "net")
	if _, err := a.GetIP("net", ""); err != nil {
		t.Fatalf("GetIP: %v", err)
	}

	a.DeleteSubnet("net")

	if err := a.ReleaseIP("net", "192.168.99.1"); err == nil {
		t.Fatal("expected error after subnet delete")
	}
	if _, err := a.GetIP("net", ""); err == nil {
		t.Fatal("expected error after subnet delete")
	}
}

func TestIPAMConcurrentAllocations(t *testing.T) {
	a := New()
	if err := a.NewSubnet("net", "192.168.50.0/28", "192.168.50.1", "192.168.50.14"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	const workers = 14
	var wg sync.WaitGroup
	got := make([]string, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = a.GetIP("net", "")
		}(i)
	}
	wg.Wait()

	prefix := netip.MustParsePrefix("192.168.50.0/28")
	seen := make(map[string]bool)
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if seen[got[i]] {
			t.Errorf("duplicate allocation %s", got[i])
		}
		seen[got[i]] = true
		addr, err := netip.ParseAddr(got[i])
		if err != nil || !prefix.Contains(addr) {
			t.Errorf("allocation %q is not in pool %s", got[i], prefix)
		}
		if addr == netip.MustParseAddr("192.168.50.15") {
			t.Errorf("allocation %q is the broadcast address", got[i])
		}
	}
	if used := a.Used("net"); used != workers {
		t.Errorf("Used = %d, want %d", used, workers)
	}
	if avail := a.Available("net"); avail != 0 {
		t.Errorf("Available = %d, want 0", avail)
	}
}

// the ippool reload and the vmnetcfg reconcile run on parallel workqueues:
// the writer below models a reload deleting and recreating the subnet while
// the reader workers model concurrent allocations and status counter reads.
// every call path must share the subnet map safely, so this test panics
// under -race (and crashes the test binary on an unsynchronized map) if a
// call path is missing its lock. no result value is asserted: all lookup
// failures are legitimate outcomes of the concurrent delete.
func TestIPAMConcurrentSubnetLifecycleWithReaders(t *testing.T) {
	a := New()
	if err := a.NewSubnet("net", "192.168.51.0/27", "192.168.51.1", "192.168.51.30"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	level := log.GetLevel()
	log.SetLevel(log.PanicLevel)
	defer log.SetLevel(level)

	const workers = 8
	const rounds = 50

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			a.DeleteSubnet("net")

			if err := a.NewSubnet("net", "192.168.51.0/27", "192.168.51.1", "192.168.51.30"); err != nil {
				t.Errorf("recreating the subnet: %v", err)

				return
			}
		}
	}()

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				ip, err := a.GetIP("net", "192.168.51.5")
				if err == nil {
					_ = a.ReleaseIP("net", ip)
				}
				_, _ = a.GetIP("net", "")
				_ = a.Used("net")
				_ = a.Available("net")
				a.Usage("net")
			}
		}()
	}

	wg.Wait()
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

func TestIPAMUsageLogsSubnetAndAllocations(t *testing.T) {
	a := New()
	if err := a.NewSubnet("net1", "192.168.99.0/30", "192.168.99.1", "192.168.99.2"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}
	ip, err := a.GetIP("net1", "")
	if err != nil {
		t.Fatalf("GetIP: %v", err)
	}

	out := captureLogrus(t, func() { a.Usage("net1") })

	for _, want := range []string{
		"(ipam.Usage)",
		"cidr=192.168.99.0/30",
		"start=192.168.99.1",
		"end=192.168.99.2",
		"allocated ips:",
		"- " + ip,
		"ipsinpool=2",
		"usedips=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Usage log missing %q", want)
		}
	}
}

func TestIPAMUsageUnknownNetworkLogsWarning(t *testing.T) {
	a := New()

	out := captureLogrus(t, func() { a.Usage("ghost") })

	if !strings.Contains(out, "(ipam.Usage) network ghost does not exists") {
		t.Errorf("Usage log missing warning for unknown network: %s", out)
	}
}

func TestIPAMNewConstructorAlias(t *testing.T) {
	a := New()
	if err := a.NewSubnet("alias", "10.0.3.0/30", "10.0.3.1", "10.0.3.2"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	ip, err := a.GetIP("alias", "")
	if err != nil {
		t.Fatalf("GetIP: %v", err)
	}
	if ip != "10.0.3.1" && ip != "10.0.3.2" {
		t.Errorf("allocated ip = %s, want one of 10.0.3.1, 10.0.3.2", ip)
	}
	if got := a.Used("alias"); got != 1 {
		t.Errorf("Used = %d, want 1", got)
	}
}

// IPOwnedBy resolves the reservation adoption of the vmnetcfg reconcile
// flow: it reports the live claim of exactly the given allocation
// reference, so a binding can adopt its own held reservation instead of
// allocating a second address while the first stays claimed.
func TestIPAMIPOwnedByFindsOnlyTheOwnersLiveClaim(t *testing.T) {
	a := New()
	mustAddTwoAddressSubnet(t, a, "net")

	owner := "ns/vm [02:00:00:00:00:01]"
	foreign := "ns/vm2 [02:00:00:00:00:02]"

	// a claimless owner, an empty owner and an unknown network find nothing
	if ip, found := a.IPOwnedBy("net", owner); found {
		t.Errorf("IPOwnedBy = %s, want not found for a claimless owner", ip)
	}
	if _, found := a.IPOwnedBy("net", ""); found {
		t.Error("an empty owner must never match a claim")
	}
	if _, found := a.IPOwnedBy("missing", owner); found {
		t.Error("an unknown network must never report a claim")
	}

	// the live claim of the owner is found, the foreign owner's is not
	first, err := a.AllocateIP("net", owner)
	if err != nil {
		t.Fatalf("AllocateIP: %v", err)
	}
	if _, err := a.AllocateIP("net", foreign); err != nil {
		t.Fatalf("AllocateIP: %v", err)
	}
	if ip, found := a.IPOwnedBy("net", owner); !found || ip != first {
		t.Errorf("IPOwnedBy = %s (found %v), want the owner's claim %s", ip, found, first)
	}

	// a released claim is gone: the adoption never resurrects it
	if err := a.ReleaseIPOwnedBy("net", first, owner); err != nil {
		t.Fatalf("ReleaseIPOwnedBy: %v", err)
	}
	if ip, found := a.IPOwnedBy("net", owner); found {
		t.Errorf("IPOwnedBy = %s, want not found after the release", ip)
	}
}

// a stale state which ever holds two claims of one owner in one network
// must resolve deterministically instead of following the map iteration
// order: the lowest address wins.
func TestIPAMIPOwnedByIsDeterministicAcrossMultipleClaims(t *testing.T) {
	a := New()
	mustAddTwoAddressSubnet(t, a, "net")

	owner := "ns/vm [02:00:00:00:00:01]"
	for _, ip := range []string{"192.168.99.1", "192.168.99.2"} {
		if _, err := a.ReclaimIP("net", ip, owner); err != nil {
			t.Fatalf("ReclaimIP(%s): %v", ip, err)
		}
	}

	for range 10 {
		if ip, found := a.IPOwnedBy("net", owner); !found || ip != "192.168.99.1" {
			t.Fatalf("IPOwnedBy = %s (found %v), want the lowest claimed address 192.168.99.1", ip, found)
		}
	}
}

// HasSubnet gates the persisted pool counter recomputation (F04): the
// counters may only be recomputed from the in-memory state while the
// network is registered in the allocator, because Used and Available of
// an unknown network report zero.
func TestIPAMHasSubnetTracksRegistration(t *testing.T) {
	a := New()

	if a.HasSubnet("net") {
		t.Error("an unknown network must not report a subnet")
	}

	mustAddTwoAddressSubnet(t, a, "net")

	if !a.HasSubnet("net") {
		t.Error("a registered network must report its subnet")
	}

	a.DeleteSubnet("net")

	if a.HasSubnet("net") {
		t.Error("a deleted network must not report a subnet anymore")
	}
}
