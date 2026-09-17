package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	log "github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubevirtV1 "kubevirt.io/api/core/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ippoolstatus"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

func (c *Controller) handleVirtualMachineObjectChange(vm *kubevirtV1.VirtualMachine) (err error) {
	if vm.DeletionTimestamp != nil {
		return nil
	}
	vmnetcfg, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vm.Namespace).Get(c.ctx, vm.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.createVirtualMachineNetworkConfigObject(vm)
		} else {
			return
		}
	}

	// an object which is already being deleted is not this vm's to
	// configure: the vmnetcfg controller's finalizer cleanup iterates the
	// spec of the doomed object and releases the leases, the ipam claims
	// and the ledger records of the nics it finds there, so writing this
	// vm's spec into it hands the reservations of the replacement to a
	// cleanup which is about to delete the object (the replacement then
	// serves without them until its own resync re-creates the vmnetcfg).
	// the sync is deferred with a retriable error: the rate-limited retry
	// and the resync converge once the object is gone, and the retried
	// sync creates the replacement's own object
	if vmnetcfg.ObjectMeta.DeletionTimestamp != nil {
		return fmt.Errorf("(vm.handleVirtualMachineObjectChange) [%s/%s] the VirtualMachineNetworkConfig object is being deleted, deferring the sync until it is gone",
			vm.Namespace, vm.Name)
	}

	return c.updateVirtualMachineNetworkConfigObject(vm, vmnetcfg)
}

func (c *Controller) createVirtualMachineNetworkConfigObject(vm *kubevirtV1.VirtualMachine) (err error) {
	if vm.DeletionTimestamp != nil {
		return nil
	}
	log.Tracef("(vm.createVirtualMachineNetworkConfigObject) [%s/%s] processing new VirtualMachine [%+v]",
		vm.Namespace, vm.Name, vm)

	newVmNetCfg := kihv1.VirtualMachineNetworkConfig{}
	newVmNetCfg.ObjectMeta.Name = vm.ObjectMeta.Name
	newVmNetCfg.ObjectMeta.Namespace = vm.ObjectMeta.Namespace
	finalizers := []string{}
	finalizers = append(finalizers, "kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup")
	newVmNetCfg.ObjectMeta.Finalizers = finalizers
	newVmNetCfg.Spec.VMName = vm.ObjectMeta.Name

	netCfgs, err := c.getNetworkConfigs(vm, nil)
	if err != nil {
		return
	}
	if len(netCfgs) < 1 {
		log.Debugf("(vm.createVirtualMachineNetworkConfig) [%s/%s] no network configuration found for vm",
			vm.Namespace, vm.Name)

		return
	}
	newVmNetCfg.Spec.NetworkConfig = netCfgs

	vmNetCfgObj, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(newVmNetCfg.Namespace).Create(c.ctx, &newVmNetCfg, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, getErr := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vm.Namespace).Get(c.ctx, vm.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		return c.updateVirtualMachineNetworkConfigObject(vm, current)
	}
	if err != nil {
		return fmt.Errorf("(vm.createVirtualMachineNetworkConfig) [%s/%s] cannot create VirtualMachineNetworkConfig object for vm: %s",
			vm.Namespace, vm.Name, err.Error())
	}

	log.Infof("(vm.createVirtualMachineNetworkConfig) [%s/%s] successfully created vmnetcfg object [%s/%s]",
		vm.Namespace, vm.Name, vmNetCfgObj.ObjectMeta.Namespace, vmNetCfgObj.ObjectMeta.Name)

	return
}

