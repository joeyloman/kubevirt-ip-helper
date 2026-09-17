package vmnetcfg

import (
	"errors"
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	ipam "github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ippoolstatus"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	log "github.com/sirupsen/logrus"
)

// hijackErrorStatusMessage is the terminal error marker of an object which
// was created while the operator was down: such an object must come from
// the vm controller, so its recorded addresses are never honored and the
// marker makes the rejection survive the steady-state error retries.
const hijackErrorStatusMessage = "vmnetcfg was manually created after this program was (re)started, preventing possible ip hijack"

// vmnetcfgCleanupFinalizer is the finalizer the vm controller puts on
// every vmnetcfg it creates. its presence marks the object as
// controller-managed, which is the admission condition of the orphan
// sweep: a manually created vmnetcfg without it is never swept.
const vmnetcfgCleanupFinalizer = "kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"

// allocatedNetworkConfig tracks one fully applied interface allocation of a
// vmnetcfg object so it can be reverted if the durable object update fails.
type allocatedNetworkConfig struct {
	macAddress  string
	networkName string
	ipAddress   string
	poolName    string
	// contested marks an allocation whose address the pool status records
	// for another owner; such a claim must never survive the rollback,
	// while an uncontested one may already be served to its guest
	contested bool
}

// pendingLedgerDelete records the ledger entry of a nic whose compensating
// or unwind delete failed while a concurrent cleanup removed the nic: the
// tuple is not reconstructible from the spec anymore once the removal is
// durable, so the controller keeps it reachable and replays the
// owner-validated deletion on the reconciliations of the owning object
// until it converges (a restart loses the record, but the pool
// registration revalidates the persisted ledger and drops the orphaned
// entry of a positively removed binding).
type pendingLedgerDelete struct {
	namespace   string
	vmName      string
	ip          string
	networkName string
	macAddress  string
	poolName    string
}

// rollbackNetworkAllocation reverts the allocation side effects of a
// single network interface of a vmnetcfg object. the releases run before
// the pool status write, so the persisted counters are computed from an
// ipam state which already excludes the unwound allocation, and the pool
// metrics republish the settled accounting.
//
// only contested allocations are unwound: the dhcp server may already have
// acked the lease of an uncontested applied allocation to its guest, so
// deleting that lease would let the address be reissued while the old
// guest keeps using it (duplicate ip). the lease, the ipam claim and the
// pool status record of an uncontested allocation therefore stay as a
// quarantine: the retried sync's regular mismatch cleanup releases them
// through the owner-validated path and re-allocates, converging exactly
// like a regular ip change. a contested address must never be served by
// this binding's lease, so it is always released.
func (c *Controller) rollbackNetworkAllocation(vmnetcfg *kihv1.VirtualMachineNetworkConfig, allocated allocatedNetworkConfig) {
	if !c.scope.Owns(vmnetcfg.Namespace, allocated.networkName) {
		return
	}
	if !allocated.contested {
		log.Warnf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] keeping the served lease, claim and status record of ip %s (hwaddr %s, network %s) quarantined after the failed object update; the retried sync releases it through its regular cleanup",
			vmnetcfg.Namespace, vmnetcfg.Name, allocated.ipAddress, allocated.macAddress, allocated.networkName)
		c.metrics.UpdateLogStatus("warning")

		return
	}

	ref := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)

	if err := c.deleteLeaseInScope(allocated.macAddress, ref); err != nil {
		if errors.Is(err, dhcp.ErrLeaseNotFound) || errors.Is(err, dhcp.ErrLeaseForeignOwner) {
			// no lease of this binding is left, or a concurrent writer
			// reassigned it to another owner: the dhcp side of the rollback
			// converged and must not raise the error level
			log.Debugf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] the lease of hwaddr %s is gone or foreign, nothing left to revert",
				vmnetcfg.Namespace, vmnetcfg.Name, allocated.macAddress)
		} else {
			log.Errorf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] failed to revert the dhcp lease for hwaddr %s: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, allocated.macAddress, err)
			c.metrics.UpdateLogStatus("error")
		}
	}

	// the release is owner-validated: an allocation this sync made carries
	// this binding's owner reference, while an address which a successor
	// took over in the meantime (or which this sync never claimed, a
	// contested restore) is never freed with it
	ownerRef := util.AllocationRef(vmnetcfg.Namespace, vmnetcfg.Spec.VMName, allocated.macAddress)
	if err := c.ipam.ReleaseIPOwnedBy(allocated.networkName, allocated.ipAddress, ownerRef); err != nil &&
		!errors.Is(err, ipam.ErrIPForeignOwner) && !util.IsAlreadyReleased(err) {
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
		if errors.Is(err, util.ErrForeignOwner) {
			// the ledger record of a contested address belongs to its
			// foreign owner by definition, so the compensating delete
			// converged and must not raise the error level
			log.Debugf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] the ippool status record of ip %s belongs to another owner, leaving it",
				vmnetcfg.Namespace, vmnetcfg.Name, allocated.ipAddress)
		} else {
			log.Errorf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] failed to revert the ippool status for ip %s: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, allocated.ipAddress, err)
			c.metrics.UpdateLogStatus("error")
		}
	}

	if err := c.updateIPPoolMetrics(allocated.poolName); err != nil {
		log.Errorf("(vmnetcfg.rollbackNetworkAllocation) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, err)
		c.metrics.UpdateLogStatus("error")
	}
}

// rollbackAppliedAllocations processes the allocations queued by this sync,
// newest first. Contested claims are unwound: their addresses must never be
// served with the lease of this binding. Uncontested allocations remain
// quarantined: their leases may already have been served by the DHCP server,
// and their lease, claim, and status record converge through the retried
// sync's regular owner-validated cleanup (see rollbackNetworkAllocation).
// Restores of allocations already recorded in the saved spec are never
// queued by the caller, and are kept applied.
func (c *Controller) rollbackAppliedAllocations(vmnetcfg *kihv1.VirtualMachineNetworkConfig, applied []allocatedNetworkConfig) {
	for i := len(applied) - 1; i >= 0; i-- {
		c.rollbackNetworkAllocation(vmnetcfg, applied[i])
	}
}

