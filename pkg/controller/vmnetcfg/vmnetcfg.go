package vmnetcfg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	log "github.com/sirupsen/logrus"
)

// allocatedNetworkConfig tracks one fully applied interface allocation of a
// vmnetcfg object so it can be reverted if the durable object update fails.
type allocatedNetworkConfig struct {
	macAddress  string
	networkName string
	ipAddress   string
	poolName    string
}

// rollbackNetworkAllocation reverts the allocation side effects of a single
// network interface of a vmnetcfg object.
func (c *Controller) rollbackNetworkAllocation(vmnetcfg *kihv1.VirtualMachineNetworkConfig, allocated allocatedNetworkConfig) {
	ref := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)

	if err := c.updateIPPoolStatus(
		DELETE,
		vmnetcfg.Namespace,
		vmnetcfg.Spec.VMName,
		allocated.ipAddress,
		allocated.networkName,
		allocated.macAddress,
		allocated.poolName,
	); err != nil {
		log.Errorf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] failed to revert the ippool status for ip %s: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, allocated.ipAddress, err)
		c.metrics.UpdateLogStatus("error")
	}

	if err := c.dhcp.DeleteLeaseOwnedBy(allocated.macAddress, ref); err != nil && !errors.Is(err, dhcp.ErrLeaseNotFound) {
		log.Errorf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] failed to revert the dhcp lease for hwaddr %s: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, allocated.macAddress, err)
		c.metrics.UpdateLogStatus("error")
	}

	if err := c.ipam.ReleaseIP(allocated.networkName, allocated.ipAddress); err != nil && !util.IsAlreadyReleased(err) {
		log.Errorf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] failed to revert the ipam allocation for ip %s: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, allocated.ipAddress, err)
		c.metrics.UpdateLogStatus("error")
	}
}