func (c *Controller) updateVirtualMachineNetworkConfigObject(vm *kubevirtV1.VirtualMachine, vmnetcfg *kihv1.VirtualMachineNetworkConfig) error {
	if vmnetcfg.DeletionTimestamp != nil || vm.DeletionTimestamp != nil {
		return fmt.Errorf("cannot project NICs onto deleting VM or VMNetCfg %s/%s", vm.Namespace, vm.Name)
	}
	if vmnetcfg.Spec.VMName != vm.Name {
		return fmt.Errorf("VMNetCfg %s/%s belongs to VM %q", vmnetcfg.Namespace, vmnetcfg.Name, vmnetcfg.Spec.VMName)
	}
	desired, err := c.getNetworkConfigs(vm, nil)
	if err != nil {
		return err
	}
	wanted := make(map[networkConfigIdentity]bool, len(desired))
	for _, nic := range desired {
		wanted[networkConfigKey(vm.Namespace, nic.NetworkName, nic.MACAddress)] = true
	}
	// Cleanup is performed once, outside the API conflict loops. Retain all
	// durable acknowledgements until every removed owned binding is clean.
	removed := make(map[networkConfigIdentity]kihv1.NetworkConfig)
	for _, nic := range c.scope.FilterSpec(vmnetcfg.Namespace, vmnetcfg.Spec.NetworkConfig) {
		key := networkConfigKey(vmnetcfg.Namespace, nic.NetworkName, nic.MACAddress)
		if !wanted[key] {
			removed[key] = nic
		}
	}
	for _, nic := range c.scope.FilterStatus(vmnetcfg.Namespace, vmnetcfg.Status.NetworkConfig) {
		key := networkConfigKey(vmnetcfg.Namespace, nic.NetworkName, nic.MACAddress)
		if _, found := removed[key]; !wanted[key] && !found {
			removed[key] = kihv1.NetworkConfig{NetworkName: nic.NetworkName, MACAddress: nic.MACAddress}
		}
	}
	for _, nic := range removed {
		if err := c.cleanupRemovedInterface(vmnetcfg, nic); err != nil {
			return err
		}
	}
	if len(desired) == 0 && len(removed) == 0 {
		return nil
	}
	if err := c.commitProjection(vmnetcfg, false, func(current *kihv1.VirtualMachineNetworkConfig) error {
		if err := c.verifyProjectionRows(vmnetcfg, current, wanted, removed); err != nil {
			return err
		}
		// Preserve the latest allocated IP, including an allocation committed
		// by the NIC controller after the projection's original read.
		for i := range desired {
			desired[i].IPAddress = ""
			for _, nic := range current.Spec.NetworkConfig {
				if networkConfigKey(current.Namespace, nic.NetworkName, nic.MACAddress) == networkConfigKey(current.Namespace, desired[i].NetworkName, desired[i].MACAddress) {
					desired[i].IPAddress = nic.IPAddress
					break
				}
			}
		}
		current.Spec.NetworkConfig = c.scope.MergeSpec(current.Namespace, current.Spec.NetworkConfig, desired)
		return nil
	}); err != nil {
		return err
	}
	if len(removed) == 0 {
		return nil
	}
	return c.commitProjection(vmnetcfg, true, func(current *kihv1.VirtualMachineNetworkConfig) error {
		if err := c.verifyProjectionRows(vmnetcfg, current, wanted, removed); err != nil {
			return err
		}
		var remaining []kihv1.NetworkConfigStatus
		for _, nic := range c.scope.FilterStatus(current.Namespace, current.Status.NetworkConfig) {
			if _, cleaned := removed[networkConfigKey(current.Namespace, nic.NetworkName, nic.MACAddress)]; !cleaned {
				remaining = append(remaining, nic)
			}
		}
		current.Status.NetworkConfig = c.scope.MergeStatus(current.Namespace, current.Status.NetworkConfig, remaining)
		return nil
	})
}

type networkConfigIdentity struct {
	network string
	mac     string
}

func networkConfigKey(namespace, network, mac string) networkConfigIdentity {
	return networkConfigIdentity{network: util.QualifyNetworkName(namespace, network), mac: util.CanonicalHWAddr(mac)}
}