// releaseOwnClaim releases an ipam claim which still carries this owner's
// reference but which no live state justifies anymore: the claim of a nic
// whose lease vanished during this reconciliation, or the claim of an
// allocation whose dhcp lease could not be registered (nothing was served
// and no ledger record was written). a successor which took the freed
// address over in the meantime (a fresh anonymous allocation or another
// owner's named reclaim) is never released by it. the converged outcomes
// (a foreign owner, an already-free address, a subnet which is gone) are
// tolerated: no durable state references an undelivered claim.
func (c *Controller) releaseOwnClaim(networkName string, ip string, ownerRef string) {
	if networkName != c.scope.NetworkName() {
		return
	}
	if err := c.ipam.ReleaseIPOwnedBy(networkName, ip, ownerRef); err != nil &&
		!errors.Is(err, ipam.ErrIPForeignOwner) && !util.IsAlreadyReleased(err) {
		log.Errorf("(vmnetcfg.releaseOwnClaim) [%s] cannot release the own claim of ip %s in network %s: %s",
			ownerRef, ip, networkName, err)
		c.metrics.UpdateLogStatus("error")
	}
}

func (c *Controller) updateVirtualMachineNetworkConfig(eventAction string, vmnetcfg *kihv1.VirtualMachineNetworkConfig) (err error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	live, readErr := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmnetcfg.Namespace).Get(c.ctx, vmnetcfg.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(readErr) {
		c.drainPendingUnwinds(fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Name))
		return nil
	}
	if readErr != nil {
		return readErr
	}
	if live.UID != vmnetcfg.UID {
		return errOwnedStateChanged
	}
	base := live
	vmnetcfg = c.ownedConfig(live)
	var networkChange bool = false
	var skipNic bool = false

	log.Tracef("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] processing new vmnetcfg [%+v]",
		vmnetcfg.Namespace, vmnetcfg.Name, vmnetcfg)

	// restoreErr records the first per-interface failure of this sync whose
	// repair needs a retry (a network without a registered pool, or an
	// unusable macaddress), while the remaining interfaces are still
	// processed: one interface's failure must never block the restoration
	// of the other interfaces (their assignments are protected through this
	// same sync). the error is reported after every interface was handled;
	// the startup gate counts the object through its settled classification
	// and the resynced retry converges once the failure is repaired.
	var restoreErr error

	// replay the ledger deletions whose compensating or unwind attempt
	// failed while the nic was concurrently removed: their tuple is not
	// reconstructible from the spec anymore, so this replay is the only
	// path which keeps them reachable. a transiently failing replay
	// defers the failure like a per-interface one, so the remaining
	// interfaces are still reconciled while the rate-limited retry or
	// the resync keeps replaying the deletion
	if unwindErr := c.retryPendingUnwinds(vmnetcfg); unwindErr != nil && restoreErr == nil {
		restoreErr = unwindErr
	}

	// cleanup the network configuration if the object is marked for deletion
	if vmnetcfg.ObjectMeta.DeletionTimestamp != nil {
		// a pending ledger unwind whose replay failed again must not be
		// dropped by the deletion: the cleanup below iterates only the
		// nics of the present spec, so the tuple of the pending entry is
		// not reconstructible anymore and its record would survive the
		// deletion of this object (blocking a later binding of the
		// address for the whole era). keep the finalizers and let the
		// retried sync replay the deletion until it converges: every
		// failure mode of the replay is transient (the converged
		// foreign-owner and not-found outcomes are classified inside
		// retryPendingUnwinds), so the finalizer path cannot hot-loop on
		// a permanent failure
		if restoreErr != nil {
			return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] pending ledger unwind did not converge, keeping the finalizers: %w",
				vmnetcfg.Namespace, vmnetcfg.Name, restoreErr)
		}

		if err := c.cleanupVirtualMachineNetworkConfig(base); err != nil {
			return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] failed to cleanup vmnetcfg: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
		}

		return
	}

	if len(vmnetcfg.Spec.NetworkConfig) == 0 && len(vmnetcfg.Status.NetworkConfig) == 0 {
		// A globally empty managed config can outlive its VM after last-NIC
		// removal. Use the full object so foreign-only configs stay excluded.
		c.sweepOrphanedBinding(base)
		return restoreErr
	}
	// Status-only rows are outstanding cleanup, not successful assignments.
	// Recover their addresses before replacing the owned status projection.
	for _, status := range vmnetcfg.Status.NetworkConfig {
		if !nicRecorded(vmnetcfg, allocatedNetworkConfig{macAddress: status.MACAddress, networkName: status.NetworkName}) {
			if err := c.recoverBindings(vmnetcfg, util.CanonicalHWAddr(status.MACAddress)); err != nil {
				return err
			}
		}
	}

	// an orphaned binding must not hold its reservations forever: the vm
	// controller is the only writer which deletes the vmnetcfg when its
	// VirtualMachine is deleted, and a vm deleted while no controller was
	// watching (the pod was down) produces no event any restart could
	// replay - the fresh informer lists only what exists. such an object
	// keeps its finalizer, its recorded binding keeps the ledger owner
	// alive, and its address stays allocated to a deleted vm until
	// someone deletes the object by hand. the sweep routes a
	// controller-managed binding whose vm is definitively gone into the
	// regular deletion flow, which releases the reservations through the
	// finalizer cleanup; a vm recreated with the same name rebuilds its
	// vmnetcfg through the vm controller's resync.
	if c.sweepOrphanedBinding(vmnetcfg) {
		return
	}

	newVmNetCfgs := []kihv1.NetworkConfig{}
	newNetCfgStatusList := []kihv1.NetworkConfigStatus{}

	// allocations which are applied to dhcp/ipam/ippool status while
	// processing this object; reverted when the object update fails.
	// restores of addresses which the stored spec already records never
	// enter this list: releasing them would free addresses which the
	// guests still use while the durable object keeps claiming them
	appliedAllocations := []allocatedNetworkConfig{}

	// claimedNics records the claims this sync freshly bound or restored,
	// so their ownership can be re-verified against the live object before
	// the commit: a nic which the vm controller removed while this sync
	// ran must not keep a freshly recreated lease/claim/ledger entry which
	// no reconciliation ever cleans again (the stale-spec restore race)
	claimedNics := []allocatedNetworkConfig{}

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
			contested:   contested,
		})
	}

	for _, v := range vmnetcfg.Spec.NetworkConfig {
		// create a fresh nic status
		netcfgStatus := kihv1.NetworkConfigStatus{}
		netcfgStatus.MACAddress = v.MACAddress
		netcfgStatus.NetworkName = v.NetworkName

		pool, poolErr := c.cache.Get("pool", v.NetworkName)
		if poolErr == nil {
			selected := pool.(kihv1.IPPool)
			if !c.scope.MatchesPool(&selected) {
				poolErr = fmt.Errorf("cached pool %s does not match network scope %s", selected.Name, c.scope.NetworkName())
			}
		}
		if poolErr != nil {
			// keep the durable spec and the previous status entry untouched,
			// skip this interface and continue with the next one
			if restoreErr == nil {
				// the sentinel lets the startup gate classify this failure as
				// permanent (initSyncSettled): a networkname without a live
				// pool registration cannot restore its reservation until the
				// offending IPPool is repaired
				restoreErr = fmt.Errorf("%w: %s", errNicPoolMissing, poolErr)
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

		// a nic in the ERROR status is re-attempted on a steady-state
		// UPDATE: the resync is the retry loop for transient failures (a
		// full pool, a competing foreign lease which is being cleaned up),
		// so once the underlying cause is gone the retried sync restores
		// the interface. the skip stays in two cases: during the startup
		// replay (the ADD sync must not touch the reached state it counts
		// for the gate), and for the hijack marker of an object which was
		// created while the operator was down (that decision is terminal -
		// the object must come from the vm controller and a later retry
		// must never honor its recorded addresses).
		skipNic = false
		for _, nic := range vmnetcfg.Status.NetworkConfig {
			if v.MACAddress == nic.MACAddress && v.NetworkName == nic.NetworkName && nic.Status == "ERROR" {
				if eventAction == UPDATE && nic.Message != hijackErrorStatusMessage {
					log.Infof("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] re-attempting the failed interface %s in network %s: %s",
						vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, v.NetworkName, nic.Message)

					break
				}

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
			if lease.Reference != fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName) || lease.PoolName != c.scope.NetworkName() {
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
				log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
					vmnetcfg.Namespace, vmnetcfg.Name, hijackErrorStatusMessage)
				c.metrics.UpdateLogStatus("error")

				netcfgStatus.Status = "ERROR"
				netcfgStatus.Message = hijackErrorStatusMessage
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
				// the sentinel lets the startup gate classify this failure as
				// permanent (initSyncSettled): an unusable macaddress can
				// never register a lease until the spec is corrected
				restoreErr = fmt.Errorf("%w: invalid macaddress %q for network %s", errNicMacInvalid, v.MACAddress, v.NetworkName)
			}

			continue
		}

		// handle address and network changes in the vmnetcfg object: the
		// lease identity is the (address, network) pair it was served
		// under, so a mac which moved to another network must migrate
		// even when it keeps its numeric address - treating a same-ip
		// network move as an unchanged lease would adopt the address in
		// the NEW network's allocator and repair its ledger while the
		// dhcp lease keeps serving the OLD network's configuration and
		// the old network's claim and ledger entry leak
		if c.dhcp.CheckLease(v.MACAddress) {
			lease := c.dhcp.GetLease(v.MACAddress)
			if lease.ClientIP.String() != v.IPAddress || lease.PoolName != v.NetworkName {
				// two-phase startup replay: a pending nic (no recorded
				// address) whose lease is live must keep its intact lease,
				// claim and status record during the initialization
				// replay: the fresh allocation is deferred until the gate
				// opened anyway, and tearing the reached state down here
				// would free an address whose record write may have been
				// lost before the restart, letting the replay hand it out
				// a second time. the post-gate requeue performs the same
				// cleanup and fresh allocation this branch would do.
				if v.IPAddress == "" && c.appStatus.Load() == APP_INIT {
					log.Infof("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] deferring the cleanup of the leased address %s of hwaddr %s until the initialization replay finished",
						vmnetcfg.Namespace, vmnetcfg.Name, lease.ClientIP.String(), v.MACAddress)
					c.metrics.UpdateLogStatus("warning")
					c.deferInitAllocation(fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Name))

					newVmNetCfgs = append(newVmNetCfgs, v)

					// a deferred nic keeps a previous status entry untouched
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

				log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] address or network change found for hwaddr=%s: the lease holds ip=%s in network=%s, the spec records ip=%s in network=%s, starting cleanup of the leased state",
					vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, lease.ClientIP.String(), lease.PoolName, v.IPAddress, v.NetworkName)
				c.metrics.UpdateLogStatus("warning")

				oldNetcfg := kihv1.NetworkConfig{}
				// the cleanup must un-record and release the allocation the
				// lease actually holds: the lease carries the network its
				// address was allocated from, which is not necessarily the
				// network of the spec entry (a mac which moved to another
				// network). targeting the spec's network would release an
				// address which was never allocated there and leak the old
				// network's claim and ledger entry instead
				oldNetcfg.NetworkName = lease.PoolName
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

				// set the old status; a binding whose verification and
				// repair succeed without any previous entry synthesizes its
				// success status below instead of serving silently. an
				// ERROR entry is deliberately not carried over: this sync
				// re-attempted the failed interface and verified its
				// lease, claim and ownership record, so republishing the
				// previous failure verbatim would leave the status and its
				// metric stuck on ERROR for an interface which serves,
				// while every resync re-attempts it again. the success
				// status is synthesized below instead
				statusRecorded := false
				for _, nic := range vmnetcfg.Status.NetworkConfig {
					if v.MACAddress == nic.MACAddress && v.NetworkName == nic.NetworkName {
						if nic.Status == "ERROR" {
							break
						}

						netcfgStatus.Status = nic.Status
						netcfgStatus.Message = nic.Message
						newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)
						statusRecorded = true

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
				adoptErr := c.dhcp.WithOwnedLease(v.MACAddress, vmRef, v.NetworkName, v.IPAddress, func() error {
					return c.ipam.AdoptIP(v.NetworkName, v.IPAddress, ownerRef)
				})
				if adoptErr != nil {
					if errors.Is(adoptErr, dhcp.ErrLeaseNotFound) || errors.Is(adoptErr, dhcp.ErrLeaseForeignOwner) || errors.Is(adoptErr, dhcp.ErrLeaseIdentityMismatch) {
						// the lease vanished, was reassigned to another owner,
						// or was replaced for the same owner under another
						// network or address between the snapshot and the
						// adoption: the state this snapshot verified is gone,
						// so the nic is being reworked by a concurrent writer
						// either way. the guarded adopt recreated nothing, and
						// a claim an earlier reconciliation of this binding
						// left behind is released while it still carries this
						// owner's reference, so the removed nic cannot keep
						// the address blocked, the replacement lease of the
						// same owner is never adopted under the stale
						// snapshot, and a successor is never touched - the
						// retried sync observes the live identity and
						// migrates it properly
						log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] the lease of hwaddr %s vanished before its address could be adopted, releasing the stale claim of the removed nic",
							vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress)
						c.metrics.UpdateLogStatus("warning")

						c.releaseOwnClaim(v.NetworkName, lease.ClientIP.String(), ownerRef)

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
				// the guarded adoption and this write, or a concurrent writer
				// can replace it for the same owner reference under another
				// network or address. the repair must not resurrect ownership
				// state which the raced cleanup removes, and it must not
				// confirm the identity of a replacement lease the snapshot
				// never observed, so the full binding identity is re-validated
				// immediately before the write and verified again afterwards:
				// a vanished or replaced lease skips the repair and releases
				// the stale claim, and a lease which changes between the write
				// and the verification is undone by the owner-validated
				// compensating delete and release (a meanwhile recorded
				// foreign owner or a successor's allocation is never clobbered
				// - the own reference only removes the own state).
				if !c.dhcp.HasOwnedLease(v.MACAddress, vmRef, v.NetworkName, v.IPAddress) {
					log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] the lease of hwaddr %s vanished during the ownership repair, skipping it and releasing the stale claim of the removed nic",
						vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress)
					c.metrics.UpdateLogStatus("warning")

					c.releaseOwnClaim(v.NetworkName, lease.ClientIP.String(), ownerRef)

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
					!c.dhcp.HasOwnedLease(v.MACAddress, vmRef, v.NetworkName, v.IPAddress) {
					// the concurrent cleanup removed the lease, or a
					// concurrent writer replaced it for the same owner under
					// another identity, between the repair decision and the
					// durable write: undo the resurrected ownership record
					// before it blocks the address for a later binding
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

						// the tuple of this compensating delete is about to
						// leave the spec (the concurrent cleanup removes the
						// nic before the retried sync re-reads the object),
						// so it must stay reachable independently of the
						// nic list: the retried sync cannot reconstruct it
						// from the spec anymore and replays it through the
						// pending unwinds instead
						c.rememberPendingUnwind(
							fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Name),
							pendingLedgerDelete{
								namespace:   vmnetcfg.Namespace,
								vmName:      vmnetcfg.Spec.VMName,
								ip:          v.IPAddress,
								networkName: v.NetworkName,
								macAddress:  v.MACAddress,
								poolName:    pool.(kihv1.IPPool).Name,
							},
						)
					}

					// the claim this reconciliation adopted for the removed
					// nic is not justified by its lease anymore: release it
					// while it still carries this owner's reference. the
					// concurrent cleanup normally released it already (the
					// owner-validated release treats that as converged) but
					// a cleanup which skipped its own release must not
					// leave the claim behind ownerless
					c.releaseOwnClaim(v.NetworkName, lease.ClientIP.String(), ownerRef)
				}

				if repairErr != nil && restoreErr == nil {
					restoreErr = repairErr
				}
				if c.dhcp.HasOwnedLease(v.MACAddress, vmRef, v.NetworkName, v.IPAddress) {
					claimedNics = append(claimedNics, allocatedNetworkConfig{
						macAddress: v.MACAddress, networkName: v.NetworkName,
						ipAddress: v.IPAddress, poolName: pool.(kihv1.IPPool).Name,
					})
				}

				// a binding whose lease was verified and adopted but which
				// carries no previous status entry (its status write was
				// lost before a restart, or an earlier sync discarded the
				// freshly generated one when its pool write failed) must
				// not serve silently without its success status:
				// synthesize it so the tail publishes the status and its
				// metric exactly like the fresh allocation path does
				if !statusRecorded {
					netcfgStatus.Status = "OK"
					netcfgStatus.Message = "IP address successfully allocated"
					newNetCfgStatusList = append(newNetCfgStatusList, netcfgStatus)
				}

				continue
			}
		}

		// if v.IPAddress is not empty we re-claim it else we get a new one.
		// the re-claim carries the owner reference so a registration which
		// pinned the persisted pool-status claims into a fresh allocator
		// accepts the restore of the recorded owner idempotently, while a
		// foreign fresh or seeded allocation is rejected instead of being
		// silently taken. the claimant identity additionally accepts the
		// own ownerless protection pin of the registration sweep: the sweep
		// pins the recorded address of a claim whose macaddress was
		// unusable at registration time, so the binding of that vm retakes
		// its own pin once the identity is corrected, while a pin of
		// another vm and an unattributed pin stay rejected.
		ownerRef := util.AllocationRef(vmnetcfg.Namespace, vmnetcfg.Spec.VMName, v.MACAddress)
		vmRef := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)

		var ip string
		var err error
		if v.IPAddress != "" {
			ip, err = c.ipam.ReclaimIPClaimant(v.NetworkName, v.IPAddress, ownerRef, vmRef)
		} else {
			if c.appStatus.Load() == APP_INIT {
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

			// the fresh allocation is a named reservation of this binding:
			// the delayed cleanup of a removed nic can release it through
			// the owner-validated release, while no other owner can ever
			// displace it
			ip, err = c.ipam.AllocateIP(v.NetworkName, ownerRef)
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

		if err := c.dhcp.AddLease(
			v.MACAddress,
			pool.(kihv1.IPPool).Spec.NetworkName,
			ip,
			vmRef,
		); err != nil {
			// dhcp must not serve the address when its owner reference
			// cannot be registered: nothing was served, no ledger record
			// was written and the pending spec entry is not durable yet,
			// so the claim has no lease, no status record and no spec
			// entry which any later reconciliation could detect again -
			// quarantining it would block the address until a process
			// restart. release it directly instead (a restored durable
			// claim keeps its reservation: the spec entry protects it) and
			// defer the failure so the remaining interfaces are still
			// processed
			log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] error registering the dhcp lease: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, err)
			c.metrics.UpdateLogStatus("error")

			if !durableAllocations[v.MACAddress+"/"+v.NetworkName+"/"+ip] {
				c.releaseOwnClaim(v.NetworkName, ip, ownerRef)

				if err := c.updateIPPoolMetrics(pool.(kihv1.IPPool).Name); err != nil {
					log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
						vmnetcfg.Namespace, vmnetcfg.Name, err)
					c.metrics.UpdateLogStatus("error")
				}
			}

			newVmNetCfgs = append(newVmNetCfgs, v)

			if restoreErr == nil {
				restoreErr = fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] cannot register the dhcp lease for hwaddr %s: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, v.MACAddress, err.Error())
			}

			continue
		}
		claimedNics = append(claimedNics, allocatedNetworkConfig{
			macAddress:  v.MACAddress,
			networkName: v.NetworkName,
			ipAddress:   ip,
			poolName:    pool.(kihv1.IPPool).Name,
		})

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

	// verify the freshly claimed ownership records against the live object
	// before anything is committed: the vm controller releases the state of
	// a removed nic before its durable spec update lands, and a sync which
	// read the stale spec would re-claim and re-record the nic into an
	// object which no longer references it - an orphan lease, claim and
	// ledger entry which no reconciliation ever cleans (the finalizer
	// iterates only the present spec nics). a nic which vanished during
	// this sync is unwound through the owner-validated release and dropped
	// from the pending commit
	if err := c.verifyClaimedNics(base, claimedNics, &newVmNetCfgs, &newNetCfgStatusList); err != nil {
		log.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, err)
		c.metrics.UpdateLogStatus("error")

		// the verification could not run and the pending commit is lost
		// either way: unwind the contested claims of this sync now (an
		// uncontested allocation stays quarantined like below). the retried
		// sync takes the lease-based repair path for these nics, which
		// never unwinds a contested claim, so skipping the rollback here
		// would leave their leases serving addresses whose ledger record
		// belongs to another owner
		c.rollbackAppliedAllocations(vmnetcfg, appliedAllocations)

		return err
	}

	if restoreErr == nil && c.hasPendingCleanup(base) {
		restoreErr = fmt.Errorf("pending VMNetCfg cleanup or accounting remains")
	}
	if restoreErr != nil {
		// the contested claims of this sync are unwound while the
		// uncontested applied allocations stay quarantined (see
		// rollbackNetworkAllocation): a lease may already have been served
		// for them, so releasing would risk a duplicate ip; the retried
		// sync converges through its regular cleanup
		c.rollbackAppliedAllocations(vmnetcfg, appliedAllocations)

		return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfig) [%s/%s] %w",
			vmnetcfg.Namespace, vmnetcfg.Name, restoreErr)
	}

	newVmnetCfgStatus := kihv1.VirtualMachineNetworkConfigStatus{}
	newVmnetCfgStatus.NetworkConfig = newNetCfgStatusList

	commitBase := base
	if networkChange {
		commitBase, err = c.commitSpec(base, newVmNetCfgs)
		if err != nil {
			// Reverify even non-conflict failures: a retry fence may have
			// detected deletion, replacement, owner change or a removed NIC.
			if verifyErr := c.verifyClaimedNics(base, claimedNics, &newVmNetCfgs, &newNetCfgStatusList); verifyErr != nil {
				log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) cannot verify failed commit: %s", verifyErr)
			}
			c.rollbackAppliedAllocations(vmnetcfg, appliedAllocations)
			return err
		}
		if !equalStatusRows(c.scope.FilterStatus(base.Namespace, base.Status.NetworkConfig), c.scope.FilterStatus(commitBase.Namespace, commitBase.Status.NetworkConfig)) {
			return errOwnedStateChanged
		}
	}
	if err := c.updateVirtualMachineNetworkConfigStatus(commitBase, &newVmnetCfgStatus); err != nil {
		if verifyErr := c.verifyClaimedNics(commitBase, claimedNics, &newVmNetCfgs, &newNetCfgStatusList); verifyErr != nil {
			log.Warnf("(vmnetcfg.updateVirtualMachineNetworkConfig) cannot verify failed status commit: %s", verifyErr)
		}
		return err
	}
	if err := c.updateVirtualMachineNetworkConfigMetrics(base.Namespace, base.Name); err != nil {
		return err
	}

	return
}