func (c *Controller) updateVirtualMachineNetworkConfig(eventAction string, vmnetcfg *kihv1.VirtualMachineNetworkConfig) (err error) {
	var networkChange bool = false
	var skipNic bool = false

	log.Tracef("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] processing new vmnetcfg [%+v]",
		vmnetcfg.Namespace, vmnetcfg.Name, vmnetcfg)

	// cleanup the network configuration if the object is marked for deletion
	if vmnetcfg.ObjectMeta.DeletionTimestamp != nil {
		if err := c.cleanupVirtualMachineNetworkConfig(vmnetcfg); err != nil {
			return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] failed to cleanup vmnetcfg: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
		}

		return
	}

	newVmNetCfg := vmnetcfg.DeepCopy()
	newVmNetCfgs := []kihv1.NetworkConfig{}
	newNetCfgStatusList := []kihv1.NetworkConfigStatus{}

	// allocations which are applied to dhcp/ipam/ippool status while
	// processing this object; reverted when the object update fails
	appliedAllocations := []allocatedNetworkConfig{}

	for _, v := range vmnetcfg.Spec.NetworkConfig {
		pool, err := c.cache.Get("pool", v.NetworkName)
		if err != nil {
			return err
		}

		// create a fresh nic status
		netcfgStatus := kihv1.NetworkConfigStatus{}
		netcfgStatus.MACAddress = v.MACAddress
		netcfgStatus.NetworkName = v.NetworkName

		// skip the network interface updates when it has a status ERROR
		skipNic = false
		for _, nic := range vmnetcfg.Status.NetworkConfig {
			if v.MACAddress == nic.MACAddress && v.NetworkName == nic.NetworkName && nic.Status == "ERROR" {
				netcfgStatus.Status = nic.Status
				netcfgStatus.Message = nic.Message
				newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)

				skipNic = true

				break
			}
		}

		// check for duplicate mac address registrations
		if !skipNic && c.dhcp.CheckLease(v.MACAddress) {
			lease := c.dhcp.GetLease(v.MACAddress)
			if lease.Reference != fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName) {
				log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] hwaddr %s belongs to %s",
					vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, lease.Reference)
				c.metrics.UpdateLogStatus("error")

				netcfgStatus.Status = "ERROR"
				netcfgStatus.Message = "macaddress belongs to another vm"
				newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)

				skipNic = true
			}
		}

		// check the added vmnetcfgs which are new and don't have a networkconfig status
		if !skipNic && eventAction == ADD && len(vmnetcfg.Status.NetworkConfig) == 0 {
			log.Tracef("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] vmnetcfg.CreateTimestamp=%s, pool.LastUpdateBeforeStart=%s, pool.lastUpdate=%s",
				vmnetcfg.Namespace, vmnetcfg.Name, vmnetcfg.CreationTimestamp,
				pool.(kihv1.IPPool).Status.LastUpdateBeforeStart.Time,
				pool.(kihv1.IPPool).Status.LastUpdate.Time)

			// put the network interfaces in ERROR state when the vmnetcfg is (manually) created between
			// the last status update before the program was stopped and the restart of the program
			// this could cause a possible hijack of ip addresses which are already registered in existing vmnetcfgs
			// this should be automatically handled by the vm controller and not manually when the program is not running
			if vmnetcfg.CreationTimestamp.After(pool.(kihv1.IPPool).Status.LastUpdateBeforeStart.Time) &&
				pool.(kihv1.IPPool).Status.LastUpdate.After(vmnetcfg.CreationTimestamp.Time) {
				log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] vmnetcfg was manually created after this program was (re)started, preventing possible ip hijack",
					vmnetcfg.Namespace, vmnetcfg.Name)
				c.metrics.UpdateLogStatus("error")

				netcfgStatus.Status = "ERROR"
				netcfgStatus.Message = "vmnetcfg was manually created after this program was (re)started, preventing possible ip hijack"
				newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)

				skipNic = true
			}
		}

		if skipNic {
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] network interface has an error status, skipping updates",
				vmnetcfg.Namespace, vmnetcfg.Name)
			c.metrics.UpdateLogStatus("error")

			newVmNetCfgs = append(newVmNetCfgs, v)

			continue
		}

		// handle ip changes in the vmnetcfg object
		if c.dhcp.CheckLease(v.MACAddress) {
			lease := c.dhcp.GetLease(v.MACAddress)
			if lease.ClientIP.String() != v.IPAddress {
				log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] ip address update found for hwaddr=%s, oldip=%s, newip=%s, starting cleanup of old ip address",
					vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, lease.ClientIP.String(), v.IPAddress)
				c.metrics.UpdateLogStatus("warning")

				oldNetcfg := kihv1.NetworkConfig{}
				oldNetcfg.NetworkName = v.NetworkName
				oldNetcfg.MACAddress = v.MACAddress
				oldNetcfg.IPAddress = lease.ClientIP.String()

				if err := c.cleanupNetworkInterface(vmnetcfg, &oldNetcfg, false); err != nil {
					return err
				}
			} else {
				log.Debugf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] hwaddr %s already exists in the leases, skipping interface",
					vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress)

				newVmNetCfgs = append(newVmNetCfgs, v)

				// set the old status
				for _, nic := range vmnetcfg.Status.NetworkConfig {
					if v.MACAddress == nic.MACAddress && v.NetworkName == nic.NetworkName {
						netcfgStatus.Status = nic.Status
						netcfgStatus.Message = nic.Message
						newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)

						break
					}
				}

				continue
			}
		}

		// if v.IPAddress is not empty we register it else we get a new one
		ip, err := c.ipam.GetIP(v.NetworkName, v.IPAddress)
		if err != nil {
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] ipam error: %s, skipping interface",
				vmnetcfg.Namespace, vmnetcfg.Name, err)
			c.metrics.UpdateLogStatus("error")

			newVmNetCfgs = append(newVmNetCfgs, v)

			netcfgStatus.Status = "ERROR"
			netcfgStatus.Message = err.Error()
			newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)

			continue
		}

		ref := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)
		if err := c.dhcp.AddLease(
			v.MACAddress,
			pool.(kihv1.IPPool).Spec.NetworkName,
			ip,
			ref,
		); err != nil {
			// dhcp must not serve the address when its owner reference
			// cannot be registered, drop the ipam claim again
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] error registering the dhcp lease: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err)
			c.metrics.UpdateLogStatus("error")

			c.rollbackNetworkAllocation(vmnetcfg, allocatedNetworkConfig{
				macAddress:  v.MACAddress,
				networkName: v.NetworkName,
				ipAddress:   ip,
				poolName:    pool.(kihv1.IPPool).Name,
			})

			return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] cannot register the dhcp lease for hwaddr %s: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, err.Error())
		}

		n := kihv1.NetworkConfig{}
		n.IPAddress = ip
		n.MACAddress = v.MACAddress
		n.NetworkName = v.NetworkName
		newVmNetCfgs = append(newVmNetCfgs, n)

		netcfgStatus.Status = "OK"
		netcfgStatus.Message = "IP address successfully allocated"
		newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)

		if err := c.updateIPPoolStatus(
			ADD,
			vmnetcfg.Namespace,
			vmnetcfg.Spec.VMName,
			ip,
			v.NetworkName,
			v.MACAddress,
			pool.(kihv1.IPPool).Name,
		); err != nil {
			// the lease would be served while the durable allocation state
			// is missing, revert the whole allocation of this interface
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err)
			c.metrics.UpdateLogStatus("error")

			c.rollbackNetworkAllocation(vmnetcfg, allocatedNetworkConfig{
				macAddress:  v.MACAddress,
				networkName: v.NetworkName,
				ipAddress:   ip,
				poolName:    pool.(kihv1.IPPool).Name,
			})
			return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] cannot update the IPPool %s status for ip %s: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, pool.(kihv1.IPPool).Name, ip, err.Error())
		}

		if err := c.updateIPPoolMetrics(pool.(kihv1.IPPool).Name); err != nil {
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err)
			c.metrics.UpdateLogStatus("error")
		}

		appliedAllocations = append(appliedAllocations, allocatedNetworkConfig{
			macAddress:  v.MACAddress,
			networkName: v.NetworkName,
			ipAddress:   ip,
			poolName:    pool.(kihv1.IPPool).Name,
		})

		networkChange = true
	}

	newVmnetCfgStatus := kihv1.VirtualMachineNetworkConfigStatus{}
	newVmnetCfgStatus.NetworkConfig = newNetCfgStatusList

	if !networkChange {
		log.Debugf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] no network changes detected, skipping object update",
			vmnetcfg.Namespace, vmnetcfg.Name)

		// only update the status and metrics when the status.networkconfig array has items
		if len(newVmnetCfgStatus.NetworkConfig) > 0 {
			if err := c.updateVirtualMachineNetworkConfigStatus(vmnetcfg, &newVmnetCfgStatus); err != nil {
				log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
					vmnetcfg.ObjectMeta.Namespace, vmnetcfg.ObjectMeta.Name, err)
				c.metrics.UpdateLogStatus("error")
			}

			if err := c.updateVirtualMachineNetworkConfigMetrics(vmnetcfg.Namespace, vmnetcfg.Name); err != nil {
				log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err)
				c.metrics.UpdateLogStatus("error")
			}
		}

		return
	}

	newVmNetCfg.Spec.NetworkConfig = newVmNetCfgs

	log.Tracef("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] updating vmnetcfg object to [%+v]",
		vmnetcfg.Namespace, vmnetcfg.Name, newVmNetCfg)

	vmNetCfgObj, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(newVmNetCfg.Namespace).Update(context.TODO(), newVmNetCfg, metav1.UpdateOptions{})
	if err != nil {
		// the durable object still holds the previous configuration; revert
		// dhcp/ipam/ippool status so they cannot serve unrecorded addresses
		for i := len(appliedAllocations) - 1; i >= 0; i-- {
			c.rollbackNetworkAllocation(vmnetcfg, appliedAllocations[i])
		}

		return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] cannot update VirtualMachineNetworkConfig object: %s",
			newVmNetCfg.Namespace, newVmNetCfg.Name, err.Error())
	}

	log.Debugf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] successfully processed the network configuration",
		vmNetCfgObj.ObjectMeta.Namespace, vmNetCfgObj.ObjectMeta.Name)

	if err := c.updateVirtualMachineNetworkConfigStatus(vmNetCfgObj, &newVmnetCfgStatus); err != nil {
		log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
			vmNetCfgObj.ObjectMeta.Namespace, vmNetCfgObj.ObjectMeta.Name, err)
		c.metrics.UpdateLogStatus("error")
	}

	if err := c.updateVirtualMachineNetworkConfigMetrics(vmNetCfgObj.Namespace, vmNetCfgObj.Name); err != nil {
		log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
			vmNetCfgObj.Namespace, vmNetCfgObj.Name, err)
		c.metrics.UpdateLogStatus("error")
	}

	return
}

