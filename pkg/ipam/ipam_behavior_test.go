package ipam

import (
	"bytes"
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
	}
	for _, tc := range cases {
		if err := a.NewSubnet(tc.name, tc.subnet, tc.start, tc.end); err == nil {
			t.Errorf("NewSubnet(%s) expected error, got nil", tc.name)
		}
	}
}

func TestIPAMNewSubnetOverwrites(t *testing.T) {
	a := New()
	if err := a.NewSubnet("net", "192.168.1.0/30", "192.168.1.1", "192.168.1.2"); err != nil {
		t.Fatalf("first NewSubnet: %v", err)
	}
	// Re-registering the same name replaces the subnet with no error.
	if err := a.NewSubnet("net", "192.168.2.0/30", "192.168.2.1", "192.168.2.2"); err != nil {
		t.Fatalf("second NewSubnet: %v", err)
	}

	if got := a.Used("net"); got != 0 {
		t.Errorf("Used = %d, want 0 after replace", got)
	}
	ip, err := a.GetIP("net", "192.168.2.1")
	if err != nil {
		t.Fatalf("GetIP after replace: %v", err)
	}
	if ip != "192.168.2.1" {
		t.Errorf("got %s, want 192.168.2.1", ip)
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