func (c *Controller) cleanupNetworkInterface(vmnetcfg *kihv1.VirtualMachineNetworkConfig, netCfg *kihv1.NetworkConfig, deleting bool) (err error) {
	if !c.scope.Owns(vmnetcfg.Namespace, netCfg.NetworkName) {
		return nil
	}
	canonical := *netCfg
	canonical.NetworkName = c.scope.NetworkName()
	canonical.MACAddress = util.CanonicalHWAddr(canonical.MACAddress)
	netCfg = &canonical
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
		if lease := c.dhcp.GetLease(netCfg.MACAddress); lease.ClientIP != nil && lease.PoolName != c.scope.NetworkName() {
			return nil
		}
		if err := c.deleteLeaseInScope(netCfg.MACAddress, ref); err != nil {
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

			case errors.Is(err, dhcp.ErrLeaseInvalidHwAddr):
				// an unparseable mac can never own a lease: the dhcp side
				// of this cleanup has converged, the ipam release of an
				// unparseable ip is classified the same way below

			default:
				return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] error deleting lease from dhcp: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			}
		}

		return nil
	}

	// the release runs under the validated ownership decisions above and
	// is owner-validated in ipam: a same numeric lease of another network
	// is no claim against this network's allocation, and a successor which
	// took the address over in the meantime keeps it
	releaseAllocation := func() error {
		if !releaseIP {
			return nil
		}

		ownerRef := util.AllocationRef(vmnetcfg.Namespace, vmnetcfg.Spec.VMName, netCfg.MACAddress)
		if err := c.ipam.ReleaseIPOwnedBy(netCfg.NetworkName, netCfg.IPAddress, ownerRef); err != nil {
			if errors.Is(err, ipam.ErrIPForeignOwner) {
				// the address belongs to a successor or to a conservative
				// registration pin: converged, nothing left to release
				log.Warnf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] ip %s is allocated by another owner, skipping the ipam release of it",
					vmnetcfg.Namespace, vmnetcfg.Name, netCfg.IPAddress)
				c.metrics.UpdateLogStatus("warning")
			} else if !util.IsAlreadyReleased(err) && !util.IsUnusableIdentity(err) {
				// already-free addresses and unparseable addresses are
				// treated as done so a retried cleanup can converge
				return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] error releasing ip from ipam: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
			}
		}

		return nil
	}

	// Capture a quarantined tuple before deleting its last local reference.
	// Durable un-record comes first on both live and deleting paths.
	var capturedLease dhcp.DHCPLease
	if deleting {
		if lease := c.dhcp.GetLease(netCfg.MACAddress); lease.Reference == ref && lease.PoolName == c.scope.NetworkName() && lease.ClientIP != nil {
			capturedLease = lease
		}
	}
	releaseLocal := func() error {
		if capturedLease.ClientIP != nil && capturedLease.ClientIP.String() != netCfg.IPAddress {
			if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{
				MACAddress:  netCfg.MACAddress,
				NetworkName: capturedLease.PoolName,
				IPAddress:   capturedLease.ClientIP.String(),
			}, true); err != nil {
				return err
			}
		}
		if err := removeLease(); err != nil {
			return err
		}
		return releaseAllocation()
	}

	pool, poolErr := c.cache.Get("pool", netCfg.NetworkName)
	if poolErr == nil {
		selected := pool.(kihv1.IPPool)
		if !c.scope.MatchesPool(&selected) {
			return fmt.Errorf("cached pool %s does not match network scope %s", selected.Name, c.scope.NetworkName())
		}
	}
	if poolErr != nil {
		if deleting {
			// a deleted pool object takes its whole status ledger with it,
			// so on the deletion path the un-record may only be skipped
			// when the pool is truly gone: verify that through the api. a
			// pool object which merely missed the cache still holds the
			// ledger entry, and the finalizer must stay (error) until the
			// entry is removed, or the record is orphaned forever
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
					log.Warnf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] the pool of network %s does not exist anymore, its status record is gone with it",
						vmnetcfg.Namespace, vmnetcfg.Name, netCfg.NetworkName)

					return releaseLocal()
				}
			} else {
				// the api verification itself failed: fail conservatively,
				// the record might still exist in a live pool object
				return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] cannot verify the pool of network %s during deletion, cache miss: %s: %s",
					vmnetcfg.Namespace, vmnetcfg.Name, netCfg.NetworkName, poolErr.Error(), listErr.Error())
			}
		}

		// the status entry cannot be removed while the pool is not cached:
		// this is a failed cleanup, not a converged one. proceeding would
		// orphan the ledger entry forever. No local release has happened;
		// the retried cleanup converges
		// once the pool is cached again
		return fmt.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, poolErr.Error())
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

	if err := releaseLocal(); err != nil {
		return err
	}
	// The ledger write observed the pre-release allocator. Refresh counters
	// best-effort without making accounting a cleanup acknowledgement gate.
	if err := ippoolstatus.UpdateAccounting(c.ctx, c.kihClientset, c.ipam, c.scope.NetworkName(), pool.(kihv1.IPPool).Name); err != nil {
		log.Errorf("(vmnetcfg.cleanupNetworkInterface) [%s/%s] cannot refresh pool accounting: %s", vmnetcfg.Namespace, vmnetcfg.Name, err)
		c.metrics.UpdateLogStatus("error")
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

// sweepOrphanedBinding routes a controller-managed vmnetcfg whose
// VirtualMachine is definitively gone into the deletion flow, and reports
// whether it did. the vm controller puts the cleanup finalizer on every
// vmnetcfg it creates and is the only writer which deletes the object when
// its vm is deleted, so a finalizer-carrying binding whose vm answers
// NotFound on the authoritative api is an orphan: no event of the gone vm
// exists anymore which any restart could replay. the delete is conditioned
// on the uid the informer delivered, so a same-name replacement created
// between the delivery and the delete is rejected by the apiserver instead
// of destroyed. the deletion runs through the regular finalizer cleanup of
// the next sync, which releases the lease, the claim and the ledger entry.
// the sweep is best-effort by design: a transient verification or delete
// failure never fails the reconciliation of a possibly live binding - the
// next resync retries the sweep instead. a nil verifyVM seam fails closed:
// nothing is swept (tests and any client without a kubevirt api).
func (c *Controller) sweepOrphanedBinding(vmnetcfg *kihv1.VirtualMachineNetworkConfig) bool {
	if c.verifyVM == nil || vmnetcfg.Spec.VMName == "" {
		return false
	}
	if len(vmnetcfg.Spec.NetworkConfig)+len(vmnetcfg.Status.NetworkConfig) != 0 &&
		len(c.scope.FilterSpec(vmnetcfg.Namespace, vmnetcfg.Spec.NetworkConfig)) == 0 && len(c.scope.FilterStatus(vmnetcfg.Namespace, vmnetcfg.Status.NetworkConfig)) == 0 {
		return false
	}

	// the cleanup finalizer is only put on the object by the vm
	// controller, so only a finalizer-carrying binding is known to be
	// controller-managed: a manually created vmnetcfg is never swept
	controllerManaged := false
	for _, finalizer := range vmnetcfg.ObjectMeta.Finalizers {
		if finalizer == vmnetcfgCleanupFinalizer {
			controllerManaged = true

			break
		}
	}
	if !controllerManaged {
		return false
	}

	vmExists, vmErr := c.verifyVM(vmnetcfg.Namespace, vmnetcfg.Spec.VMName)
	if vmErr != nil {
		log.Warnf("(vmnetcfg.sweepOrphanedBinding) [%s/%s] cannot verify the VirtualMachine %s of the binding, skipping the orphan sweep of this sync: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, vmnetcfg.Spec.VMName, vmErr.Error())
		c.metrics.UpdateLogStatus("warning")

		return false
	}

	if vmExists {
		return false
	}

	if err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmnetcfg.Namespace).Delete(c.ctx, vmnetcfg.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &vmnetcfg.UID, ResourceVersion: &vmnetcfg.ResourceVersion},
	}); err != nil && !apierrors.IsNotFound(err) {
		log.Errorf("(vmnetcfg.sweepOrphanedBinding) [%s/%s] cannot delete the orphaned vmnetcfg of the gone VirtualMachine %s: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, vmnetcfg.Spec.VMName, err.Error())
		c.metrics.UpdateLogStatus("error")

		return false
	}

	log.Infof("(vmnetcfg.sweepOrphanedBinding) [%s/%s] the VirtualMachine %s is gone, deleting the orphaned vmnetcfg so its reservations are released through the cleanup",
		vmnetcfg.Namespace, vmnetcfg.Name, vmnetcfg.Spec.VMName)

	return true
}