func (c *Controller) cleanupNetworkInterface(vmnetcfg *kihv1.VirtualMachineNetworkConfig, netCfg *kihv1.NetworkConfig, deleting bool) (err error) {
	log.Debugf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] cleaning interface with hwaddr=%s, networkname=%s, ipaddress=%s",
		vmnetcfg.Namespace, vmnetcfg.Name, netCfg.MACAddress, netCfg.NetworkName, netCfg.IPAddress)

	ref := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)

	// a delayed cleanup must not tear down allocations which were assigned
	// to another vm in the meantime: the lease deletion re-validates the
	// owner under the same lock, so the decision cannot race a concurrent
	// reassignment. for a live vmnetcfg a foreign owner aborts the sync so
	// the changed state is re-inspected on the next update; during deletion
	// the foreign allocation is left to its owner and the remaining own
	// allocations are cleaned so the finalizer completes
	if err := c.dhcp.DeleteLeaseOwnedBy(netCfg.MACAddress, ref); err != nil {
		switch {
		case errors.Is(err, dhcp.ErrLeaseForeignOwner):
			if !deleting {
				return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			}

			log.Warnf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] %s, skipping the dhcp cleanup of it",
				vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			c.metrics.UpdateLogStatus("warning")

		case errors.Is(err, dhcp.ErrLeaseNotFound):
			// no lease left for this interface: the cleanup already
			// converged, nothing to revert

		default:
			return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] error deleting lease from dhcp: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
		}
	}

	// freeing an ip which is leased to another vm would leave the other
	// lease serving an address ipam could reissue to a third client; ipam
	// itself holds no owner references, so this stays a snapshot check
	// without an owner-validated release primitive
	releaseIP := false
	if netCfg.IPAddress != "" {
		releaseIP = true

		if leaseHwAddr, lease, found := c.dhcp.GetLeaseByIP(netCfg.IPAddress); found && lease.Reference != ref {
			if !deleting {
				return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] ip %s belongs to %s via hwaddr %s, aborting cleanup to preserve the allocation",
					vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, lease.Reference, leaseHwAddr)
			}

			log.Warnf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] ip %s belongs to %s via hwaddr %s, skipping the ipam release of it",
				vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, lease.Reference, leaseHwAddr)
			c.metrics.UpdateLogStatus("warning")

			releaseIP = false
		}
	}

	if releaseIP {
		if err := c.ipam.ReleaseIP(netCfg.NetworkName, netCfg.IPAddress); err != nil {
			// already-free addresses are treated as done so a retried
			// cleanup can converge
			if !util.IsAlreadyReleased(err) {
				return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] error releasing ip from ipam: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			}
		}
	}

	pool, poolErr := c.cache.Get("pool", netCfg.NetworkName)
	if poolErr != nil {
		// without the pool object the status entry cannot be removed; the
		// cleanup of the reached state still continues
		log.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, poolErr)
		c.metrics.UpdateLogStatus("error")

		return
	}

	if err := c.updateIPPoolStatus(
		DELETE,
		vmnetcfg.Namespace,
		vmnetcfg.Spec.VMName,
		netCfg.IPAddress,
		netCfg.NetworkName,
		netCfg.MACAddress,
		pool.(kihv1.IPPool).Name,
	); err != nil {
		// the entry of another owner is not this vmnetcfg's to remove; a
		// deleting object must still finish, so the entry is reported and
		// kept
		if deleting && errors.Is(err, util.ErrForeignOwner) {
			log.Warnf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] the allocation of ip %s in the %s status belongs to another owner, leaving the entry",
				vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress, pool.(kihv1.IPPool).Name)
			c.metrics.UpdateLogStatus("warning")
		} else {
			return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
		}
	}

	return
}

