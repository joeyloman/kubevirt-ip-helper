package vmnetcfg

import (
	"fmt"
	"reflect"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func equalStatusRows(a, b []kihv1.NetworkConfigStatus) bool { return reflect.DeepEqual(a, b) }

func (c *Controller) hasPendingCleanup(obj *kihv1.VirtualMachineNetworkConfig) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return len(c.pendingUnwinds[obj.Namespace+"/"+obj.Name]) != 0
}

// Local allocators contain only this helper's network. Keep the network
// fence explicit as well as the allocator's atomic owner check.
func (c *Controller) deleteLeaseInScope(mac, owner string) error {
	lease := c.dhcp.GetLease(mac)
	if lease.ClientIP != nil && lease.PoolName != c.scope.NetworkName() {
		return dhcp.ErrLeaseForeignOwner
	}
	return c.dhcp.DeleteLeaseOwnedBy(mac, owner)
}

// claimDecisionRetained accepts the original owned row or the exact allocation
// this sync intended to commit. An Update can commit and lose its response:
// that durable, still-wanted binding must not be unwound as a stale decision.
// API commits separately fence the original owned rows before every write.
func claimDecisionRetained(base, live *kihv1.VirtualMachineNetworkConfig, nc allocatedNetworkConfig) bool {
	var before, after []kihv1.NetworkConfig
	for _, row := range base.Spec.NetworkConfig {
		if util.CanonicalHWAddr(row.MACAddress) == util.CanonicalHWAddr(nc.macAddress) && util.QualifyNetworkName(base.Namespace, row.NetworkName) == nc.networkName {
			before = append(before, row)
		}
	}
	for _, row := range live.Spec.NetworkConfig {
		if util.CanonicalHWAddr(row.MACAddress) == util.CanonicalHWAddr(nc.macAddress) && util.QualifyNetworkName(live.Namespace, row.NetworkName) == nc.networkName {
			after = append(after, row)
		}
	}
	if len(before) == 0 || len(before) != len(after) {
		return false
	}
	if reflect.DeepEqual(before, after) {
		return true
	}
	// The canonical mac/network filters above already fixed the identity of
	// every matched row, so only the intended address may differ from it.
	for _, row := range after {
		if row.IPAddress != nc.ipAddress {
			return false
		}
	}
	return true
}

// recoverBindings never guesses an address from a status row. It combines
// authoritative owner-matching ledger entries with local owner snapshots.
// The global LIST is verification only; none of its objects enter discovery.
func (c *Controller) recoverBindings(obj *kihv1.VirtualMachineNetworkConfig, onlyMAC string) error {
	pools, err := c.kihClientset.KubevirtiphelperV1().IPPools().List(c.ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("cannot establish remaining owned reservations: %w", err)
	}
	macs := make(map[string]bool)
	tuples := make(map[kihv1.NetworkConfig]bool)
	if onlyMAC != "" {
		// a single-nic recovery already starts with its only permissible
		// key: no spec or status row can add another one
		macs[util.CanonicalHWAddr(onlyMAC)] = true
	} else {
		for _, row := range c.scope.FilterSpec(obj.Namespace, obj.Spec.NetworkConfig) {
			macs[util.CanonicalHWAddr(row.MACAddress)] = true
		}
		for _, row := range c.scope.FilterStatus(obj.Namespace, obj.Status.NetworkConfig) {
			macs[util.CanonicalHWAddr(row.MACAddress)] = true
		}
	}
	for _, pool := range pools.Items {
		if pool.Spec.NetworkName != c.scope.NetworkName() {
			continue
		}
		for ip, owner := range pool.Status.IPv4.Allocated {
			namespace, vmName, mac, ok := util.ParseAllocationRef(owner)
			if !ok || namespace != obj.Namespace || vmName != obj.Spec.VMName || (onlyMAC != "" && mac != onlyMAC) {
				continue
			}
			if !macs[mac] {
				if lease := c.dhcp.GetLease(mac); lease.ClientIP != nil && lease.PoolName == c.scope.NetworkName() && lease.Reference != obj.Namespace+"/"+obj.Spec.VMName {
					continue
				}
			}
			macs[mac] = true
			tuples[kihv1.NetworkConfig{MACAddress: mac, NetworkName: c.scope.NetworkName(), IPAddress: ip}] = true
		}
		if len(macs) == 0 {
			continue
		}
		cached, cacheErr := c.cache.Get("pool", c.scope.NetworkName())
		if cacheErr != nil || cached.(kihv1.IPPool).Name != pool.Name || !c.scope.MatchesPool(&pool) {
			return fmt.Errorf("pool %s is live but unavailable for owned cleanup", pool.Name)
		}
	}
	for mac := range macs {
		lease := c.dhcp.GetLease(mac)
		if lease.Reference == obj.Namespace+"/"+obj.Spec.VMName && lease.PoolName == c.scope.NetworkName() && lease.ClientIP != nil {
			tuples[kihv1.NetworkConfig{MACAddress: mac, NetworkName: c.scope.NetworkName(), IPAddress: lease.ClientIP.String()}] = true
		}
		for _, ip := range c.ipam.IPsOwnedBy(c.scope.NetworkName(), util.AllocationRef(obj.Namespace, obj.Spec.VMName, mac)) {
			tuples[kihv1.NetworkConfig{MACAddress: mac, NetworkName: c.scope.NetworkName(), IPAddress: ip}] = true
		}
	}
	// Ledger and local owner strings identify a VM/MAC, not a config. Even
	// a MAC still in our status can have transferred after spec acknowledgement.
	// Attribute every recovered tuple before releasing any of its state.
	if len(tuples) == 0 {
		return nil
	}
	configs, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(obj.Namespace).List(c.ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("cannot attribute recovered reservations: %w", err)
	}
	otherConfigMACs := make(map[string]bool)
	for _, other := range configs.Items {
		if (other.Name == obj.Name && other.UID == obj.UID) || other.Spec.VMName != obj.Spec.VMName {
			continue
		}
		for _, row := range c.scope.FilterSpec(other.Namespace, other.Spec.NetworkConfig) {
			otherConfigMACs[util.CanonicalHWAddr(row.MACAddress)] = true
		}
		for _, row := range c.scope.FilterStatus(other.Namespace, other.Status.NetworkConfig) {
			otherConfigMACs[util.CanonicalHWAddr(row.MACAddress)] = true
		}
	}
	for tuple := range tuples {
		if otherConfigMACs[tuple.MACAddress] {
			continue
		}
		if err := c.cleanupNetworkInterface(obj, &tuple, true); err != nil {
			return err
		}
	}
	return nil
}
