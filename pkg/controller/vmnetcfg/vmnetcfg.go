package vmnetcfg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	ipam "github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
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

// rollbackNetworkAllocation reverts the allocation side effects of a
// single network interface of a vmnetcfg object. the releases run before
// the pool status write, so the persisted counters are computed from an
// ipam state which already excludes the unwound allocation, and the pool
// metrics republish the settled accounting.
func (c *Controller) rollbackNetworkAllocation(vmnetcfg *kihv1.VirtualMachineNetworkConfig, allocated allocatedNetworkConfig) {
	ref := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)

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

	if err := c.updateIPPoolMetrics(allocated.poolName); err != nil {
		log.Errorf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, err)
		c.metrics.UpdateLogStatus("error")
	}
}

// rollbackAppliedAllocations reverts the queued allocations of the vmnetcfg
// object, newest first, so a pre-commit failure cannot leave live allocations
// for a durable state which was never persisted. restores of assignments
// which the stored spec already records are never queued by the caller and
// stay applied: their addresses must remain reserved for the running guests.
func (c *Controller) rollbackAppliedAllocations(vmnetcfg *kihv1.VirtualMachineNetworkConfig, applied []allocatedNetworkConfig) {
	for i := len(applied) - 1; i >= 0; i-- {
		c.rollbackNetworkAllocation(vmnetcfg, applied[i])
	}
}