func (c *Controller) cleanupVirtualMachineNetworkConfig(vmnetcfg *kihv1.VirtualMachineNetworkConfig) (err error) {
	log.Debugf("(vmnetcfg.cleanupVirtualMachineNetworkConfig) [%s/%s] starting cleanup for vmnetcfg",
		vmnetcfg.Namespace, vmnetcfg.Name)
	for i := range vmnetcfg.Spec.NetworkConfig {
		if err := c.cleanupNetworkInterface(vmnetcfg, &vmnetcfg.Spec.NetworkConfig[i], true); err != nil {
			// the finalizers stay so a failed cleanup is retried
			return fmt.Errorf("(vmnetcfg.cleanupVirtualMachineNetworkConfig) [%s/%s] %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
		}
	}

	c.deleteVirtualMachineNetworkConfigMetrics(vmnetcfg)

	updatedVmNetCfg := vmnetcfg.DeepCopy()
	newFinalizers := []string{}
	for i := 0; i < len(vmnetcfg.ObjectMeta.Finalizers); i++ {
		// TODO: remove the "kubevirtiphelper" finalizer in the next minor release
		if vmnetcfg.ObjectMeta.Finalizers[i] != "kubevirtiphelper" && vmnetcfg.ObjectMeta.Finalizers[i] != "kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup" {
			newFinalizers = append(newFinalizers, vmnetcfg.ObjectMeta.Finalizers[i])
		}
	}

	if len(newFinalizers) == len(updatedVmNetCfg.ObjectMeta.Finalizers) {
		return
	}

	updatedVmNetCfg.ObjectMeta.Finalizers = newFinalizers
	vmNetCfgObj, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(updatedVmNetCfg.Namespace).Update(context.TODO(), updatedVmNetCfg, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("(vmnetcfg.cleanupVirtualMachineNetworkConfig) [%s/%s] cannot remove finalizers for VirtualMachineNetworkConfig object: %s",
			updatedVmNetCfg.Namespace, updatedVmNetCfg.Name, err.Error())
	}

	log.Debugf("(vmnetcfg.cleanupVirtualMachineNetworkConfig) [%s/%s] succesfully removed finalizers for VirtualMachineNetworkConfig object",
		vmNetCfgObj.Namespace, vmNetCfgObj.Name)

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
			log.Warnf("(vmnetcfg.updateIPPoolStatus) [%s/%s] cannot update status of IPPool %s after %d attempt(s), retrying in a bit",
				vmnetcfgNamespace, vmnetcfgVMName, updatedPool.Name, retry+1)
			time.Sleep(time.Duration(retry) * retryDelay)
			continue
		}
	}

	return fmt.Errorf("cannot update status of IPPool %s after max retries: %s", poolName, err.Error())
}

