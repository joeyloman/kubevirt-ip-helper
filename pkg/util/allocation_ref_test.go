package util

import "testing"

func TestAllocationRefRoundTrip(t *testing.T) {
	const ref = "default/vm-test [02:00:00:00:00:01]"

	if got := AllocationRef("default", "vm-test", "02:00:00:00:00:01"); got != ref {
		t.Errorf("AllocationRef = %q, want %q", got, ref)
	}

	// a non-canonical but parseable mac spelling normalizes to the
	// canonical colon form
	if got := AllocationRef("default", "vm-test", "02-00-00-00-00-01"); got != ref {
		t.Errorf("AllocationRef with a non-canonical mac = %q, want %q", got, ref)
	}

	ns, vm, hw, ok := ParseAllocationRef(ref)
	if !ok {
		t.Fatal("ParseAllocationRef rejected the canonical reference")
	}
	if ns != "default" || vm != "vm-test" || hw != "02:00:00:00:00:01" {
		t.Errorf("ParseAllocationRef = %q/%q/%q, want default/vm-test/02:00:00:00:00:01", ns, vm, hw)
	}
}

func TestParseAllocationRefRejectsGarbage(t *testing.T) {
	for _, ref := range []string{
		"",
		"EXCLUDED",
		"USED",
		"no-slash",
		"/missing-namespace [02:00:00:00:00:01]",
		"default/ [02:00:00:00:00:01]",
		"default/vm []",
		"default/vm [not a mac!]",
		"default/vm [02:00:00:00:00:01",
		"bracket-open-only [abc",
	} {
		if _, _, _, ok := ParseAllocationRef(ref); ok {
			t.Errorf("ParseAllocationRef(%q) = ok, want rejection", ref)
		}
	}
}
