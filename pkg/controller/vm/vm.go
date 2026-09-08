package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubevirtV1 "kubevirt.io/api/core/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

func (c *Controller) handleVirtualMachineObjectChange(vm *kubevirtV1.VirtualMachine) (err error) {
	vmnetcfg, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vm.Namespace).Get(context.TODO(), vm.Name, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
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

	vmNetCfgObj, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(newVmNetCfg.Namespace).Create(context.TODO(), &newVmNetCfg, metav1.CreateOptions{})
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

	vmNetCfgObj, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(newVmNetCfg.Namespace).Update(context.TODO(), newVmNetCfg, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("(vm.updateVirtualMachineNetworkConfigObject) [%s/%s] cannot update VirtualMachineNetworkConfig object for vm: %s",
			vm.Namespace, vm.Name, err.Error())
	}

	log.Infof("(vm.updateVirtualMachineNetworkConfigObject) [%s/%s] successfully updated vmnetcfg object [%s/%s]",
		vm.Namespace, vm.Name, vmNetCfgObj.ObjectMeta.Namespace, vmNetCfgObj.ObjectMeta.Name)

	return
}

func (c *Controller) deleteVirtualMachineNetworkConfigObject(vmNamespace string, vmName string) (err error) {
	if !c.checkVirtualMachineNetworkConfigObject(vmNamespace, vmName) {
		log.Warnf("(vm.deleteVirtualMachineNetworkConfigObject) [%s/%s] vmnetcfg %s/%s does not exists",
			vmNamespace, vmName, vmNamespace, vmName)

		return
	}

	if err = c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmNamespace).Delete(context.TODO(), vmName, metav1.DeleteOptions{}); err != nil {
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

func (c *Controller) checkVirtualMachineNetworkConfigObject(vmNamespace string, vmName string) bool {
	if _, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmNamespace).Get(context.TODO(), vmName, metav1.GetOptions{}); err != nil {
		return false
	}

	return true
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
// has. the release is ownership-safe, so a retried cleanup after a failed
// durable update cannot free state another vm acquired in the meantime:
// own leases are removed under an owner check, and an ip whose lease is
// already held by another vm in the same network is left to that vm.
func (c *Controller) cleanupNetworkInterface(vmnetcfg *kihv1.VirtualMachineNetworkConfig, netCfg *kihv1.NetworkConfig) (err error) {
	log.Debugf("(vm.cleanupNetworkInterface) [%s/%s] cleaning interface with hwaddr=%s, networkname=%s, ipaddress=%s",
		vmnetcfg.Namespace, vmnetcfg.Name, netCfg.MACAddress, netCfg.NetworkName, netCfg.IPAddress)

	ref := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)

	// the owner check and the deletion run under one lock acquisition, so
	// a delayed cleanup cannot delete a lease which a concurrent writer
	// reassigned to another vm
	if err := c.dhcp.DeleteLeaseOwnedBy(netCfg.MACAddress, ref); err != nil {
		switch {
		case errors.Is(err, dhcp.ErrLeaseNotFound):
			// no lease left for this interface: the cleanup already
			// converged, nothing to replay

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

	// freeing an ip which is leased to another vm would leave the other
	// lease serving an address ipam could reissue to a third client; ipam
	// itself holds no owner references, so this stays a snapshot check
	// without an owner-validated release primitive. a lease under another
	// networkname holds no claim on this network's allocation, so the
	// release proceeds.
	if netCfg.IPAddress != "" {
		if leaseHwAddr, lease, found := c.dhcp.GetLeaseByIP(netCfg.IPAddress); found && lease.PoolName == netCfg.NetworkName && lease.Reference != ref {
			// the release already happened in an earlier attempt and a
			// successor vm owns the ip now: this interface's cleanup
			// converged
			log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] ip %s belongs to %s via hwaddr %s, skipping the release of it",
				vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, lease.Reference, leaseHwAddr)
			c.metrics.UpdateLogStatus("warning")

			return
		}

		if err := c.ipam.ReleaseIP(netCfg.NetworkName, netCfg.IPAddress); err != nil {
			// already-free addresses are treated as done so a retried
			// cleanup can converge
			if !util.IsAlreadyReleased(err) {
				return fmt.Errorf("(vm.cleanupNetworkInterface) [%s/%s] error releasing ip from ipam: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			}
		}
	}

	pool, poolErr := c.cache.Get("pool", netCfg.NetworkName)
	if poolErr != nil {
		// without the pool object the status entry cannot be removed; the
		// cleanup of the reached state still continues so the durable
		// update for the remaining interfaces can proceed
		log.Errorf("(vm.cleanupNetworkInterface) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, poolErr)
		c.metrics.UpdateLogStatus("error")

		return
	}

	if statusErr := c.updateIPPoolStatus(
		DELETE,
		vmnetcfg.Namespace,
		vmnetcfg.Spec.VMName,
		netCfg.IPAddress,
		netCfg.NetworkName,
		netCfg.MACAddress,
		pool.(kihv1.IPPool).Name,
	); statusErr != nil {
		// the status entry of another owner is not this vm's to remove;
		// replaying the cleanup must not abort the durable update over it
		if errors.Is(statusErr, util.ErrForeignOwner) {
			log.Warnf("(vm.cleanupNetworkInterface) [%s/%s] the allocation of ip %s in the %s status belongs to another owner, leaving the entry",
				vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, pool.(kihv1.IPPool).Name)
			c.metrics.UpdateLogStatus("warning")

			return
		}

		return fmt.Errorf("(vm.cleanupNetworkInterface) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, statusErr.Error())
	}

	return
}

func (c *Controller) updateIPPoolStatus(event string, vmnetcfgNamespace string, vmnetcfgVMName string, ip string, networkName string, hwAddr string, poolName string) (err error) {
	// Retry max 10 attempts for conflicts
	maxRetries := 10
	retryDelay := 100 * time.Millisecond

	for retry := 0; retry < maxRetries; retry++ {
		currentPool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(context.TODO(), poolName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("cannot get IPPool %s: %s", poolName, err.Error())
		}

		updatedPool := currentPool.DeepCopy()
		updatedAllocated := make(map[string]string)

		// allocation references carry the canonical mac address spelling so
		// add and delete computations agree on the owner identity
		ownerRef := fmt.Sprintf("%s/%s [%s]", vmnetcfgNamespace, vmnetcfgVMName, util.CanonicalHWAddr(hwAddr))

		switch event {
		case ADD:
			for k, v := range currentPool.Status.IPv4.Allocated {
				if k == ip {
					if v == ownerRef {
						// the allocation reference is already recorded, so a
						// retry after a partially applied update treats it as
						// done
						return nil
					}

					return fmt.Errorf("ip %s already found in IPPool status", ip)
				}
				updatedAllocated[k] = v
			}
			updatedAllocated[ip] = ownerRef
		case DELETE:
			for k, v := range currentPool.Status.IPv4.Allocated {
				if k != ip {
					updatedAllocated[k] = v
				}
			}

			if existing, exists := currentPool.Status.IPv4.Allocated[ip]; exists && existing != ownerRef {
				return fmt.Errorf("allocation for ip %s belongs to %s, not removing it from the %s status: %w", ip, existing, poolName, util.ErrForeignOwner)
			}
		default:
			// any unknown event must never reach the persisted status:
			// falling through would rebuild the allocation map from scratch
			// and erase every live allocation entry
			return fmt.Errorf("unsupported ippool status event %s for ip %s in pool %s", event, ip, poolName)
		}
		updatedPool.Status.IPv4.Allocated = updatedAllocated
		updatedPool.Status.IPv4.Used = c.ipam.Used(networkName)
		updatedPool.Status.IPv4.Available = c.ipam.Available(networkName)
		updatedPool.Status.LastUpdate = metav1.Now()

		if _, err := c.kihClientset.KubevirtiphelperV1().IPPools().UpdateStatus(context.TODO(), updatedPool, metav1.UpdateOptions{}); err == nil {
			// return success
			return nil
		} else {
			// If it's a conflict error try again
			if strings.Contains(err.Error(), "please apply your changes to the latest version and try again") {
				if retry == maxRetries-1 {
					return fmt.Errorf("cannot update status of IPPool %s after %d retries: %s", updatedPool.Name, maxRetries, err.Error())
				}
			} else {
				return fmt.Errorf("cannot update status of IPPool %s: %s", updatedPool.Name, err.Error())
			}

			// Wait before retrying
			log.Warnf("(vm.updateIPPoolStatus) [%s/%s] cannot update status of IPPool %s after %d attempt(s), retrying in a bit",
				vmnetcfgNamespace, vmnetcfgVMName, updatedPool.Name, retry+1)
			time.Sleep(time.Duration(retry) * retryDelay)
			continue
		}
	}

	return fmt.Errorf("cannot update status of IPPool %s after max retries: %s", poolName, err.Error())
}