// verifyProjectionRows fences cleanup acknowledgements against changed owned
// bindings. Foreign changes and newer IPs on still-desired NICs are mergeable.
func (c *Controller) verifyProjectionRows(base, current *kihv1.VirtualMachineNetworkConfig, wanted map[networkConfigIdentity]bool, removed map[networkConfigIdentity]kihv1.NetworkConfig) error {
	for _, old := range c.scope.FilterSpec(base.Namespace, base.Spec.NetworkConfig) {
		key := networkConfigKey(base.Namespace, old.NetworkName, old.MACAddress)
		if !wanted[key] {
			continue
		}
		found := false
		for _, nic := range current.Spec.NetworkConfig {
			if networkConfigKey(current.Namespace, nic.NetworkName, nic.MACAddress) == key {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("owned NIC disappeared during projection of %s/%s", current.Namespace, current.Name)
		}
	}
	for _, nic := range c.scope.FilterSpec(current.Namespace, current.Spec.NetworkConfig) {
		key := networkConfigKey(current.Namespace, nic.NetworkName, nic.MACAddress)
		if wanted[key] {
			continue
		}
		cleaned, ok := removed[key]
		if !ok || cleaned.IPAddress != nic.IPAddress {
			return fmt.Errorf("owned NIC changed during projection of %s/%s", current.Namespace, current.Name)
		}
	}
	for _, nic := range c.scope.FilterStatus(current.Namespace, current.Status.NetworkConfig) {
		key := networkConfigKey(current.Namespace, nic.NetworkName, nic.MACAddress)
		if wanted[key] {
			continue
		}
		// An exact base row already proves the unwanted row was acknowledged.
		found := false
		for _, old := range base.Status.NetworkConfig {
			if networkConfigKey(base.Namespace, old.NetworkName, old.MACAddress) == key && reflect.DeepEqual(old, nic) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("owned status NIC changed during projection of %s/%s", current.Namespace, current.Name)
		}
	}
	return nil
}

// commitProjection retries only the API intent, never lease/ledger cleanup.
func (c *Controller) commitProjection(base *kihv1.VirtualMachineNetworkConfig, status bool, merge func(*kihv1.VirtualMachineNetworkConfig) error) error {
	client := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(base.Namespace)
	for attempt := range 10 {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		current, err := client.Get(c.ctx, base.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != base.UID || current.Spec.VMName != base.Spec.VMName || current.DeletionTimestamp != nil {
			return fmt.Errorf("VMNetCfg %s/%s replaced, deleting or owner changed during projection", base.Namespace, base.Name)
		}
		next := current.DeepCopy()
		if err := merge(next); err != nil {
			return err
		}
		if status {
			if reflect.DeepEqual(current.Status.NetworkConfig, next.Status.NetworkConfig) {
				return nil
			}
			_, err = client.UpdateStatus(c.ctx, next, metav1.UpdateOptions{})
		} else {
			if reflect.DeepEqual(current.Spec.NetworkConfig, next.Spec.NetworkConfig) {
				return nil
			}
			_, err = client.Update(c.ctx, next, metav1.UpdateOptions{})
		}
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) || attempt == 9 {
			return err
		}
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
		}
	}
	return fmt.Errorf("projection retries exhausted for %s/%s", base.Namespace, base.Name)
}

