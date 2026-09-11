package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

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
	vmnetcfg, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vm.Namespace).Get(c.ctx, vm.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.createVirtualMachineNetworkConfigObject(vm)
		} else {
			return
		}
	}

	return c.updateVirtualMachineNetworkConfigObject(vm, vmnetcfg)
}

func (c *Controller) createVirtualMachineNetworkConfigObject(vm *kubevirtV1.VirtualMachine) (err error) {
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
	if err != nil {
		return fmt.Errorf("(vm.createVirtualMachineNetworkConfig) [%s/%s] cannot create VirtualMachineNetworkConfig object for vm: %s",
			vm.Namespace, vm.Name, err.Error())
	}

	log.Infof("(vm.createVirtualMachineNetworkConfig) [%s/%s] successfully created vmnetcfg object [%s/%s]",
		vm.Namespace, vm.Name, vmNetCfgObj.ObjectMeta.Namespace, vmNetCfgObj.ObjectMeta.Name)

	return
}

func (c *Controller) updateVirtualMachineNetworkConfigObject(vm *kubevirtV1.VirtualMachine, vmnetcfg *kihv1.VirtualMachineNetworkConfig) (err error) {
	log.Tracef("(vm.updateVirtualMachineNetworkConfigObject) [%s/%s] processing updated VirtualMachine  [%+v]",
		vm.Namespace, vm.Name, vm)

	newVmNetCfg := vmnetcfg.DeepCopy()

	netCfgs, err := c.getNetworkConfigs(vm, vmnetcfg.Spec.NetworkConfig)
	if err != nil {
		return
	}

	if reflect.DeepEqual(vmnetcfg.Spec.NetworkConfig, netCfgs) {
		log.Debugf("(vm.updateVirtualMachineNetworkConfigObject) [%s/%s] no network updates needed", vm.Namespace, vm.Name)
		return
	}

	newVmNetCfg.Spec.NetworkConfig = netCfgs

	log.Tracef("(vm.updateVirtualMachineNetworkConfigObject) [%s/%s] new vmnetcfg networkconfig: [%+v]",
		vm.Namespace, vm.Name, newVmNetCfg.Spec.NetworkConfig)

	// when the nics in the vm differs from the vmnetcfg the mismatches should be cleaned up first
	var nicCleanup bool
	for _, curNetCfg := range vmnetcfg.Spec.NetworkConfig {
		nicCleanup = true
		for _, newNetCfg := range netCfgs {
			if curNetCfg.MACAddress == newNetCfg.MACAddress && curNetCfg.NetworkName == newNetCfg.NetworkName && curNetCfg.IPAddress == newNetCfg.IPAddress {
				nicCleanup = false
			}
		}
		if nicCleanup {
			// a failed cleanup aborts the sync: the durable update must not
			// proceed on half-freed interface state
			if err := c.cleanupNetworkInterface(vmnetcfg, &curNetCfg); err != nil {
				return err
			}
		}
	}

	vmNetCfgObj, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(newVmNetCfg.Namespace).Update(c.ctx, newVmNetCfg, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("(vm.updateVirtualMachineNetworkConfigObject) [%s/%s] cannot update VirtualMachineNetworkConfig object for vm: %s",
			vm.Namespace, vm.Name, err.Error())
	}

	log.Infof("(vm.updateVirtualMachineNetworkConfigObject) [%s/%s] successfully updated vmnetcfg object [%s/%s]",
		vm.Namespace, vm.Name, vmNetCfgObj.ObjectMeta.Namespace, vmNetCfgObj.ObjectMeta.Name)

	return
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
						if c.dhcp.CheckLease(nic.MacAddress) {
							lease := c.dhcp.GetLease(nic.MacAddress)
							if lease.Reference != fmt.Sprintf("%s/%s", vm.Namespace, vm.Name) {
								return netCfgs, fmt.Errorf("hwaddr %s belongs to %s instead of %s/%s, skipping vmnetcfg actions",
									nic.MacAddress, lease.Reference, vm.Namespace, vm.Name)
							}
						}

						netCfg := kihv1.NetworkConfig{}
						netCfg.MACAddress = nic.MacAddress
						netCfg.NetworkName = net.Multus.NetworkName

						for _, oldnet := range curNetCfg {
							if oldnet.MACAddress == nic.MacAddress && oldnet.NetworkName == net.Multus.NetworkName {
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
	var unrecordedPoolName string
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
				unrecordedPoolName = pool.(kihv1.IPPool).Name
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
		} else if unrecordedPoolName != "" {
			// the release above changed the pool accounting after the
			// un-record already persisted the pre-release counts: the
			// persisted status must match the live allocator even when no
			// follow-up write of the vmnetcfg controller ever lands, so
			// the counts are republished through the same computation
			// ippoolstatus.UpdateStatus performs (a converged no-op
			// DELETE whose entry is already gone). the republish is
			// best-effort: a foreign re-entry or a failed write is
			// reported, not retried - the cleanup itself has converged
			if republishErr := c.updateIPPoolStatus(
				DELETE,
				vmnetcfg.Namespace,
				vmnetcfg.Spec.VMName,
				netCfg.IPAddress,
				netCfg.NetworkName,
				netCfg.MACAddress,
				unrecordedPoolName,
			); republishErr != nil {
				log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] cannot republish the pool status counts of network %s: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, netCfg.NetworkName, republishErr.Error())
				c.metrics.UpdateLogStatus("warning")
			}
		}
	}

	return
}

func (c *Controller) updateIPPoolStatus(event string, vmnetcfgNamespace string, vmnetcfgVMName string, ip string, networkName string, hwAddr string, poolName string) (err error) {
	return ippoolstatus.UpdateStatus(c.ctx, c.kihClientset, c.ipam, event, vmnetcfgNamespace, vmnetcfgVMName, ip, networkName, hwAddr, poolName)
}
