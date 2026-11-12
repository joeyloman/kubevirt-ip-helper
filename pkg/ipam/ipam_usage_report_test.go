package ipam

import (
	"reflect"
	"testing"
)

func TestUsageReportEmptyAllocator(t *testing.T) {
	a := New()

	report := a.UsageReport()
	if report == nil {
		t.Fatal("UsageReport returned nil, want an empty slice")
	}
	if len(report) != 0 {
		t.Fatalf("UsageReport returned %d networks, want 0", len(report))
	}
}

func TestUsageReportSnapshot(t *testing.T) {
	a := New()
	if err := a.NewSubnet("net-b", "10.0.1.0/30", "10.0.1.1", "10.0.1.2"); err != nil {
		t.Fatalf("NewSubnet net-b: %v", err)
	}
	if err := a.NewSubnet("net-a", "192.168.50.0/29", "192.168.50.1", "192.168.50.6"); err != nil {
		t.Fatalf("NewSubnet net-a: %v", err)
	}

	// allocate out of address order so the report has to sort
	if _, err := a.GetIP("net-a", "192.168.50.3"); err != nil {
		t.Fatalf("GetIP 192.168.50.3: %v", err)
	}
	if _, err := a.GetIP("net-a", "192.168.50.1"); err != nil {
		t.Fatalf("GetIP 192.168.50.1: %v", err)
	}

	report := a.UsageReport()
	if len(report) != 2 {
		t.Fatalf("UsageReport returned %d networks, want 2", len(report))
	}
	if report[0].Name != "net-a" || report[1].Name != "net-b" {
		t.Fatalf("networks are not sorted by name: %q, %q", report[0].Name, report[1].Name)
	}

	got := report[0]
	want := SubnetUsage{
		Name:      "net-a",
		CIDR:      "192.168.50.0/29",
		Start:     "192.168.50.1",
		End:       "192.168.50.6",
		Broadcast: "192.168.50.7",
		Total:     6,
		Used:      2,
		Available: 4,
		Allocated: []string{"192.168.50.1", "192.168.50.3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("net-a report = %+v, want %+v", got, want)
	}

	// the report is a snapshot: it must not change when the allocator does
	if _, err := a.GetIP("net-a", "192.168.50.2"); err != nil {
		t.Fatalf("GetIP 192.168.50.2: %v", err)
	}
	if got.Used != 2 || len(got.Allocated) != 2 {
		t.Errorf("report changed after a later allocation: %+v", got)
	}

	if got := a.UsageReport()[0]; got.Used != 3 || got.Available != 3 {
		t.Errorf("fresh report counters = used %d, available %d, want 3, 3", got.Used, got.Available)
	}

	// an unallocated network reports an empty array, never null, so a
	// client can iterate the field unconditionally
	if report[1].Allocated == nil || len(report[1].Allocated) != 0 {
		t.Errorf("net-b allocated = %v, want an empty slice", report[1].Allocated)
	}
}

func TestUsageReportDoesNotLeakOwnerIdentity(t *testing.T) {
	a := New()
	if err := a.NewSubnet("net", "10.0.2.0/30", "10.0.2.1", "10.0.2.2"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}
	if _, err := a.AllocateIP("net", "owner-secret"); err != nil {
		t.Fatalf("AllocateIP: %v", err)
	}

	report := a.UsageReport()
	if len(report) != 1 || len(report[0].Allocated) != 1 {
		t.Fatalf("UsageReport = %+v, want one allocated address", report)
	}
	if report[0].Allocated[0] == "owner-secret" {
		t.Fatal("UsageReport exposed the owner token as an address")
	}
}