func (c *Controller) cleanupVirtualMachineNetworkConfig(vmnetcfg *kihv1.VirtualMachineNetworkConfig) error {
	if vmnetcfg.DeletionTimestamp == nil {
		return errOwnedStateChanged
	}
	owned := c.ownedConfig(vmnetcfg)
	for i := range owned.Spec.NetworkConfig {
		if err := c.cleanupNetworkInterface(owned, &owned.Spec.NetworkConfig[i], true); err != nil {
			return err
		}
	}
	// A crash after spec acknowledgement leaves only status. Pool ledger
	// recovery also finds quarantined tuples no longer present in either row.
	if len(owned.Spec.NetworkConfig) > 0 || len(owned.Status.NetworkConfig) > 0 || (len(vmnetcfg.Spec.NetworkConfig) == 0 && len(vmnetcfg.Status.NetworkConfig) == 0) {
		if err := c.recoverBindings(owned, ""); err != nil {
			return err
		}
	}
	if c.hasPendingCleanup(vmnetcfg) {
		return fmt.Errorf("pending VMNetCfg cleanup remains")
	}
	afterSpec, err := c.commitSpec(vmnetcfg, nil)
	if err != nil {
		return err
	}
	// Do not acknowledge a status row which changed while cleanup was running.
	if !equalStatusRows(c.scope.FilterStatus(vmnetcfg.Namespace, vmnetcfg.Status.NetworkConfig), c.scope.FilterStatus(afterSpec.Namespace, afterSpec.Status.NetworkConfig)) {
		return errOwnedStateChanged
	}
	afterStatus, err := c.commitStatus(afterSpec, nil)
	if err != nil {
		return err
	}
	c.deleteVirtualMachineNetworkConfigMetrics(owned)
	_, err = c.retryOwnedWrite(afterStatus, false, func(live *kihv1.VirtualMachineNetworkConfig) (bool, error) {
		if live.DeletionTimestamp == nil || len(live.Spec.NetworkConfig) != 0 || len(live.Status.NetworkConfig) != 0 || c.hasPendingCleanup(live) {
			return false, nil
		}
		kept := live.Finalizers[:0]
		for _, finalizer := range live.Finalizers {
			if finalizer != vmnetcfgCleanupFinalizer && finalizer != "kubevirtiphelper" {
				kept = append(kept, finalizer)
			}
		}
		if len(kept) == len(live.Finalizers) {
			return false, nil
		}
		live.Finalizers = kept
		return true, nil
	})
	return err
}