// A status-only or not-yet-assigned spec row does not prove cleanup completed.
// Resolve its binding from the owner-checked durable ledger and local lease.
func (c *Controller) cleanupRemovedInterface(cfg *kihv1.VirtualMachineNetworkConfig, nic kihv1.NetworkConfig) error {
	if !c.scope.Owns(cfg.Namespace, nic.NetworkName) {
		return nil
	}
	nic.NetworkName = c.scope.NetworkName()
	nic.MACAddress = util.CanonicalHWAddr(nic.MACAddress)
	if nic.IPAddress != "" {
		return c.cleanupNetworkInterface(cfg, &nic)
	}
	pools, err := c.kihClientset.KubevirtiphelperV1().IPPools().List(c.ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("cannot resolve removed NIC reservation: %w", err)
	}
	addresses := make(map[string]bool)
	for _, ip := range c.ipam.IPsOwnedBy(nic.NetworkName, util.AllocationRef(cfg.Namespace, cfg.Spec.VMName, nic.MACAddress)) {
		addresses[ip] = true
	}
	for _, pool := range pools.Items {
		if pool.Spec.NetworkName != nic.NetworkName {
			continue
		}
		if _, err := c.cache.Get("pool", nic.NetworkName); err != nil {
			return fmt.Errorf("cannot resolve removed NIC while pool %s is unavailable: %w", pool.Name, err)
		}
		for ip, ref := range pool.Status.IPv4.Allocated {
			ns, vm, mac, valid := util.ParseAllocationRef(ref)
			if valid && ns == cfg.Namespace && vm == cfg.Spec.VMName && mac == nic.MACAddress {
				addresses[ip] = true
			}
		}
	}
	lease := c.dhcp.GetLease(nic.MACAddress)
	if lease.Reference == cfg.Namespace+"/"+cfg.Spec.VMName && lease.PoolName == nic.NetworkName && lease.ClientIP != nil {
		addresses[lease.ClientIP.String()] = true
	}
	for ip := range addresses {
		nic.IPAddress = ip
		if err := c.cleanupNetworkInterface(cfg, &nic); err != nil {
			return err
		}
	}
	if len(addresses) == 0 {
		if err := c.cleanupNetworkInterface(cfg, &nic); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) deleteVirtualMachineNetworkConfigObject(vmNamespace string, vmName string) (err error) {
	obj, exists, err := c.checkVirtualMachineNetworkConfigObject(vmNamespace, vmName)
	if err != nil {
		// a transient get failure must not be mistaken for a missing
		// object: propagate it so the rate-limited retry re-runs the
		// deletion while the binding stays retained
		return fmt.Errorf("(vm.deleteVirtualMachineNetworkConfigObject) [%s/%s] cannot check VirtualMachineNetworkConfig object for vm: %s",
			vmNamespace, vmName, err.Error())
	}

	if !exists {
		log.Warnf("(vm.deleteVirtualMachineNetworkConfigObject) [%s/%s] vmnetcfg %s/%s does not exists",
			vmNamespace, vmName, vmNamespace, vmName)

		return
	}
	if len(c.scope.FilterSpec(obj.Namespace, obj.Spec.NetworkConfig)) == 0 && len(c.scope.FilterStatus(obj.Namespace, obj.Status.NetworkConfig)) == 0 {
		if len(obj.Spec.NetworkConfig) != 0 || len(obj.Status.NetworkConfig) != 0 {
			return nil
		}
		managed := false
		for _, finalizer := range obj.Finalizers {
			if finalizer == "kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup" {
				managed = true
				break
			}
		}
		if !managed {
			return nil
		}
	}
	if obj.Spec.VMName != vmName {
		return fmt.Errorf("VMNetCfg %s/%s belongs to VM %q", obj.Namespace, obj.Name, obj.Spec.VMName)
	}

	// the delete is conditioned on the uid the preflight get observed: a
	// same-name replacement created between the get and the delete is
	// rejected by the apiserver instead of destroyed, and the retry
	// converges through the informer-store replacement guard
	if err = c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmNamespace).Delete(c.ctx, vmName, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &obj.UID},
	}); err != nil {
		if apierrors.IsNotFound(err) {
			// another worker or a concurrent cleanup already removed the object
			log.Debugf("(vm.deleteVirtualMachineNetworkConfigObject) [%s/%s] vmnetcfg object already deleted",
				vmNamespace, vmName)

			err = nil

			return
		}

		return fmt.Errorf("(vm.deleteVirtualMachineNetworkConfigObject) [%s/%s] cannot delete VirtualMachineNetworkConfig object for vm: %s",
			vmNamespace, vmName, err.Error())
	}

	log.Infof("(vm.deleteVirtualMachineNetworkConfigObject) [%s/%s] successfully released vmnetcfg object [%s/%s]",
		vmNamespace, vmName, vmNamespace, vmName)

	return
}

// checkVirtualMachineNetworkConfigObject checks whether the vmnetcfg object
// of a vm exists and returns the observed object. a missing object is
// (nil, false, nil); every other api failure is propagated so a transient
// error can never be mistaken for absence.
func (c *Controller) checkVirtualMachineNetworkConfigObject(vmNamespace string, vmName string) (obj *kihv1.VirtualMachineNetworkConfig, exists bool, err error) {
	obj, err = c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmNamespace).Get(c.ctx, vmName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, err
	}

	return obj, true, nil
}