func (c *Controller) updateVirtualMachineNetworkConfigStatus(vmnetcfg *kihv1.VirtualMachineNetworkConfig, vmnetcfgStatus *kihv1.VirtualMachineNetworkConfigStatus) (err error) {
	vmnetcfg.Status = *vmnetcfgStatus

	vmNetCfgStatusObj, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmnetcfg.Namespace).UpdateStatus(context.TODO(), vmnetcfg, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("cannot update status of VirtualMachineNetworkConfig: %s", err.Error())
	}

	log.Debugf("(vmnetcfg.updateVirtualMachineNetworkConfigStatus) [%s/%s] successfully updated status of vmnetcfg object",
		vmNetCfgStatusObj.Namespace, vmNetCfgStatusObj.Name)

	return
}

func (c *Controller) updateIPPoolMetrics(poolName string) (err error) {
	pool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(context.TODO(), poolName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot get IPPool %s: %s", poolName, err.Error())
	}

	c.metrics.UpdateIPPoolUsed(pool.Name, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, pool.Status.IPv4.Used)
	c.metrics.UpdateIPPoolAvailable(pool.Name, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, pool.Status.IPv4.Available)

	return
}

func (c *Controller) updateVirtualMachineNetworkConfigMetrics(vmnetcfgNamespace string, vmnetcfgName string) (err error) {
	vmnetcfg, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmnetcfgNamespace).Get(context.TODO(), vmnetcfgName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfigMetrics) cannot get VirtualMachineNetworkConfig %s/%s: %s",
			vmnetcfgNamespace, vmnetcfgName, err.Error())
	}

	c.metrics.DeleteVmNetCfgStatus(fmt.Sprintf("%s/%s", vmnetcfgNamespace, vmnetcfgName))
	for _, netstat := range vmnetcfg.Status.NetworkConfig {
		for _, netcfg := range vmnetcfg.Spec.NetworkConfig {
			if netstat.MACAddress == netcfg.MACAddress {
				c.metrics.UpdateVmNetCfgStatus(
					fmt.Sprintf("%s/%s", vmnetcfgNamespace, vmnetcfgName),
					netstat.NetworkName,
					netstat.MACAddress,
					netcfg.IPAddress,
					netstat.Status,
				)
			}
		}
	}

	return
}

func (c *Controller) deleteVirtualMachineNetworkConfigMetrics(vmnetcfg *kihv1.VirtualMachineNetworkConfig) {
	c.metrics.DeleteVmNetCfgStatus(fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Name))
}