// verifyClaimedNics re-reads the vmnetcfg object after every interface of
// this sync bound its claim and confirms each claimed nic is still recorded
// in the live spec. the vm and the vmnetcfg controllers mutate the same
// object from separate queues: the vm controller releases the state of a
// removed nic before its durable spec update lands, and a sync which read
// the object before that update would restore the nic's claim into a spec
// which no longer references it - an orphan lease, claim and ledger entry
// which no reconciliation ever cleans (the finalizer iterates only the
// present spec nics). a nic which vanished during the sync is unwound
// through the owner-validated release and dropped from the pending commit,
// mirroring the registration sweep's re-verification of its own pins.
func (c *Controller) verifyClaimedNics(vmnetcfg *kihv1.VirtualMachineNetworkConfig, claimed []allocatedNetworkConfig, pendingSpec *[]kihv1.NetworkConfig, pendingStatus *[]kihv1.NetworkConfigStatus) error {
	if len(claimed) == 0 {
		return nil
	}

	live, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmnetcfg.Namespace).Get(c.ctx, vmnetcfg.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		for _, nc := range claimed {
			c.unwindClaim(vmnetcfg, nc)
			removeNicFromSpec(pendingSpec, nc.macAddress, nc.networkName)
			removeNicFromStatus(pendingStatus, nc.macAddress, nc.networkName)
		}
		return errOwnedStateChanged
	}
	if err != nil {
		// the retried sync re-runs the whole verification; an object which
		// is gone entirely is handled by its deletion event
		return fmt.Errorf("(vmnetcfg.verifyClaimedNics) [%s/%s] cannot re-read the object to verify the claimed nics: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, err.Error())
	}

	for _, nc := range claimed {
		if sameOwnerState(vmnetcfg, live) && live.DeletionTimestamp == nil && claimDecisionRetained(vmnetcfg, live, nc) {
			continue
		}

		log.Warnf("(vmnetcfg.verifyClaimedNics) [%s/%s] the nic %s of network %s with ip %s was removed while this sync restored it, unwinding its freshly created claim",
			vmnetcfg.Namespace, vmnetcfg.Name, nc.macAddress, nc.networkName, nc.ipAddress)
		c.metrics.UpdateLogStatus("warning")

		c.unwindClaim(vmnetcfg, nc)
		removeNicFromSpec(pendingSpec, nc.macAddress, nc.networkName)
		removeNicFromStatus(pendingStatus, nc.macAddress, nc.networkName)
	}

	return nil
}