func (c *Controller) getNetworkConfigs(vm *kubevirtV1.VirtualMachine, curNetCfg []kihv1.NetworkConfig) (netCfgs []kihv1.NetworkConfig, err error) {
	if vm.Spec.Template == nil {
		return nil, nil
	}
	// make sure it also stays compatible with Harvester
	var harvesterMacs map[string]string
	if vm.ObjectMeta.Annotations != nil {
		if macAnnotation, exists := vm.ObjectMeta.Annotations["harvesterhci.io/mac-address"]; exists {
			if err := json.Unmarshal([]byte(macAnnotation), &harvesterMacs); err != nil {
				log.Warnf("(vm.getNetworkConfigs) [%s/%s] failed to parse harvesterhci.io/mac-address annotation: %s",
					vm.Namespace, vm.Name, err)
			}
		}
	}

	for _, nic := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		for _, net := range vm.Spec.Template.Spec.Networks {
			if nic.Name == net.Name {
				if net.Multus == nil {
					// we only support multus at the moment
					log.Warnf("(vm.getNetworkConfigs) [%s/%s] unsupported network type found!",
						vm.Namespace, vm.Name)
				} else {
					if !c.scope.Owns(vm.Namespace, net.Multus.NetworkName) {
						continue
					}
					if nic.MacAddress == "" {
						// when a new vm is created the macaddress doesn't exists immediately
						// it takes a couple of object updates before the macaddress is assigned

						// try to get it from harvester annotation
						macAddress := ""
						if harvesterMacs != nil {
							if macFromAnnotation, found := harvesterMacs[net.Name]; found {
								macAddress = macFromAnnotation
								log.Debugf("(vm.getNetworkConfigs) [%s/%s] found mac address %s from harvester annotation for %s",
									vm.Namespace, vm.Name, macAddress, net.Name)
							}
						}
						if macAddress == "" {
							log.Debugf("(vm.getNetworkConfigs) [%s/%s] no mac address found for vm",
								vm.Namespace, vm.Name)
							continue
						}
						// use the MAC address from annotation for further processing
						nic.MacAddress = macAddress
					}
					if net.Multus.NetworkName == "" {
						// the networkname should be there from the beginning
						log.Errorf("(vm.getNetworkConfigs) [%s/%s] no networkname found for vm",
							vm.Namespace, vm.Name)
						c.metrics.UpdateLogStatus("error")
					} else {
						nic.MacAddress = util.CanonicalHWAddr(nic.MacAddress)
						if c.dhcp.CheckLease(nic.MacAddress) {
							lease := c.dhcp.GetLease(nic.MacAddress)
							if lease.Reference != fmt.Sprintf("%s/%s", vm.Namespace, vm.Name) {
								return netCfgs, fmt.Errorf("hwaddr %s belongs to %s instead of %s/%s, skipping vmnetcfg actions",
									nic.MacAddress, lease.Reference, vm.Namespace, vm.Name)
							}
						}

						netCfg := kihv1.NetworkConfig{}
						netCfg.MACAddress = nic.MacAddress
						netCfg.NetworkName = c.scope.NetworkName()

						for _, oldnet := range curNetCfg {
							if networkConfigKey(vm.Namespace, oldnet.NetworkName, oldnet.MACAddress) == networkConfigKey(vm.Namespace, netCfg.NetworkName, nic.MacAddress) {
								netCfg.IPAddress = oldnet.IPAddress
							}
						}

						netCfgs = append(netCfgs, netCfg)
					}
				}
			}
		}
	}

	return
}

