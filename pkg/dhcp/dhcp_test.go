package dhcp

import (
	"fmt"
	"net"
	"testing"
)

func TestDHCP(t *testing.T) {
	td := New()

	testPools := []struct {
		name         string
		serverIP     string
		subnetMask   string
		routerIP     string
		DNSServers   []string
		domainName   string
		domainSearch []string
		NTPServers   []string
		leaseTime    int
		want         error
	}{
		{
			name:         "vlan1",
			serverIP:     "0.0.0.0",
			subnetMask:   "255.255.255.0",
			routerIP:     "192.168.0.254",
			DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
			domainName:   "example.com",
			domainSearch: []string{"example.com"},
			NTPServers:   []string{"localhost", "127.0.0.2"},
			leaseTime:    300,
			want:         nil,
		},
		{
			name:         "vlan2",
			serverIP:     "0.0.0.0",
			subnetMask:   "255.255.0.0",
			routerIP:     "172.16.0.254",
			DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
			domainName:   "example.com",
			domainSearch: []string{"example.com"},
			NTPServers:   []string{},
			leaseTime:    1800,
			want:         nil,
		},
		{
			name:         "vlan3",
			serverIP:     "0.0.0.0",
			subnetMask:   "255.0.0.0",
			routerIP:     "10.0.0.254",
			DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
			domainName:   "example.com",
			domainSearch: []string{"example.com"},
			NTPServers:   []string{},
			leaseTime:    0,
			want:         nil,
		},
	}

	// AddPool function tests
	for i := 0; i < len(testPools); i++ {
		if got := td.AddPool(
			testPools[i].name,
			testPools[i].serverIP,
			testPools[i].subnetMask,
			testPools[i].routerIP,
			testPools[i].DNSServers,
			testPools[i].domainName,
			testPools[i].domainSearch,
			testPools[i].NTPServers,
			testPools[i].leaseTime,
		); got != testPools[i].want {
			if got == nil || testPools[i].want == nil {
				t.Errorf("got %q, wanted %q", got, testPools[i].want)
			} else if got.Error() != testPools[i].want.Error() {
				t.Errorf("got %q, wanted %q", got, testPools[i].want)
			}
		}
	}

	// GetPool function tests
	pool1 := td.GetPool("vlan2")
	if !pool1.Router.Equal(net.ParseIP(testPools[1].routerIP)) {
		t.Errorf("got %q, wanted %q", pool1.Router.String(), testPools[1].routerIP)
	}
	pool2 := td.GetPool("vlan10")
	if len(pool2.ServerIP) > 0 {
		t.Errorf("got %q, wanted nil", pool2.ServerIP.String())
	}

	// CheckPool function tests
	if !td.CheckPool("vlan1") {
		t.Errorf("got false, wanted true for poolname vlan1")
	}
	if td.CheckPool("vlan10") {
		t.Errorf("got true, wanted false for poolname vlan10")
	}

	testLeases := []struct {
		hwAddr    string
		poolName  string
		clientIP  string
		Reference string
		want      error
	}{
		{
			hwAddr:    "aa:bb:cc:dd:ee:ff",
			poolName:  "vlan1",
			clientIP:  "192.168.0.10",
			Reference: "",
			want:      nil,
		},
		{
			hwAddr:    "aa:bb:cc:dd:ee:ff",
			poolName:  "vlan1",
			clientIP:  "192.168.0.10",
			Reference: "",
			want:      fmt.Errorf("lease for hwaddr aa:bb:cc:dd:ee:ff already exists"),
		},
		{
			hwAddr:    "00:01:02:03:04:05",
			poolName:  "vlan1",
			clientIP:  "192.168.0.11",
			Reference: "someref",
			want:      nil,
		},
		{
			hwAddr:    "01:02:03:04:05:06",
			poolName:  "vlan1",
			clientIP:  "",
			Reference: "",
			want:      nil,
		},
		{
			hwAddr:    "ZZ:01:02:03:04:05",
			poolName:  "vlan2",
			clientIP:  "",
			Reference: "",
			want:      fmt.Errorf("hwaddr ZZ:01:02:03:04:05 is not valid"),
		},
		{
			hwAddr:    "00-01:02:03:04:05",
			poolName:  "vlan2",
			clientIP:  "",
			Reference: "",
			want:      fmt.Errorf("hwaddr 00-01:02:03:04:05 is not valid"),
		},
		{
			hwAddr:    "",
			poolName:  "vlan2",
			clientIP:  "",
			Reference: "",
			want:      fmt.Errorf("hwaddr is empty"),
		},
	}

	// AddLease function tests
	for i := 0; i < len(testLeases); i++ {
		if got := td.AddLease(
			testLeases[i].hwAddr,
			testLeases[i].poolName,
			testLeases[i].clientIP,
			testLeases[i].Reference,
		); got != testLeases[i].want {
			if got == nil || testLeases[i].want == nil {
				t.Errorf("got %q, wanted %q", got, testLeases[i].want)
			} else if got.Error() != testLeases[i].want.Error() {
				t.Errorf("got %q, wanted %q", got, testLeases[i].want)
			}
		}
	}

	// GetLease function tests
	lease1 := td.GetLease("aa:bb:cc:dd:ee:ff")
	if !lease1.ClientIP.Equal(net.ParseIP(testLeases[1].clientIP)) {
		t.Errorf("got %q, wanted %q", lease1.ClientIP.String(), testLeases[1].clientIP)
	}
	lease2 := td.GetLease("ff:ee:dd:cc:bb:aa")
	if len(lease2.ClientIP) > 0 {
		t.Errorf("got %q, wanted nil", lease2.ClientIP.String())
	}
	lease3 := td.GetLease("00:01:02:03:04:05")
	if lease3.Reference != testLeases[2].Reference {
		t.Errorf("got %q, wanted %q", lease3.Reference, testLeases[2].Reference)
	}

	// CheckLease function tests
	if !td.CheckLease("aa:bb:cc:dd:ee:ff") {
		t.Errorf("got false, wanted true for hwAddr aa:bb:cc:dd:ee:ff")
	}
	if td.CheckLease("00:11:22:33:44:55") {
		t.Errorf("got true, wanted false for hwAddr 00:11:22:33:44:55")
	}
	if td.CheckLease("ff:ee:dd:cc:bb:aa") {
		t.Errorf("got true, wanted false for hwAddr ff:ee:dd:cc:bb:aa")
	}
	if !td.CheckLease("00:01:02:03:04:05") {
		t.Errorf("got false, wanted true for hwAddr 00:01:02:03:04:05")
	}

	// DeleteLease function tests
	if got := td.DeleteLease("aa:bb:cc:dd:ee:ff"); got != nil {
		t.Errorf("got %q, wanted nil", got)
	}
	if got := td.DeleteLease("aa:bb:cc:dd:ee:ff"); got != nil {
		wanted := "lease for hwaddr aa:bb:cc:dd:ee:ff does not exists"
		if got.Error() != wanted {
			t.Errorf("got %q, wanted %q", got, wanted)
		}
	}

	// DeletePool function tests
	if got := td.DeletePool("vlan2"); got != nil {
		t.Errorf("got %q, wanted nil", got)
	}
	if got := td.DeletePool("vlan10"); got != nil {
		wanted := "pool vlan10 does not exists"
		if got.Error() != wanted {
			t.Errorf("got %q, wanted %q", got, wanted)
		}
	}
}
