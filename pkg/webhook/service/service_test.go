package service

import (
	"net/netip"
	"sort"
	"strings"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func vmnetcfg(namespace string, name string, vmName string, nics ...kihv1.NetworkConfig) *kihv1.VirtualMachineNetworkConfig {
	return &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        vmName,
			NetworkConfig: nics,
		},
	}
}

func TestParseAllocationRef(t *testing.T) {
	tests := []struct {
		name      string
		ref       string
		namespace string
		vmName    string
		hwAddr    string
		ok        bool
	}{
		{"canonical reference", "default/cirros-vm1 [02:7b:d9:84:8f:e5]", "default", "cirros-vm1", "02:7b:d9:84:8f:e5", true},
		{"dash spelling is canonicalized", "default/vm-1 [02-7b-d9-84-8f-e5]", "default", "vm-1", "02:7b:d9:84:8f:e5", true},
		{"uppercase spelling is canonicalized", "DEFAULT/vm-1 [02:7B:D9:84:8F:E5]", "DEFAULT", "vm-1", "02:7b:d9:84:8f:e5", true},
		{"reference without a macaddress", "default/vm-1", "", "", "", false},
		{"reference with an unparseable macaddress", "default/vm-1 [not-a-mac]", "", "", "", false},
		{"owner without a namespace", "vmname [02:7b:d9:84:8f:e5]", "", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespace, vmName, hwAddr, ok := parseAllocationRef(tt.ref)

			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}

			if namespace != tt.namespace || vmName != tt.vmName || hwAddr != tt.hwAddr {
				t.Fatalf("parsed (%q, %q, %q), want (%q, %q, %q)", namespace, vmName, hwAddr, tt.namespace, tt.vmName, tt.hwAddr)
			}
		})
	}
}

func TestBuildAllocationOwnerIndex(t *testing.T) {
	list := &kihv1.VirtualMachineNetworkConfigList{
		Items: []kihv1.VirtualMachineNetworkConfig{
			*vmnetcfg("default", "cirros-vm1", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "net-a"},
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e6", NetworkName: "net-b"}),
			*vmnetcfg("default", "other", "vm-2",
				kihv1.NetworkConfig{MACAddress: "02-00-00-00-00-11", NetworkName: "net-a"}),
			*vmnetcfg("default", "broken", "vm-3",
				kihv1.NetworkConfig{MACAddress: "not-a-mac", NetworkName: "net-a"}),
			*vmnetcfg("default", "anonymous", "",
				kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:22", NetworkName: "net-a"}),
		},
	}

	index := buildAllocationOwnerIndex(list)

	if objName, live := index["default/cirros-vm1"]["02:7b:d9:84:8f:e5"]; !live || objName != "cirros-vm1" {
		t.Fatalf("the canonical tuple of cirros-vm1 is not indexed: live=%v objName=%q", live, objName)
	}

	// the dash spelling of the live object must canonicalize to the same key
	if objName, live := index["default/vm-2"]["02:00:00:00:00:11"]; !live || objName != "other" {
		t.Fatalf("the dash-spelled tuple of vm-2 is not indexed canonically: live=%v objName=%q", live, objName)
	}

	if _, live := index["default/vm-3"]["not-a-mac"]; live {
		t.Fatal("an unparseable macaddress must not be indexed")
	}

	if len(index["default/"]) != 0 {
		t.Fatal("an object without a vmname must not be indexed")
	}
}

// TestEvaluateIPPoolRecords covers the orphan-aware deletion gate: only a
// record whose owner tuple is backed by a live object blocks, and every
// unprovably orphaned record blocks.
func TestEvaluateIPPoolRecords(t *testing.T) {
	list := &kihv1.VirtualMachineNetworkConfigList{
		Items: []kihv1.VirtualMachineNetworkConfig{
			*vmnetcfg("default", "cirros-vm1", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "net-a"}),
		},
	}
	index := buildAllocationOwnerIndex(list)

	allocated := map[string]string{
		"192.168.10.63":  "default/cirros-vm1 [02:7b:d9:84:8f:e5]",
		"192.168.10.99":  "default/gone-vm [02:00:00:00:00:33]",
		"192.168.10.100": "EXCLUDED",
		"192.168.10.101": "unparseable",
	}

	blocking, orphaned := evaluateIPPoolRecords(allocated, index, true)

	if len(blocking) != 2 {
		t.Fatalf("blocking = %v, want exactly the live and the unparseable records", blocking)
	}

	if !strings.Contains(blocking[0], "192.168.10.101") || !strings.Contains(blocking[0], "unparseable reference") {
		t.Fatalf("the unparseable record must block and be reported first (sorted by ip): %v", blocking)
	}

	if !strings.Contains(blocking[1], "192.168.10.63") || !strings.Contains(blocking[1], "VirtualMachineNetworkConfig default/cirros-vm1") {
		t.Fatalf("the live record must block and name its recording object: %v", blocking)
	}

	if len(orphaned) != 1 || !strings.Contains(orphaned[0], "192.168.10.99") || !strings.Contains(orphaned[0], "gone-vm") {
		t.Fatalf("the orphaned record must be reported and must not block: %v", orphaned)
	}

	// an unavailable index keeps every non-EXCLUDED record blocking
	blocking, orphaned = evaluateIPPoolRecords(allocated, index, false)

	if len(blocking) != 3 || len(orphaned) != 0 {
		t.Fatalf("with an unavailable index every record must block: blocking=%v orphaned=%v", blocking, orphaned)
	}

	for _, entry := range blocking {
		if strings.Contains(entry, "EXCLUDED") {
			t.Fatalf("an EXCLUDED record must never block: %v", blocking)
		}
	}
}