// cleanupNetworkInterface frees the dhcp lease, the ipam reservation and
// the ippool status entry of a network interface which the vm no longer
// has. the release is ownership-safe, so a delayed or retried cleanup can
// never free state another vm acquired in the meantime: the lease is
// removed under an owner check, an ip whose lease is already held by
// another vm in the same network is left to that vm while this nic's own
// status entry is still un-recorded (a successor lease is not proof that
// the bookkeeping completed), and the ipam release only frees the address
// while its reservation still carries this nic's owner reference - a
// successor's named allocation and a registration protection pin both
// stay untouched.
func (c *Controller) cleanupNetworkInterface(vmnetcfg *kihv1.VirtualMachineNetworkConfig, netCfg *kihv1.NetworkConfig) (err error) {
	if !c.scope.Owns(vmnetcfg.Namespace, netCfg.NetworkName) {
		return nil
	}
	canonical := *netCfg
	canonical.NetworkName = c.scope.NetworkName()
	canonical.MACAddress = util.CanonicalHWAddr(canonical.MACAddress)
	netCfg = &canonical
	log.Debugf("(vm.cleanupNetworkInterface) [%s/%s] cleaning interface with hwaddr=%s, networkname=%s, ipaddress=%s",
		vmnetcfg.Namespace, vmnetcfg.Name, netCfg.MACAddress, netCfg.NetworkName, netCfg.IPAddress)

	ref := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)

	// freeing an ip which is leased to another vm of the same network
	// would leave the other lease serving an address ipam could reissue to
	// a third client; ipam itself holds no owner references, so this
	// stays a network-scoped snapshot check without an owner-validated
	// release primitive: the same numeric addresses of separate networks
	// are no claim on this network's allocation
	successorLive := false
	if netCfg.IPAddress != "" {
		if leaseHwAddr, lease, found := c.dhcp.GetLeaseByIPAndNetwork(netCfg.NetworkName, netCfg.IPAddress); found && lease.Reference != ref {
			// a successor vm owns the ip live: its lease and its
			// reservation are never touched by this cleanup. but a
			// successor lease is not proof that this nic's bookkeeping
			// completed: the local release and the durable un-record are
			// independent steps, and an un-record which failed or never
			// ran leaves this nic's own ledger entry behind while the
			// successor already serves the address - the orphan then
			// blocks the successor's own ledger write forever and every
			// registration re-pins the address to the ghost owner. the
			// owner-checked un-record below still runs: it removes this
			// nic's own orphan and is a converged no-op when the entry is
			// already gone, while the successor's entry (a foreign owner)
			// is never touched. the live release stays skipped: the
			// successor demonstrably owns the address, and even a claim
			// which still carries this nic's own reference must not be
			// freed while the successor's lease keeps serving the address
			log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] ip %s belongs to %s via hwaddr %s, skipping the release of it",
				vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, lease.Reference, leaseHwAddr)
			c.metrics.UpdateLogStatus("warning")

			successorLive = true
		}
	}

	// the durable un-record happens before any local release (mirroring
	// the vmnetcfg live path): the address is never locally freed while
	// its ownership record is still written, otherwise a crash between the
	// release and the status write leaves an orphan ledger entry which the
	// next registration re-pins to the ghost owner
	var cleanupPoolName string
	if netCfg.IPAddress != "" {
		pool, poolErr := c.cache.Get("pool", netCfg.NetworkName)
		if poolErr != nil {
			// a deleted pool object takes its whole status ledger with it,
			// so the un-record may only be skipped when the pool is truly
			// gone: verify that through the api. a pool object which merely
			// missed the cache still holds the ledger entry, and skipping
			// the un-record would orphan it forever
			apiPools, listErr := c.kihClientset.KubevirtiphelperV1().IPPools().List(c.ctx, metav1.ListOptions{})
			if listErr == nil {
				poolExists := false
				for _, p := range apiPools.Items {
					if p.Spec.NetworkName == netCfg.NetworkName {
						poolExists = true

						break
					}
				}

				if !poolExists {
					log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] the pool of network %s does not exist anymore, its status record is gone with it",
						vmnetcfg.Namespace, vmnetcfg.Name, netCfg.NetworkName)
				} else {
					return fmt.Errorf("(vm.cleanupNetworkInterface) [%s/%s] cannot un-record ip %s of network %s: %s",
						vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, netCfg.NetworkName, poolErr.Error())
				}
			} else if listErr != nil {
				// the api verification itself failed: fail conservatively,
				// the record might still exist in a live pool object
				return fmt.Errorf("(vm.cleanupNetworkInterface) [%s/%s] cannot verify the pool of network %s, cache miss: %s: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, netCfg.NetworkName, poolErr.Error(), listErr.Error())
			}
		} else {
			if statusErr := c.updateIPPoolStatus(
				DELETE,
				vmnetcfg.Namespace,
				vmnetcfg.Spec.VMName,
				netCfg.IPAddress,
				netCfg.NetworkName,
				netCfg.MACAddress,
				pool.(kihv1.IPPool).Name,
			); statusErr != nil {
				// the status entry of another owner is not this vm's to
				// remove; replaying the cleanup must not abort the durable
				// update over it. the entry stays, but the live state of this
				// nic is still released below: the lease deletion and the ipam
				// release are independently owner-validated, so they are
				// converged no-ops when the live state genuinely belongs to
				// another owner, while a ledger entry which merely diverges
				// from the live state (a legacy spelling or a hand-edited
				// record) must not pin the lease and the reservation of a
				// removed nic forever
				if !errors.Is(statusErr, util.ErrForeignOwner) {
					return fmt.Errorf("(vm.cleanupNetworkInterface) [%s/%s] %s",
						vmnetcfg.Namespace, vmnetcfg.Name, statusErr.Error())
				}

				log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] the allocation of ip %s in the %s status belongs to another owner, leaving the entry",
					vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, pool.(kihv1.IPPool).Name)
				c.metrics.UpdateLogStatus("warning")
			} else {
				cleanupPoolName = pool.(kihv1.IPPool).Name
			}
		}
	}

	// the successor owns the whole live state of the address: only the
	// durable un-record of this nic's own entry ran above, while the
	// lease and the reservation below belong to the successor's binding
	// and a claim which diverges from the live state is never freed under
	// a foreign lease
	if successorLive {
		return
	}

	// the owner check and the deletion run under one lock acquisition, so
	// a delayed cleanup cannot delete a lease which a concurrent writer
	// reassigned to another vm
	lease := c.dhcp.GetLease(netCfg.MACAddress)
	// A same-MAC binding on another network is never ours to delete.
	// The owner-checked IPAM release below remains network-scoped.
	if lease.Reference != ref || lease.PoolName == netCfg.NetworkName {
		if err := c.dhcp.DeleteLeaseOwnedBy(netCfg.MACAddress, ref); err != nil {
			switch {
			case errors.Is(err, dhcp.ErrLeaseNotFound):
				// no lease left for this interface: the cleanup already
				// converged, nothing to replay

			case errors.Is(err, dhcp.ErrLeaseInvalidHwAddr):
				// an unparseable mac can never own a lease: the dhcp side of
				// this cleanup has converged, the ipam release of an
				// unparseable ip is classified the same way below

			case errors.Is(err, dhcp.ErrLeaseForeignOwner):
				// the mac was reassigned to another vm which owns the whole
				// interface state by now
				log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
				c.metrics.UpdateLogStatus("warning")

			default:
				return fmt.Errorf("(vm.cleanupNetworkInterface) [%s/%s] error deleting lease from dhcp: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			}
		}
	}

	if netCfg.IPAddress != "" {
		// the release is owner-validated: a binding's fresh allocation is
		// a named reservation, so the release only frees the address while
		// it still carries this nic's owner reference. a successor which
		// took the address over in the meantime - after this cleanup's own
		// lease snapshot check passed, or after the removed nic's binding
		// compensated a raced cleanup by releasing its claim - is never
		// freed with it, while an ownerless protection pin of the
		// registration (an unusable-mac claim or an unknown historical
		// reference) is not this cleanup's to release either
		ownerRef := util.AllocationRef(vmnetcfg.Namespace, vmnetcfg.Spec.VMName, netCfg.MACAddress)

		log.Debugf("(vm.cleanupNetworkInterface) [%s/%s] releasing the ipam reservation of ip %s for hwaddr %s under the owner %q",
			vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, netCfg.MACAddress, ownerRef)

		if err := c.ipam.ReleaseIPOwnedBy(netCfg.NetworkName, netCfg.IPAddress, ownerRef); err != nil {
			if errors.Is(err, ipam.ErrIPForeignOwner) {
				// the address belongs to a successor or to a conservative
				// registration pin: converged, nothing left to release
				log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] ip %s is allocated by another owner, skipping the release of it",
					vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress)
				c.metrics.UpdateLogStatus("warning")
			} else if !util.IsAlreadyReleased(err) && !util.IsUnusableIdentity(err) {
				// already-free addresses and unparseable addresses are
				// treated as done so a retried cleanup can converge
				return fmt.Errorf("(vm.cleanupNetworkInterface) [%s/%s] error releasing ip from ipam: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			}
		} else if cleanupPoolName != "" {
			// Republish only after our successful un-record and local release.
			// As before, a failed refresh is logged rather than retried.
			if err := c.updateIPPoolStatus(DELETE, vmnetcfg.Namespace, vmnetcfg.Spec.VMName, netCfg.IPAddress, netCfg.NetworkName, netCfg.MACAddress, cleanupPoolName); err != nil {
				log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] cannot refresh pool accounting: %s", vmnetcfg.Namespace, vmnetcfg.Name, err)
				c.metrics.UpdateLogStatus("warning")
			}
		}
	}

	return
}

func (c *Controller) updateIPPoolStatus(event string, vmnetcfgNamespace string, vmnetcfgVMName string, ip string, networkName string, hwAddr string, poolName string) (err error) {
	return ippoolstatus.UpdateStatus(c.ctx, c.kihClientset, c.ipam, event, vmnetcfgNamespace, vmnetcfgVMName, ip, networkName, hwAddr, poolName)
}