// releaseStaleNicClaim releases the ipam claim of a nic whose lease
// vanished during this reconciliation, but only while the reservation
// still carries this owner's reference: a successor which took the freed
// address over in the meantime (a fresh anonymous allocation or another
// owner's named reclaim) is never released by the stale cleanup. the
// converged outcomes (a foreign owner, an already-free address, a subnet
// which is gone) are tolerated: the release is an in-memory operation
// without a transient failure mode, so it cannot leave the claim behind
// retriable, and a process restart converges as well because the removed
// nic records the address nowhere anymore.
func (c *Controller) releaseStaleNicClaim(networkName string, ip string, ownerRef string) {
	if err := c.ipam.ReleaseIPOwnedBy(networkName, ip, ownerRef); err != nil &&
		!errors.Is(err, ipam.ErrIPForeignOwner) &&
		!util.IsAlreadyReleased(err) {
		log.Errorf("(vmnetcfg.releaseStaleNicClaim) [%s] cannot release the stale claim of ip %s in network %s: %s",
			ownerRef, ip, networkName, err)
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
	// processing this object; reverted when the object update fails.
	// restores of addresses which the stored spec already records never
	// enter this list: releasing them would free addresses which the
	// guests still use while the durable object keeps claiming them
	appliedAllocations := []allocatedNetworkConfig{}

	// addresses which the stored spec already records (mac, networkname
	// and ip): applying them again only restores the previous assignment
	durableAllocations := make(map[string]bool)
	for _, v := range vmnetcfg.Spec.NetworkConfig {
		if v.IPAddress != "" {
			durableAllocations[v.MACAddress+"/"+v.NetworkName+"/"+v.IPAddress] = true
		}
	}

	// rememberApplied queues an allocation of this sync for the rollback
	// unless the stored spec already records it: a failed sync unwinds only
	// the not-yet-durable claims and keeps the reservations of already
	// persisted assignments protected. a contested claim whose address the
	// pool status records for another owner is invalid even when the spec
	// requests it, so it is always queued for the release.
	rememberApplied := func(poolName string, macAddress string, networkName string, ipAddress string, contested bool) {
		if !contested && durableAllocations[macAddress+"/"+networkName+"/"+ipAddress] {
			return
		}

		appliedAllocations = append(appliedAllocations, allocatedNetworkConfig{
			macAddress:  macAddress,
			networkName: networkName,
			ipAddress:   ipAddress,
			poolName:    poolName,
		})
	}

	// restoreErr records the first per-interface failure of this sync whose
	// repair needs a retry (a network without a registered pool, or an
	// unusable macaddress), while the remaining interfaces are still
	// processed: one interface's failure must never block the restoration
	// of the other interfaces (their assignments are protected through this
	// same sync). the error is reported after every interface was handled;
	// the startup gate counts the object through its settled classification
	// and the resynced retry converges once the failure is repaired.
	var restoreErr error

	for _, v := range vmnetcfg.Spec.NetworkConfig {
		// create a fresh nic status
		netcfgStatus := kihv1.NetworkConfigStatus{}
		netcfgStatus.MACAddress = v.MACAddress
		netcfgStatus.NetworkName = v.NetworkName

		pool, poolErr := c.cache.Get("pool", v.NetworkName)
		if poolErr != nil {
			// keep the durable spec and the previous status entry untouched,
			// skip this interface and continue with the next one
			if restoreErr == nil {
				restoreErr = poolErr
			}

			newVmNetCfgs = append(newVmNetCfgs, v)

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

		// validate the hardware identity before the address is claimed: an
		// unusable macaddress must never consume a reservation, otherwise
		// the corrected object could not be served anymore (the previous
		// claim would sit in the bitmap without an owner able to release
		// it). the interface is skipped, its durable spec entry and the
		// previous status are kept, and the sync reports the failure so
		// the retried resync converges once the identity is corrected.
		if _, macErr := net.ParseMAC(v.MACAddress); macErr != nil {
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] invalid macaddress %q for network %s, skipping interface",
				vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, v.NetworkName)
			c.metrics.UpdateLogStatus("error")

			newVmNetCfgs = append(newVmNetCfgs, v)

			for _, nic := range vmnetcfg.Status.NetworkConfig {
				if v.MACAddress == nic.MACAddress && v.NetworkName == nic.NetworkName {
					netcfgStatus.Status = nic.Status
					netcfgStatus.Message = nic.Message
					newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)

					break
				}
			}

			if restoreErr == nil {
				restoreErr = fmt.Errorf("invalid macaddress %q for network %s", v.MACAddress, v.NetworkName)
			}

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

				if cleanupErr := c.cleanupNetworkInterface(vmnetcfg, &oldNetcfg, false); cleanupErr != nil {
					// the transition could not complete: defer the failure
					// and keep processing the remaining interfaces, so a
					// failed cleanup of one interface never blocks the
					// restoration of the others
					log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] failed to clean up the old address of hwaddr %s: %s",
						vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, cleanupErr)
					c.metrics.UpdateLogStatus("error")

					newVmNetCfgs = append(newVmNetCfgs, v)

					for _, nic := range vmnetcfg.Status.NetworkConfig {
						if v.MACAddress == nic.MACAddress && v.NetworkName == nic.NetworkName {
							netcfgStatus.Status = nic.Status
							netcfgStatus.Message = nic.Message
							newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)

							break
						}
					}

					if restoreErr == nil {
						restoreErr = cleanupErr
					}

					continue
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
				// pin the lease's address in the allocator under the
				// verified owner reference: a binding whose lease survived
				// but whose ipam claim was lost keeps the address
				// unavailable to fresh allocations. the adoption is guarded
				// by the live lease - the lease identity is re-validated
				// under the dhcp lock around the allocator mutation,
				// because the vm and the vmnetcfg controllers run
				// independently and a concurrent cleanup can remove the
				// lease and release the address between the snapshot and
				// the adopt: an unguarded adopt would recreate the released
				// reservation for the removed nic. the guarded adopt is
				// idempotent for the own claim and promotes an anonymous
				// allocation of a failed earlier sync; a foreign named
				// allocation fails the sync visibly like the conflicting
				// ownership record does.
				ownerRef := util.AllocationRef(vmnetcfg.Namespace, vmnetcfg.Spec.VMName, v.MACAddress)
				vmRef := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)
				adoptErr := c.dhcp.WithOwnedLease(v.MACAddress, vmRef, func(clientIP string) error {
					return c.ipam.AdoptIP(v.NetworkName, clientIP, ownerRef)
				})
				if adoptErr != nil {
					if errors.Is(adoptErr, dhcp.ErrLeaseNotFound) || errors.Is(adoptErr, dhcp.ErrLeaseForeignOwner) {
						// the lease vanished or was reassigned between the
						// snapshot and the adoption: the nic is being
						// removed by a concurrent cleanup. the guarded
						// adopt recreated nothing, and a claim an earlier
						// reconciliation of this binding left behind is
						// released while it still carries this owner's
						// reference, so the removed nic cannot keep the
						// address blocked and a successor is never touched
						log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] the lease of hwaddr %s vanished before its address could be adopted, releasing the stale claim of the removed nic",
							vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress)
						c.metrics.UpdateLogStatus("warning")

						c.releaseStaleNicClaim(v.NetworkName, lease.ClientIP.String(), ownerRef)

						continue
					}

					// without the subnet in the allocator no fresh
					// allocation is possible either, so there is no
					// unprotected window: leave the pin to the converging
					// registration retry instead of failing a binding which
					// keeps serving by its lease
					if errors.Is(adoptErr, ipam.ErrSubnetNotFound) {
						log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] cannot pin the leased address %s: %s",
							vmnetcfg.Namespace, vmnetcfg.Name, lease.ClientIP.String(), adoptErr)
					} else {
						log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] ipam re-claim error: %s, skipping interface",
							vmnetcfg.Namespace, vmnetcfg.Name, adoptErr)
						c.metrics.UpdateLogStatus("error")

						if restoreErr == nil {
							restoreErr = adoptErr
						}
					}
				}

				// the reservation is already applied: repair the durable
				// pool ownership record, which an earlier status write
				// failure may have left missing (the lease and the ipam
				// claim were retained while the record was never rebuilt).
				// a matching owner entry is confirmed read-only, a missing
				// entry is rebuilt, and a conflicting entry fails the sync
				// visibly (like the bind path does) instead of serving
				// silently with a leftover claim. a pool which is gone
				// surfaces as a cache miss before this path (its deletion
				// removes the registration), so an IPPool GET failure here
				// is transient and the resynced retry converges through the
				// pool-miss handling.

				// this reconciliation can still hold a stale snapshot whose
				// nic is concurrently removed: the lease can vanish between
				// the guarded adoption and this write. the repair must not
				// resurrect ownership state which the raced cleanup removes,
				// so the lease is re-validated immediately before the write
				// and the record is verified again afterwards: a vanished
				// lease skips the repair and releases the stale claim, and a
				// lease which vanishes between the write and the
				// verification is undone by the owner-validated compensating
				// delete and release (a meanwhile recorded foreign owner or
				// a successor's allocation is never clobbered - the own
				// reference only removes the own state).
				if !c.dhcp.CheckLease(v.MACAddress) || c.dhcp.GetLease(v.MACAddress).Reference != vmRef {
					log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] the lease of hwaddr %s vanished during the ownership repair, skipping it and releasing the stale claim of the removed nic",
						vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress)
					c.metrics.UpdateLogStatus("warning")

					c.releaseStaleNicClaim(v.NetworkName, lease.ClientIP.String(), ownerRef)

					continue
				}

				var repairErr error
				if err := c.updateIPPoolStatus(
					ADD,
					vmnetcfg.Namespace,
					vmnetcfg.Spec.VMName,
					v.IPAddress,
					v.NetworkName,
					v.MACAddress,
					pool.(kihv1.IPPool).Name,
				); err != nil {
					log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
						vmnetcfg.Namespace, vmnetcfg.Name, err)
					c.metrics.UpdateLogStatus("error")

					repairErr = err
				}

				if repairErr == nil &&
					(!c.dhcp.CheckLease(v.MACAddress) || c.dhcp.GetLease(v.MACAddress).Reference != vmRef) {
					// the cleanup of the vm controller removed the lease
					// between the repair decision and the durable write:
					// undo the resurrected ownership record before it
					// blocks the address for a later binding
					log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] the lease of hwaddr %s was removed by a concurrent cleanup during the ownership repair, undoing the record",
						vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress)
					c.metrics.UpdateLogStatus("warning")

					if err := c.updateIPPoolStatus(
						DELETE,
						vmnetcfg.Namespace,
						vmnetcfg.Spec.VMName,
						v.IPAddress,
						v.NetworkName,
						v.MACAddress,
						pool.(kihv1.IPPool).Name,
					); err != nil && !errors.Is(err, util.ErrForeignOwner) {
						// a foreign owner which recorded the address in
						// the meantime is protected by the owner
						// validation; any other failure is retriable
						log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] cannot undo the ownership record after the raced cleanup: %s",
							vmnetcfg.Namespace, vmnetcfg.Name, err)
						c.metrics.UpdateLogStatus("error")

						repairErr = err
					}

					// the claim this reconciliation adopted for the removed
					// nic is not justified by its lease anymore: release it
					// while it still carries this owner's reference. the
					// concurrent cleanup normally released it already (the
					// owner-validated release treats that as converged) but
					// a cleanup which skipped its own release must not
					// leave the claim behind ownerless
					c.releaseStaleNicClaim(v.NetworkName, lease.ClientIP.String(), ownerRef)
				}

				if repairErr != nil && restoreErr == nil {
					restoreErr = repairErr
				}

				continue
			}
		}

		// if v.IPAddress is not empty we re-claim it else we get a new one.
		// the re-claim carries the owner reference so a registration which
		// pinned the persisted pool-status claims into a fresh allocator
		// accepts the restore of the recorded owner idempotently, while a
		// foreign fresh or seeded allocation is rejected instead of being
		// silently taken.
		var ip string
		var err error
		if v.IPAddress != "" {
			ip, err = c.ipam.ReclaimIP(v.NetworkName, v.IPAddress, util.AllocationRef(vmnetcfg.Namespace, vmnetcfg.Spec.VMName, v.MACAddress))
		} else {
			if *c.appStatus == APP_INIT {
				// two-phase startup replay: a pending nic without a
				// recorded address must not allocate during the
				// initialization replay, because the recorded
				// assignments of the other objects are still waiting
				// for their own sync and the pool status does not pin
				// an address whose record write was lost before the
				// restart. the object still settles for the gate and
				// the controller requeues it once every object's
				// durable assignments are restored.
				log.Infof("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] deferring the fresh allocation of hwaddr %s until the initialization replay finished",
					vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress)
				c.metrics.UpdateLogStatus("warning")
				c.deferInitAllocation(fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Name))

				newVmNetCfgs = append(newVmNetCfgs, v)

				// a pending nic carries no previous status entry; a
				// deferred nic with one keeps it untouched
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

			ip, err = c.ipam.GetIP(v.NetworkName, "")
		}
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
			// cannot be registered: queue this interface's claim for the
			// post-sync unwind (a restored durable claim stays reserved)
			// and defer the failure so the remaining interfaces are still
			// processed
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] error registering the dhcp lease: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err)
			c.metrics.UpdateLogStatus("error")

			rememberApplied(pool.(kihv1.IPPool).Name, v.MACAddress, v.NetworkName, ip, false)

			newVmNetCfgs = append(newVmNetCfgs, v)

			if restoreErr == nil {
				restoreErr = fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] cannot register the dhcp lease for hwaddr %s: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, err.Error())
			}

			continue
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
			// is missing: queue this interface's claim for the post-sync
			// unwind (a restored durable claim of a transient status write
			// failure stays reserved and protected) and defer the failure
			// so the remaining interfaces are still processed
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err)
			c.metrics.UpdateLogStatus("error")

			rememberApplied(pool.(kihv1.IPPool).Name, v.MACAddress, v.NetworkName, ip, errors.Is(err, util.ErrForeignOwner))

			if restoreErr == nil {
				restoreErr = fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] cannot update the IPPool %s status for ip %s: %w",
					vmnetcfg.Namespace, vmnetcfg.Name, pool.(kihv1.IPPool).Name, ip, err)
			}

			continue
		}

		if err := c.updateIPPoolMetrics(pool.(kihv1.IPPool).Name); err != nil {
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err)
			c.metrics.UpdateLogStatus("error")
		}

		rememberApplied(pool.(kihv1.IPPool).Name, v.MACAddress, v.NetworkName, ip, false)

		networkChange = true
	}

	if restoreErr != nil {
		// the interfaces of this sync which were applied while the object
		// is not committed yet must be unwound, so no live allocation is
		// left for a never-persisted state; restored durable assignments
		// are never queued and stay applied, protecting the running guests
		c.rollbackAppliedAllocations(vmnetcfg, appliedAllocations)

		return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %w",
			vmnetcfg.Namespace, vmnetcfg.Name, restoreErr)
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
		c.rollbackAppliedAllocations(vmnetcfg, appliedAllocations)

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
	// to another vm in the meantime: the foreign snapshot check is
	// read-only and decides before any durable or local state is mutated.
	// for a live vmnetcfg a foreign owner aborts the sync so the changed
	// state is re-inspected on the next update; during deletion the
	// foreign allocation is left to its owner and the remaining own
	// allocations are cleaned so the finalizer completes
	releaseIP := false
	if netCfg.IPAddress != "" {
		releaseIP = true

		if leaseHwAddr, lease, found := c.dhcp.GetLeaseByIPAndNetwork(netCfg.NetworkName, netCfg.IPAddress); found && lease.Reference != ref {
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

	// the lease deletion re-validates the owner under the dhcp lock: the
	// by-ip snapshot decision above cannot race a concurrent reassignment
	// acting between the checks
	removeLease := func() error {
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

		return nil
	}

	// the release runs under the validated ownership decisions above, so a
	// same numeric lease of another network is no claim against this
	// network's allocation
	releaseAllocation := func() error {
		if !releaseIP {
			return nil
		}

		if err := c.ipam.ReleaseIP(netCfg.NetworkName, netCfg.IPAddress); err != nil {
			// already-free addresses are treated as done so a retried
			// cleanup can converge
			if !util.IsAlreadyReleased(err) {
				return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] error releasing ip from ipam: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			}
		}

		return nil
	}

	if deleting {
		// immediate release on VM delete stays the documented behavior: the
		// lease and the allocation are freed first and the finalizer retry
		// re-runs the whole cleanup until the status entry converges
		if err := removeLease(); err != nil {
			return err
		}

		if err := releaseAllocation(); err != nil {
			return err
		}
	}

	pool, poolErr := c.cache.Get("pool", netCfg.NetworkName)
	if poolErr != nil {
		// without the pool object the status entry cannot be removed: no
		// local allocation was touched yet, so the failing cleanup stays
		// fully consistent (lease, claim and record intact) for its retry
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
			// during a live transition the durable un-record happens
			// before any local release: a failed status write leaves the
			// lease, the ipam claim and the record fully intact, so the
			// owner keeps serving and the retried cleanup converges from
			// a consistent state. releasing before the un-record would
			// need a re-mark band-aid, whose anonymous re-mark cannot
			// idempotently recover the owner-mapped reservation and
			// bricks a one-address pool behind the sticky error status.
			return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
		}
	}

	if !deleting {
		// the live transition releases only after the durable un-record:
		// the address is never locally freed while its ownership record
		// is still written
		if err := removeLease(); err != nil {
			return err
		}

		if err := releaseAllocation(); err != nil {
			return err
		}
	}

	// the release above changed the pool accounting: republish the
	// metrics so the gauges do not stay stale after the last allocation
	// of a pool was cleaned
	if err := c.updateIPPoolMetrics(pool.(kihv1.IPPool).Name); err != nil {
		log.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, err)
		c.metrics.UpdateLogStatus("error")
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
		ownerRef := util.AllocationRef(vmnetcfgNamespace, vmnetcfgVMName, hwAddr)

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

					return fmt.Errorf("ip %s already found in IPPool status: %w", ip, util.ErrForeignOwner)
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

	// the gauges are computed from the live allocator state, not from the
	// persisted pool status: the cleanup un-records the status entry
	// before it releases the address, so a status write can carry the
	// counters of the not-yet-released allocation
	c.metrics.UpdateIPPoolUsed(pool.Name, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, c.ipam.Used(pool.Spec.NetworkName))
	c.metrics.UpdateIPPoolAvailable(pool.Name, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, c.ipam.Available(pool.Spec.NetworkName))

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
					util.CanonicalHWAddr(netstat.MACAddress),
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