// TestFindRecordedTuple covers the duplicate (vmname, macaddress) guard:
// the same pair claimed by another object is denied on any network and
// in every spelling of the macaddress (the comparison is canonical,
// R10), while a different vmname, the object itself, and empty
// macaddresses pass.
func TestFindRecordedTuple(t *testing.T) {
	list := &kihv1.VirtualMachineNetworkConfigList{
		Items: []kihv1.VirtualMachineNetworkConfig{
			*vmnetcfg("default", "cirros-vm1", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/public-vlan-1"}),
		},
	}

	tests := []struct {
		name    string
		obj     *kihv1.VirtualMachineNetworkConfig
		denied  bool
		wantSub string
	}{
		{
			"the same vm and mac in another object",
			vmnetcfg("default", "dup-cfg", "cirros-vm1",
				kihv1.NetworkConfig{IPAddress: "192.168.10.150", MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/public-vlan-1"}),
			true,
			"already recorded with macaddress 02:7b:d9:84:8f:e5 by VirtualMachineNetworkConfig default/cirros-vm1",
		},
		{
			"the same vm and mac on a different network",
			vmnetcfg("default", "dup-cfg", "cirros-vm1",
				kihv1.NetworkConfig{IPAddress: "192.168.11.140", MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/public-vlan-2"}),
			true,
			"(network kubevirt-public/public-vlan-1)",
		},
		{
			"the same vm and mac in a dash spelling",
			vmnetcfg("default", "dup-cfg", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02-7b-d9-84-8f-e5", NetworkName: "kubevirt-public/public-vlan-1"}),
			true,
			"already recorded with macaddress 02-7b-d9-84-8f-e5 by VirtualMachineNetworkConfig default/cirros-vm1",
		},
		{
			"the same vm and mac in an uppercase spelling",
			vmnetcfg("default", "dup-cfg", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02:7B:D9:84:8F:E5", NetworkName: "kubevirt-public/public-vlan-2"}),
			true,
			"(network kubevirt-public/public-vlan-1)",
		},
		{
			"a different vmname claiming the same mac stays admissible",
			vmnetcfg("default", "foreign-cfg", "other-vm",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/public-vlan-1"}),
			false,
			"",
		},
		{
			"the object never conflicts with itself",
			vmnetcfg("default", "cirros-vm1", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/public-vlan-1"}),
			false,
			"",
		},
		{
			"an empty macaddress is skipped",
			vmnetcfg("default", "macless-cfg", "cirros-vm1",
				kihv1.NetworkConfig{IPAddress: "192.168.10.150"}),
			false,
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			denied := findRecordedTuple(tt.obj, list)

			if (denied != nil) != tt.denied {
				t.Fatalf("denied = %v, want %v", denied != nil, tt.denied)
			}

			if denied != nil && !strings.Contains(*denied, tt.wantSub) {
				t.Fatalf("message %q does not contain %q", *denied, tt.wantSub)
			}
		})
	}
}

func testPool(name string, networkName string, start string, end string) *kihv1.IPPool {
	return &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kihv1.IPPoolSpec{
			NetworkName: networkName,
			IPv4Config: kihv1.IPv4Config{
				Subnet:   "192.168.11.0/24",
				ServerIP: "192.168.11.9",
				Pool:     kihv1.Pool{Start: start, End: end},
			},
		},
	}
}

// TestCheckNICIPAddress covers the ipaddress range guard of the vmnetcfg
// admission check, including the two deliberate allowances: a network
// without a pool (the vm-before-pool ordering) and a pool whose range does
// not parse (the ippool controller's own rejection).
func TestCheckNICIPAddress(t *testing.T) {
	pool := testPool("public-vlan-2", "kubevirt-public/public-vlan-2", "192.168.11.100", "192.168.11.166")
	brokenRange := testPool("broken-pool", "kubevirt-public/public-vlan-2", "192.168.11.abc", "192.168.11.166")

	tests := []struct {
		name   string
		nc     kihv1.NetworkConfig
		pool   *kihv1.IPPool
		denied bool
	}{
		{"an ip inside the range", kihv1.NetworkConfig{IPAddress: "192.168.11.120", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/public-vlan-2"}, pool, false},
		{"the range start itself", kihv1.NetworkConfig{IPAddress: "192.168.11.100", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/public-vlan-2"}, pool, false},
		{"the range end itself", kihv1.NetworkConfig{IPAddress: "192.168.11.166", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/public-vlan-2"}, pool, false},
		{"an ip above the range", kihv1.NetworkConfig{IPAddress: "192.168.11.200", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/public-vlan-2"}, pool, true},
		{"an ip below the range", kihv1.NetworkConfig{IPAddress: "192.168.11.99", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/public-vlan-2"}, pool, true},
		{"an ip outside the subnet", kihv1.NetworkConfig{IPAddress: "10.0.0.5", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/public-vlan-2"}, pool, true},
		{"a network without a pool", kihv1.NetworkConfig{IPAddress: "10.99.0.10", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/nonexistent"}, nil, false},
		{"a pool whose range does not parse", kihv1.NetworkConfig{IPAddress: "192.168.11.120", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/public-vlan-2"}, brokenRange, false},
		{"an empty ipaddress is skipped", kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/public-vlan-2"}, pool, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			denied := checkNICIPAddress(tt.nc, tt.pool)

			if (denied != nil) != tt.denied {
				t.Fatalf("denied = %v, want %v", denied, tt.denied)
			}
		})
	}
}

// TestEvaluateIPPoolSpec covers the ippool spec guard. every rejected case
// mirrors a bound of the controller's own registration validation, and the
// off-subnet serverip is deliberately allowed because the controller
// registers it.
func TestEvaluateIPPoolSpec(t *testing.T) {
	tests := []struct {
		name         string
		cfg          kihv1.IPv4Config
		wantProblems []string
	}{
		{
			"a valid configuration",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20", Exclude: []string{"192.168.16.15"}}},
			nil,
		},
		{
			"an off-subnet serverip is deliberately allowed",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "10.9.9.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20"}},
			nil,
		},
		{
			"a subnet length the crd schema accepts but the controller cannot register",
			kihv1.IPv4Config{Subnet: "192.168.16.0/33", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20"}},
			[]string{`the subnet "192.168.16.0/33" does not parse as an ipv4 prefix`},
		},
		{
			"a pool start outside the subnet",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.15.255", End: "192.168.16.20"}},
			[]string{"the pool start 192.168.15.255 is not within the subnet 192.168.16.0/24"},
		},
		{
			"a pool end before its start",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.20", End: "192.168.16.10"}},
			[]string{"the pool end 192.168.16.10 lies before the pool start 192.168.16.20"},
		},
		{
			"a pool end equal to the broadcast address",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.255"}},
			[]string{"the pool end 192.168.16.255 equals the broadcast address 192.168.16.255 of the subnet 192.168.16.0/24"},
		},
		{
			"an exclude address outside the pool range",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20", Exclude: []string{"192.168.16.200"}}},
			[]string{"the exclude address 192.168.16.200 is not within the pool range 192.168.16.10..192.168.16.20"},
		},
		{
			"a serverip within the pool range without an exclude entry",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.15", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20"}},
			[]string{"the serverip 192.168.16.15 lies within the pool range 192.168.16.10-192.168.16.20 and is not excluded; move it outside the allocation range or add it to the exclude list"},
		},
		{
			"a router within the pool range without an exclude entry",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Router: "192.168.16.12", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20"}},
			[]string{"the router 192.168.16.12 lies within the pool range 192.168.16.10-192.168.16.20 and is not excluded; move it outside the allocation range or add it to the exclude list"},
		},
		{
			"an in-range serverip with an exclude entry is admitted",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.15", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20", Exclude: []string{"192.168.16.15"}}},
			nil,
		},
		{
			"a serverip inside the subnet but outside the pool range stays admitted",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Router: "192.168.16.254", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20"}},
			nil,
		},
		{
			"an exclude address equal to the broadcast address",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20", Exclude: []string{"192.168.16.255"}}},
			[]string{"the exclude address 192.168.16.255 equals the broadcast address 192.168.16.255 of the subnet 192.168.16.0/24", "the exclude address 192.168.16.255 is not within the pool range 192.168.16.10..192.168.16.20"},
		},
		{
			"a range larger than the cap",
			kihv1.IPv4Config{Subnet: "10.20.0.0/15", ServerIP: "10.20.0.9", Pool: kihv1.Pool{Start: "10.20.0.1", End: "10.21.255.254"}},
			[]string{"the pool range 10.20.0.1 - 10.21.255.254 is larger than the maximum of 65536 addresses"},
		},
		{
			"every problem of a broken projection is reported together and sorted",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "10.9.9.9x", Pool: kihv1.Pool{Start: "192.168.16.20", End: "192.168.16.10"}},
			[]string{"the pool end 192.168.16.10 lies before the pool start 192.168.16.20", `the serverip "10.9.9.9x" does not parse as an ipv4 address`},
		},
		{
			"an empty configuration stays the controller's business",
			kihv1.IPv4Config{},
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := evaluateIPPoolSpec(tt.cfg)

			if len(problems) != len(tt.wantProblems) {
				t.Fatalf("problems = %v, want %v", problems, tt.wantProblems)
			}

			if !sort.StringsAreSorted(problems) {
				t.Fatalf("the problems must be sorted for a deterministic message: %v", problems)
			}

			for i, want := range tt.wantProblems {
				if problems[i] != want {
					t.Fatalf("problems[%d] = %q, want %q", i, problems[i], want)
				}
			}
		})
	}
}

// TestCheckNICMACAddress covers the source-address guard: every multicast
// address and the broadcast address are denied, unicast addresses pass,
// and an empty macaddress is skipped.
func TestCheckNICMACAddress(t *testing.T) {
	tests := []struct {
		name   string
		nc     kihv1.NetworkConfig
		denied bool
	}{
		{"a locally administered unicast address", kihv1.NetworkConfig{MACAddress: "02:7b:d9:00:00:61", NetworkName: "net-a"}, false},
		{"a globally unique unicast address", kihv1.NetworkConfig{MACAddress: "52:54:00:12:34:56", NetworkName: "net-a"}, false},
		{"an ipv4 multicast address", kihv1.NetworkConfig{MACAddress: "01:00:5e:00:00:99", NetworkName: "net-a"}, true},
		{"an odd first octet is multicast", kihv1.NetworkConfig{MACAddress: "03:00:00:00:00:99", NetworkName: "net-a"}, true},
		{"the broadcast address", kihv1.NetworkConfig{MACAddress: "ff:ff:ff:ff:ff:ff", NetworkName: "net-a"}, true},
		{"an empty macaddress is skipped", kihv1.NetworkConfig{IPAddress: "192.168.11.120", NetworkName: "net-a"}, false},
		{"an unparseable macaddress", kihv1.NetworkConfig{MACAddress: "02:7b:d9:00:00", NetworkName: "net-a"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			denied := checkNICMACAddress(tt.nc)

			if (denied != nil) != tt.denied {
				t.Fatalf("denied = %v, want %v", denied, tt.denied)
			}
		})
	}
}

func TestIPv4BroadcastAndRangeLen(t *testing.T) {
	tests := []struct {
		subnet    string
		broadcast string
	}{
		{"192.168.16.0/24", "192.168.16.255"},
		{"10.20.0.0/16", "10.20.255.255"},
		{"10.20.0.0/30", "10.20.0.3"},
		{"192.168.16.0/31", "192.168.16.1"},
		{"192.168.16.128/25", "192.168.16.255"},
	}

	for _, tt := range tests {
		prefix, err := netip.ParsePrefix(tt.subnet)
		if err != nil {
			t.Fatalf("subnet %s does not parse: %s", tt.subnet, err)
		}

		if got := ipv4Broadcast(prefix).String(); got != tt.broadcast {
			t.Fatalf("broadcast of %s = %s, want %s", tt.subnet, got, tt.broadcast)
		}
	}

	start, _ := netip.ParseAddr("192.168.16.10")
	end, _ := netip.ParseAddr("192.168.16.20")
	if got := ipv4RangeLen(start, end); got != 11 {
		t.Fatalf("rangeLen of an 11-address range = %d, want 11", got)
	}

	start, _ = netip.ParseAddr("10.20.0.1")
	end, _ = netip.ParseAddr("10.21.255.254")
	if got := ipv4RangeLen(start, end); got != 131070 {
		t.Fatalf("rangeLen of the oversized range = %d, want 131070", got)
	}
}