// nicRecorded reports whether the nic identified by mac/networkname is
// still part of the spec of the given object. the address is deliberately
// not compared: a fresh allocation or an ip change of this very sync is
// only durable after its own commit, so the live object still carries the
// pre-sync address (or none) at verification time. the vm controller
// removes a nic as a whole (mac+network), which is exactly the race this
// verification closes.
func nicRecorded(vmnetcfg *kihv1.VirtualMachineNetworkConfig, nc allocatedNetworkConfig) bool {
	for _, v := range vmnetcfg.Spec.NetworkConfig {
		if util.CanonicalHWAddr(v.MACAddress) == util.CanonicalHWAddr(nc.macAddress) && util.QualifyNetworkName(vmnetcfg.Namespace, v.NetworkName) == util.QualifyNetworkName(vmnetcfg.Namespace, nc.networkName) {
			return true
		}
	}

	return false
}

// unwindClaim releases the owner-validated state of a nic whose recorded
// claim this sync recreated but whose spec entry concurrently vanished.
// every release is guarded by its owner reference: a successor which took
// the address over in the meantime (a fresh allocation or another owner's
// claim) is never freed with it, and the converged outcomes (already-free
// addresses, foreign owners, absent leases) are tolerated.
func (c *Controller) unwindClaim(vmnetcfg *kihv1.VirtualMachineNetworkConfig, nc allocatedNetworkConfig) {
	if !c.scope.Owns(vmnetcfg.Namespace, nc.networkName) {
		return
	}
	ref := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName)

	if err := c.deleteLeaseInScope(nc.macAddress, ref); err != nil &&
		!errors.Is(err, dhcp.ErrLeaseNotFound) && !errors.Is(err, dhcp.ErrLeaseForeignOwner) {
		log.Errorf("(vmnetcfg.unwindClaim) [%s/%s] failed to delete the lease of hwaddr %s: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, nc.macAddress, err)
		c.metrics.UpdateLogStatus("error")
	}

	ownerRef := util.AllocationRef(vmnetcfg.Namespace, vmnetcfg.Spec.VMName, nc.macAddress)
	if err := c.ipam.ReleaseIPOwnedBy(nc.networkName, nc.ipAddress, ownerRef); err != nil &&
		!errors.Is(err, ipam.ErrIPForeignOwner) && !util.IsAlreadyReleased(err) {
		log.Errorf("(vmnetcfg.unwindClaim) [%s/%s] failed to release the claim of ip %s in network %s: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, nc.ipAddress, nc.networkName, err)
		c.metrics.UpdateLogStatus("error")
	}

	if err := c.updateIPPoolStatus(DELETE, vmnetcfg.Namespace, vmnetcfg.Spec.VMName, nc.ipAddress, nc.networkName, nc.macAddress, nc.poolName); err != nil &&
		!errors.Is(err, util.ErrForeignOwner) {
		log.Errorf("(vmnetcfg.unwindClaim) [%s/%s] failed to remove the ip %s record from the IPPool %s status: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, nc.ipAddress, nc.poolName, err)
		c.metrics.UpdateLogStatus("error")

		c.rememberPendingUnwind(
			fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Name),
			pendingLedgerDelete{
				namespace:   vmnetcfg.Namespace,
				vmName:      vmnetcfg.Spec.VMName,
				ip:          nc.ipAddress,
				networkName: nc.networkName,
				macAddress:  nc.macAddress,
				poolName:    nc.poolName,
			},
		)
	}

	if err := c.updateIPPoolMetrics(nc.poolName); err != nil {
		log.Errorf("(vmnetcfg.unwindClaim) [%s/%s] %s",
			vmnetcfg.Namespace, vmnetcfg.Name, err)
		c.metrics.UpdateLogStatus("error")
	}
}

// removeNicFromSpec and removeNicFromStatus drop the entries of a vanished
// nic from the pending commit of a sync, so the stale spec read cannot
// write the removed nic back into the live object.
func removeNicFromSpec(spec *[]kihv1.NetworkConfig, macAddress string, networkName string) {
	kept := (*spec)[:0]
	for _, v := range *spec {
		if !(v.MACAddress == macAddress && v.NetworkName == networkName) {
			kept = append(kept, v)
		}
	}
	*spec = kept
}

func removeNicFromStatus(status *[]kihv1.NetworkConfigStatus, macAddress string, networkName string) {
	kept := (*status)[:0]
	for _, v := range *status {
		if !(v.MACAddress == macAddress && v.NetworkName == networkName) {
			kept = append(kept, v)
		}
	}
	*status = kept
}

func (c *Controller) updateIPPoolStatus(event string, vmnetcfgNamespace string, vmnetcfgVMName string, ip string, networkName string, hwAddr string, poolName string) (err error) {
	if !c.scope.Owns(vmnetcfgNamespace, networkName) {
		return nil
	}
	return ippoolstatus.UpdateStatus(c.ctx, c.kihClientset, c.ipam, event, vmnetcfgNamespace, vmnetcfgVMName, ip, c.scope.NetworkName(), hwAddr, poolName)
}
func (c *Controller) updateVirtualMachineNetworkConfigStatus(vmnetcfg *kihv1.VirtualMachineNetworkConfig, vmnetcfgStatus *kihv1.VirtualMachineNetworkConfigStatus) error {
	_, err := c.commitStatus(vmnetcfg, c.scope.FilterStatus(vmnetcfg.Namespace, vmnetcfgStatus.NetworkConfig))
	return err
}

func (c *Controller) updateIPPoolMetrics(poolName string) (err error) {
	pool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(c.ctx, poolName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot get IPPool %s: %s", poolName, err.Error())
	}
	if pool.Spec.NetworkName != c.scope.NetworkName() {
		return nil
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
	vmnetcfg, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(vmnetcfgNamespace).Get(c.ctx, vmnetcfgName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("(vmnetcfg.updateVirtualMachineNetworkConfigMetrics) cannot get VirtualMachineNetworkConfig %s/%s: %s",
			vmnetcfgNamespace, vmnetcfgName, err.Error())
	}

	c.metrics.DeleteVmNetCfgStatus(fmt.Sprintf("%s/%s", vmnetcfgNamespace, vmnetcfgName))
	for _, netstat := range c.scope.FilterStatus(vmnetcfg.Namespace, vmnetcfg.Status.NetworkConfig) {
		for _, netcfg := range c.scope.FilterSpec(vmnetcfg.Namespace, vmnetcfg.Spec.NetworkConfig) {
			if util.CanonicalHWAddr(netstat.MACAddress) == util.CanonicalHWAddr(netcfg.MACAddress) && util.QualifyNetworkName(vmnetcfg.Namespace, netstat.NetworkName) == util.QualifyNetworkName(vmnetcfg.Namespace, netcfg.NetworkName) {
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
