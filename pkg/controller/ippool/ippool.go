package ippool

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	log "github.com/sirupsen/logrus"
)

const (
	IPPOOL_NOCHANGE = 0
	IPPOOL_RELOAD   = 1
	IPPOOL_RESTART  = 2
)

// ErrPoolUnregistrable reports a registration rejection which cannot
// succeed on a retry in its current form: the projection itself is
// invalid (its subnet does not parse) or its networkname is claimed by
// the live registration of another pool. the startup gate counts such
// pools as handled so a broken object does not block the controller
// startup, while every other registration failure stays retriable and
// uncounted until the attempt settles.
var ErrPoolUnregistrable = errors.New("the pool cannot be registered in its current form")

// subnetRegistrationError classifies a NewSubnet failure for the startup
// gate: a range configuration which can never become valid is a definitive
// rejection (unregistrable, so the gate counts the pool as handled instead
// of waiting forever), while every other error - including the retryable
// 'already exists' state conflict of a half-cleaned registration - keeps
// its plain error so the retried attempt can still settle it.
func subnetRegistrationError(networkName string, err error) error {
	if errors.Is(err, ipam.ErrSubnetInvalid) {
		return fmt.Errorf("error while allocating a new subnet in IPAM for network [%s]: %s: %w", networkName, err.Error(), ErrPoolUnregistrable)
	}

	return fmt.Errorf("error while allocating a new subnet in IPAM for network [%s]: %s", networkName, err.Error())
}

// validateExcludeEntries reports whether every exclude entry of the pool
// projection can ever be claimed by a registration: an entry which does
// not parse, is not an ipv4 address of the subnet, equals the broadcast
// address or lies outside the start..end pool range could never be
// reclaimed by the exclude pass, so it is rejected here - before any
// host, dhcp or allocator mutation - instead of failing the registration
// after the listener is already live and every resync rebuilding and
// tearing the half-applied registration down again.
func validateExcludeEntries(pool *kihv1.IPPool) error {
	ipnet, err := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet)
	if err != nil {
		return fmt.Errorf("invalid subnet %s: %s", pool.Spec.IPv4Config.Subnet, err.Error())
	}
	if !ipnet.Addr().Is4() {
		return fmt.Errorf("subnet %s is not an ipv4 subnet", pool.Spec.IPv4Config.Subnet)
	}

	startAddr, err := netip.ParseAddr(pool.Spec.IPv4Config.Pool.Start)
	if err != nil {
		return fmt.Errorf("invalid start address %s: %s", pool.Spec.IPv4Config.Pool.Start, err.Error())
	}
	endAddr, err := netip.ParseAddr(pool.Spec.IPv4Config.Pool.End)
	if err != nil {
		return fmt.Errorf("invalid end address %s: %s", pool.Spec.IPv4Config.Pool.End, err.Error())
	}
	startAddr, endAddr = startAddr.Unmap(), endAddr.Unmap()

	// the broadcast of the subnet: the allocator never hands it out, so an
	// exclude entry may not claim it either
	subnetStart := ipnet.Addr().As4()
	subnetMask := net.CIDRMask(ipnet.Bits(), 32)
	var broadcast [4]byte
	for i := range subnetStart {
		broadcast[i] = subnetStart[i] | ^subnetMask[i]
	}
	broadcastAddr, _ := netip.AddrFromSlice(broadcast[:])

	for _, ex := range pool.Spec.IPv4Config.Pool.Exclude {
		exAddr, err := netip.ParseAddr(ex)
		if err != nil {
			return fmt.Errorf("invalid exclude address %s: %s", ex, err.Error())
		}
		exAddr = exAddr.Unmap()

		if !exAddr.Is4() {
			return fmt.Errorf("exclude address %s is not an ipv4 address", ex)
		}
		if !ipnet.Contains(exAddr) {
			return fmt.Errorf("exclude address %s is not within subnet %s", ex, pool.Spec.IPv4Config.Subnet)
		}
		if exAddr.Compare(broadcastAddr) == 0 {
			return fmt.Errorf("exclude address %s equals the broadcast address %s", ex, broadcastAddr.String())
		}
		if exAddr.Compare(startAddr) < 0 || exAddr.Compare(endAddr) > 0 {
			return fmt.Errorf("exclude address %s is not within the pool range %s-%s",
				ex, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End)
		}
	}

	return nil
}

func (c *Controller) selectedPool(pool *kihv1.IPPool) bool {
	return c.scope.NetworkName() != "" &&
		pool.Labels[util.NetworkLabel] == c.scope.Name() &&
		pool.Labels[util.NetworkNamespaceLabel] == c.scope.Namespace()
}

func (c *Controller) poolIdentityError(pool *kihv1.IPPool) error {
	return fmt.Errorf("IPPool %s labels and spec.networkname %q must match helper network %s: %w",
		pool.Name, pool.Spec.NetworkName, c.scope.NetworkName(), ErrPoolUnregistrable)
}

// Every fresh pool read used for registration must still belong to the
// selected network before its claims or status are used.
func (c *Controller) verifyPoolIdentity(current *kihv1.IPPool) error {
	if !c.scope.MatchesPool(current) {
		return c.poolIdentityError(current)
	}
	return nil
}

func (c *Controller) registerIPPool(pool *kihv1.IPPool) (cleanup bool, err error) {

	// the startup gate counts this pool as handled once its registration
	// attempt settled: counting at the start would open the gate while the
	// pool sub-resources are still being created, so the vmnetcfg
	// controller could restore bindings before the pool registrations are
	// live. a settled registration or a definitive rejection counts, while
	// a transient failure stays uncounted so the requeued or resynced
	// retry can still settle the pool for the gate.
	defer func() {
		if err == nil || errors.Is(err, ErrPoolUnregistrable) {
			c.markInitAttempt(pool.Name)
		}
	}()

	// by default cleanup the pool sub resources
	cleanup = false

	// add the serverip to the bindinterface
	ipnet, err := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet)
	if err != nil {
		return cleanup, fmt.Errorf("error while parsing subnet [%s] for network [%s]: %s: %w",
			pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, err.Error(), ErrPoolUnregistrable)
	}
	// the full projection must be registrable before any state is mutated:
	// the nic add, the dhcp pool and its listener would otherwise run with
	// a garbage projection (an ipv6 subnet masks to a wrong v4 prefix or
	// to "<nil>") and only the later NewSubnet validation would reject it,
	// leaving the compensating cleanup to rebuild the same malformed
	// address string it should remove
	if validateErr := ipam.ValidateSubnetSpec(pool.Spec.IPv4Config.Subnet, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End); validateErr != nil {
		return cleanup, fmt.Errorf("error while validating subnet [%s] and range [%s-%s] for network [%s]: %s: %w",
			pool.Spec.IPv4Config.Subnet, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End,
			pool.Spec.NetworkName, validateErr.Error(), ErrPoolUnregistrable)
	}

	// every exclude entry must be claimable before any state is mutated:
	// an unclaimable entry (outside the pool range, the subnet or the
	// broadcast) could only fail the exclude pass after the listener is
	// already live, so every retried attempt and resync would rebuild and
	// tear the half-applied registration down again
	if excludeErr := validateExcludeEntries(pool); excludeErr != nil {
		return cleanup, fmt.Errorf("error while validating the exclude entries of pool [%s] for network [%s]: %s: %w",
			pool.Name, pool.Spec.NetworkName, excludeErr.Error(), ErrPoolUnregistrable)
	}

	// the address projection of the wire path must be registrable before
	// any state is mutated: an invalid entry would otherwise only fail
	// at the reply construction of a live listener (see
	// validatePoolProjection), so every retried attempt and resync would
	// rebuild and tear the half-applied registration down again
	if projectionErr := validatePoolProjection(pool); projectionErr != nil {
		return cleanup, fmt.Errorf("error while validating the address projection of pool [%s] for network [%s]: %s: %w",
			pool.Name, pool.Spec.NetworkName, projectionErr.Error(), ErrPoolUnregistrable)
	}

	// an exclude entry which the persisted ledger records for a live
	// binding is a configuration conflict which can never converge: the
	// exclude pass claims the address as EXCLUDED first, so the later
	// claim protection of the same address fails with a foreign-owner
	// error and every retry tears the half-built registration down again
	// (re-adding and removing the nic address, dhcp pool and listener in
	// a loop). the conflict is rejected before any mutation as a
	// definitive, unregistrable configuration, so the startup gate counts
	// the pool and the churn stops
	// the persisted-claim lookup needs the api; a controller without a
	// clientset (unit-constructed) skips the up-front check and the later
	// claim protection keeps the registration honest
	if c.kihClientset != nil {
		cPool, getErr := c.kihClientset.KubevirtiphelperV1().IPPools().Get(c.ctx, pool.Name, metav1.GetOptions{})
		if getErr != nil {
			return cleanup, fmt.Errorf("error while checking the exclude entries of pool [%s] against its persisted claims for network [%s]: %s",
				pool.Name, pool.Spec.NetworkName, getErr.Error())
		}
		if getErr == nil {
			if identityErr := c.verifyPoolIdentity(cPool); identityErr != nil {
				return cleanup, identityErr
			}
			for _, ex := range pool.Spec.IPv4Config.Pool.Exclude {
				if ref, claimed := cPool.Status.IPv4.Allocated[ex]; claimed && ref != ipam.ExcludedOwner {
					conflict, verifyErr := c.excludeEntryConflicts(pool, ex, ref)
					if verifyErr != nil {
						// an unverifiable owner is a transient state, not a
						// definitive rejection: the plain error keeps the
						// registration retriable and the startup gate open
						// until the api read succeeds
						return cleanup, fmt.Errorf("error while verifying the owner of the exclude address [%s] of network [%s] against its persisted claim: %s",
							ex, pool.Spec.NetworkName, verifyErr.Error())
					}
					if conflict {
						return cleanup, fmt.Errorf("exclude address [%s] of network [%s] is recorded in the IPPool status as allocated to [%s]; remove the exclude entry or release the claim first: %w",
							ex, pool.Spec.NetworkName, ref, ErrPoolUnregistrable)
					}
				}
			}
		}
	}
	// the pool sub-resources (dhcp pool, ipam subnet, cache entry) are all
	// keyed by the networkname, and the allocators start empty on every
	// (re)start: a live dhcp pool under this networkname therefore belongs
	// to another IPPool registration. any later failure path would tear
	// down or silently replace its live allocations, so reject before
	// any sub-resource of this pool is created.
	if c.dhcp.CheckPool(pool.Spec.NetworkName) {
		return cleanup, fmt.Errorf("networkname [%s] is already registered by another IPPool, not touching its live state: %w", pool.Spec.NetworkName, ErrPoolUnregistrable)
	}
	// one bind interface serves one pool: a second pool on the same
	// interface would share the broadcast segment with the first one, and
	// every socket of the interface receives both pools' traffic (the
	// so_reuseport delivery semantics depend on the deployment kernel, so
	// which pool answers a request is not deterministic). the second pool
	// is rejected before any of its sub-resources exist, and the rejection
	// is unregistrable so the startup gate counts the pool instead of
	// retrying the conflict forever
	if otherNetwork, inUse := c.dhcp.NicClaimedByAnotherPool(pool.Spec.BindInterface, pool.Spec.NetworkName); inUse {
		return cleanup, fmt.Errorf("bindinterface [%s] of network [%s] is already registered by the pool of network [%s]: %w",
			pool.Spec.BindInterface, pool.Spec.NetworkName, otherNetwork, ErrPoolUnregistrable)
	}

	// from here pool sub resources needs to be cleaned up when something
	// goes wrong; the flag is set before the host-state mutation so a
	// rejected add (a stale or duplicated server ip the idempotent add
	// cannot make sense of) still tears the interface state down
	cleanup = true

	ip4 := fmt.Sprintf("%s/%d", pool.Spec.IPv4Config.ServerIP, ipnet.Bits())
	if err := network.AddIpToNic(pool.Spec.BindInterface, ip4); err != nil {
		return cleanup, fmt.Errorf("error while adding IP4 address [%s] to bind interface [%s] for network [%s]: %s",
			ip4, pool.Spec.BindInterface, pool.Spec.NetworkName, err.Error())
	}

	log.Debugf("(ippool.registerIPPool) added IP4 address [%s] to nic [%s] for network [%s]",
		ip4, pool.Spec.BindInterface, pool.Spec.NetworkName)

	// create the new dhcp pool
	if err := c.createOrUpdateDHCPPool(pool); err != nil {
		return cleanup, fmt.Errorf("error while registering DHCP pool for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// start a dhcp service thread for the pool identity (networkname),
	// through the runListener seam so a registration is testable without
	// a host interface (production controllers default to dhcp.Run)
	runListener := c.runListener
	if runListener == nil {
		runListener = c.dhcp.Run
	}
	if err := runListener(pool.Spec.NetworkName, pool.Spec.BindInterface); err != nil {
		return cleanup, fmt.Errorf("error while starting DHCP service thread for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// register the new subnet in ipam
	if err = c.ipam.NewSubnet(
		pool.Spec.NetworkName,
		pool.Spec.IPv4Config.Subnet,
		pool.Spec.IPv4Config.Pool.Start,
		pool.Spec.IPv4Config.Pool.End,
	); err != nil {
		return cleanup, subnetRegistrationError(pool.Spec.NetworkName, err)
	}

	// mark the exclude ips as used
	for _, v := range pool.Spec.IPv4Config.Pool.Exclude {
		if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, v, ipam.ExcludedOwner); err != nil {
			return cleanup, fmt.Errorf("error while excluding ip [%s] in IPAM for network [%s]: %s", v, pool.Spec.NetworkName, err.Error())
		}
	}

	// pin the persisted claims of the pool in the fresh allocator before
	// the pool becomes visible to fresh allocations: a registration which
	// only succeeds after the startup gate dropped its retries (an UPDATE
	// resync recovery) must not re-create the race where a new vm snapshot
	// takes an address of the still-ownerless bindings
	protectedClaims, err := c.protectPersistedClaims(pool)
	if err != nil {
		return cleanup, fmt.Errorf("error while protecting the persisted claims of the pool for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// rebuild the pool status after restarting the process
	rPool, err := c.resetIPPoolStatus(pool, protectedClaims)
	if err != nil {
		return cleanup, fmt.Errorf("error while restting IPPool status for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// reset the pool metrics after restarting the process
	if err = c.resetIPPoolMetrics(pool); err != nil {
		return cleanup, fmt.Errorf("error while restting IPPool metrics for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// cache the pool with a status carrying the protected claims and the
	// excluded addresses. the cached projection is the spec which was
	// actually installed on the nic, the dhcp pool and the ipam subnet:
	// the status write is built on a fresh api GET, so its response may
	// carry a spec which was updated on the api after the informer
	// delivered this object (for example while the application was still
	// initializing and ignoring updates). caching that readback would
	// swallow the update forever - the next resync compares the event
	// against the newer cached projection and takes the NOCHANGE branch -
	// so only the freshly rebuilt status of the response is adopted, on
	// a deep copy of the input spec
	installed := pool.DeepCopy()
	installed.Status = rPool.Status
	if err = c.cache.Add(installed); err != nil {
		return cleanup, fmt.Errorf("error while caching the IPPool for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	log.Infof("(ippool.registerIPPool) [%s] new IPPool registered", pool.Name)

	return
}

// registerPoolWithTeardown runs one registration attempt for the pool and
// tears a partially applied registration back down when the attempt fails
// midway: the leftover sub-resources (the server ip on the bind interface,
// the dhcp pool and its listener) would otherwise claim the networkname,
// so every retried attempt is rejected by the duplicate-networkname check
// and the network stays unregistered until the process is restarted.
// failLog prefixes the failure log line with the triggering event path.
func (c *Controller) registerPoolWithTeardown(pool *kihv1.IPPool, failLog string) (err error) {
	cleanup, err := c.registerIPPool(pool)
	if err == nil {
		return
	}

	log.Errorf("(ippool.sync) %s %s: %s", failLog, pool.Name, err.Error())
	c.metrics.UpdateLogStatus("error")

	if cleanup {
		if cleanupErr := c.cleanupIPPoolObjects(pool); cleanupErr != nil {
			log.Errorf("(ippool.sync) failed to cleanup pool %s: %s", pool.Name, cleanupErr.Error())
			c.metrics.UpdateLogStatus("error")
		}
	}

	return
}

func (c *Controller) handleIPPoolObjectChange(oldPool kihv1.IPPool, newPool *kihv1.IPPool) (err error) {
	var updateAction int = IPPOOL_NOCHANGE

	// if the app still initializing don't handle IPPool updates
	if c.appStatus.Load() == APP_INIT {
		log.Debugf("(ippool.handleIPPoolObjectChange) application is still in initializing state, ignoring updates until it's running..")
		return
	}

	// a restart tears every live service down and re-registers all pools
	// during the reinitialization phase: an update whose new projection
	// cannot produce a live registration again must be rejected before any
	// teardown, so the registered configuration keeps serving. the crd
	// schema accepts spellings the controller cannot parse (for example a
	// subnet length of two digits such as 10.10.10.0/33); without this
	// guard such an update drains every dhcp listener and leaves its
	// network unregistered until the object is repaired by hand.
	if _, parseErr := netip.ParsePrefix(newPool.Spec.IPv4Config.Subnet); parseErr != nil {
		return fmt.Errorf("(ippool.handleIPPoolObjectChange) rejecting update for networkname [%s]: the subnet [%s] does not parse, keeping the currently registered configuration: %s",
			newPool.Spec.NetworkName, newPool.Spec.IPv4Config.Subnet, parseErr.Error())
	}

	// every projection which can never produce a live registration must
	// be rejected before the teardown, not just the unparseable subnet:
	// a range outside the subnet, a reversed range or the broadcast as end
	// would drain the live services and then fail the registration forever
	if validateErr := ipam.ValidateSubnetSpec(newPool.Spec.IPv4Config.Subnet, newPool.Spec.IPv4Config.Pool.Start, newPool.Spec.IPv4Config.Pool.End); validateErr != nil {
		return fmt.Errorf("(ippool.handleIPPoolObjectChange) rejecting update for networkname [%s]: %s, keeping the currently registered configuration",
			newPool.Spec.NetworkName, validateErr.Error())
	}

	// the exclude entries are part of the same pre-teardown validation:
	// an unclaimable entry would drain the live services here and then
	// fail the re-registration of the next era forever (the exclude pass
	// can never reclaim it), so the update is rejected while the
	// registered configuration keeps serving
	if excludeErr := validateExcludeEntries(newPool); excludeErr != nil {
		return fmt.Errorf("(ippool.handleIPPoolObjectChange) rejecting update for networkname [%s]: %s, keeping the currently registered configuration",
			newPool.Spec.NetworkName, excludeErr.Error())
	}

	// the address projection is part of the same pre-teardown validation:
	// an invalid entry would drain the live services here and then fail
	// the re-registration of the next era forever, so the update is
	// rejected while the registered configuration keeps serving
	if projectionErr := validatePoolProjection(newPool); projectionErr != nil {
		return fmt.Errorf("(ippool.handleIPPoolObjectChange) rejecting update for networkname [%s]: %s, keeping the currently registered configuration",
			newPool.Spec.NetworkName, projectionErr.Error())
	}

	if oldPool.Spec.NetworkName != newPool.Spec.NetworkName && c.dhcp.CheckPool(newPool.Spec.NetworkName) {
		return fmt.Errorf("(ippool.handleIPPoolObjectChange) rejecting update for [%s]: the networkname [%s] is already registered by another IPPool, keeping the currently registered configuration",
			oldPool.Spec.NetworkName, newPool.Spec.NetworkName)
	}

	// an exclude entry which the persisted ledger records for a live
	// binding is the same never-converging conflict the registration
	// rejects up front: without this guard the restart teardown would
	// drain the live services and the re-registration of the next era
	// would then be rejected forever, so the update is refused while the
	// registered configuration keeps serving. the check runs only when
	// the exclude entries actually changed: the registered entries
	// coexist with the ledger by construction. the ledger lookup needs
	// the api; a controller without a clientset (unit-constructed) skips
	// the check and the re-registration keeps the configuration honest.
	if !reflect.DeepEqual(oldPool.Spec.IPv4Config.Pool.Exclude, newPool.Spec.IPv4Config.Pool.Exclude) && c.kihClientset != nil {
		cPool, getErr := c.kihClientset.KubevirtiphelperV1().IPPools().Get(c.ctx, newPool.Name, metav1.GetOptions{})
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("(ippool.handleIPPoolObjectChange) error while checking the exclude entries of pool [%s] against its persisted claims for network [%s]: %s",
				newPool.Name, newPool.Spec.NetworkName, getErr.Error())
		}
		if getErr == nil {
			if identityErr := c.verifyPoolIdentity(cPool); identityErr != nil {
				return identityErr
			}
			for _, ex := range newPool.Spec.IPv4Config.Pool.Exclude {
				if ref, claimed := cPool.Status.IPv4.Allocated[ex]; claimed && ref != ipam.ExcludedOwner {
					conflict, verifyErr := c.excludeEntryConflicts(newPool, ex, ref)
					if verifyErr != nil {
						// an unverifiable owner is a transient state: the
						// update is retried by the requeue and the resync
						// while the registered configuration keeps serving
						return fmt.Errorf("(ippool.handleIPPoolObjectChange) error while verifying the owner of the exclude address [%s] of pool [%s]: %s",
							ex, newPool.Name, verifyErr.Error())
					}
					if conflict {
						return fmt.Errorf("(ippool.handleIPPoolObjectChange) rejecting update for [%s]: the exclude address [%s] is recorded in the IPPool status as allocated to [%s]; remove the exclude entry or release the claim first, keeping the currently registered configuration",
							newPool.Name, ex, ref)
					}
				}
			}
		}
	}

	for {
		if c.appStatus.Load() != APP_RESTART {
			break
		}

		// a dying generation must not spin forever: once the application
		// cancels the era context this worker exits and the queued update is
		// re-delivered by the next era's informer resync
		select {
		case <-c.ctx.Done():
			return fmt.Errorf("(ippool.handleIPPoolObjectChange) deferring update of pool %s during application reinitialization", newPool.Name)
		case <-time.After(time.Second * 5):
		}

		log.Warnf("(ippool.handleIPPoolObjectChange) application is still in restarting state, waiting until it's reinitialized..")
	}

	// the following pool changes need a restart
	if oldPool.Spec.IPv4Config.ServerIP != newPool.Spec.IPv4Config.ServerIP ||
		oldPool.Spec.IPv4Config.Subnet != newPool.Spec.IPv4Config.Subnet ||
		oldPool.Spec.IPv4Config.Pool.Start != newPool.Spec.IPv4Config.Pool.Start ||
		oldPool.Spec.IPv4Config.Pool.End != newPool.Spec.IPv4Config.Pool.End ||
		!reflect.DeepEqual(oldPool.Spec.IPv4Config.Pool.Exclude, newPool.Spec.IPv4Config.Pool.Exclude) ||
		oldPool.Spec.IPv4Config.Router != newPool.Spec.IPv4Config.Router ||
		oldPool.Spec.BindInterface != newPool.Spec.BindInterface ||
		oldPool.Spec.NetworkName != newPool.Spec.NetworkName {
		updateAction = IPPOOL_RESTART
	}

	if updateAction == IPPOOL_RESTART {
		log.Infof("(ippool.handleIPPoolObjectChange) IPPool configuration changes detected, starting application reinitialization")

		// stop the DHCP listener
		c.stopDHCPListener(&oldPool)

		// remove the serverip from the bindinterface
		ipnet, errr := netip.ParsePrefix(oldPool.Spec.IPv4Config.Subnet)
		if errr != nil {
			log.Errorf("%s", errr.Error())
		}
		ip4 := fmt.Sprintf("%s/%d", oldPool.Spec.IPv4Config.ServerIP, ipnet.Bits())

		log.Debugf("(ippool.handleIPPoolObjectChange) removing the IP4 address [%s] from nic [%s] for network [%s]",
			ip4, oldPool.Spec.BindInterface, oldPool.Spec.NetworkName)

		if errr := network.RemoveIpFromNic(oldPool.Spec.BindInterface, ip4); errr != nil {
			log.Errorf("%s", errr.Error())
		}

		// notify the main thread that everything needs to be reinitialized
		c.appStatus.Store(APP_RESTART)

		return
	}

	// the following pool changes can be reloaded
	if oldPool.Spec.IPv4Config.LeaseTime != newPool.Spec.IPv4Config.LeaseTime ||
		oldPool.Spec.IPv4Config.DomainName != newPool.Spec.IPv4Config.DomainName ||
		!reflect.DeepEqual(oldPool.Spec.IPv4Config.DNS, newPool.Spec.IPv4Config.DNS) ||
		!reflect.DeepEqual(oldPool.Spec.IPv4Config.DomainSearch, newPool.Spec.IPv4Config.DomainSearch) ||
		!reflect.DeepEqual(oldPool.Spec.IPv4Config.NTP, newPool.Spec.IPv4Config.NTP) {
		updateAction = IPPOOL_RELOAD
	}

	if updateAction == IPPOOL_NOCHANGE {
		// no pool options are changed, so the pool cache doesn't have to be updated
		return
	} else if updateAction == IPPOOL_RELOAD {
		log.Infof("(ippool.handleIPPoolObjectChange) IPPool configuration changes detected, updating the dhcppool")
		if err := c.createOrUpdateDHCPPool(newPool); err != nil {
			// a rejected reload must not enter the invalid configuration
			// into the cache either: keep the previously cached pool
			return fmt.Errorf("(ippool.handleIPPoolObjectChange) error while updating dhcppool [%s]: %s",
				newPool.Spec.NetworkName, err.Error())
		}
	}

	// the reloaded projection replaces the cached pool in one atomic step:
	// a delete-then-add sequence would expose a transient "pool missing"
	// window to the concurrent readers of the shared cache, and a reader
	// which acts on it (a cleanup which aborts, a binding which fails its
	// restore) would diverge from the live registration. the replacement
	// creates the entry when the pool is not cached yet, so the outcome is
	// identical to the previous delete-if-cached plus add sequence
	if err := c.cache.Upsert(newPool); err != nil {
		return fmt.Errorf("(ippool.handleIPPoolObjectChange) failed to replace pool %s in cache: %s", newPool.Name, err.Error())
	}

	return
}

func (c *Controller) stopDHCPListener(pool *kihv1.IPPool) {
	if err := c.dhcp.Stop(pool.Spec.NetworkName); err != nil {
		log.Errorf("(ippool.stopDHCPListener) error while shutting down DHCP listener running on nic [%s] for network [%s]: %s",
			pool.Spec.BindInterface, pool.Spec.NetworkName, err.Error())
		c.metrics.UpdateLogStatus("error")
	}
}

func (c *Controller) cleanupIPPoolObjects(pool *kihv1.IPPool) (err error) {
	log.Debugf("(ippool.cleanupIPPoolObjects) [%s] starting cleanup of IPPool", pool.Name)

	c.stopDHCPListener(pool)
	c.ipam.DeleteSubnet(pool.Spec.NetworkName)
	// the pool entry is removed before the leases: an in-flight handler
	// (the packet handler of the library runs in its own goroutine) which
	// snapshots the pool-absent state while the lease is still registered
	// is the deleted-pool case the nak path covers, so the ordering keeps
	// that answer reachable for as long as the teardown runs
	c.dhcp.DeletePool(pool.Spec.NetworkName)
	// a deleted pool must not leave its leases behind: its listener is
	// already stopped, so nothing of the network is answered anymore, and a
	// registration which is later re-created under the same networkname must
	// not serve the pre-deletion addresses of its own bindings - their
	// renewals would be acked with an address the new subnet may not
	// contain. the bindings re-claim their recorded address through the new
	// registration's claim protection instead, and an address which the new
	// pool cannot serve surfaces as their error status (unlike a reload,
	// which keeps the leases of the live vms and the same projection)
	c.dhcp.RemoveLeasesForNetwork(pool.Spec.NetworkName)
	c.metrics.DeleteIPPool(pool.Name, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName)
	c.cache.Delete("pool", pool.Spec.NetworkName)

	ipnet, err := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet)
	if err != nil {
		return
	}
	ip4 := fmt.Sprintf("%s/%d", pool.Spec.IPv4Config.ServerIP, ipnet.Bits())
	network.RemoveIpFromNic(pool.Spec.BindInterface, ip4)

	return
}

func (c *Controller) createOrUpdateDHCPPool(pool *kihv1.IPPool) (err error) {
	// validate the projection first: only an ipv4 subnet with a registrable
	// range may replace the active dhcp pool, otherwise a rejected update
	// would destroy the working configuration (the dhcp pool delete below
	// runs before the mask projection, so an ipv6 subnet would already be
	// masked to an all-ones v4 prefix or "<nil>" and handed to AddPool by
	// the time any later check could reject it)
	if validateErr := ipam.ValidateSubnetSpec(pool.Spec.IPv4Config.Subnet, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End); validateErr != nil {
		return fmt.Errorf("(ippool.createOrUpdateDHCPPool) invalid subnet [%s] and range [%s-%s] for network [%s]: %s",
			pool.Spec.IPv4Config.Subnet, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End,
			pool.Spec.NetworkName, validateErr.Error())
	}
	ipnet, err := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet)
	if err != nil {
		return fmt.Errorf("(ippool.createOrUpdateDHCPPool) invalid subnet [%s] for network [%s]: %s",
			pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, err.Error())
	}
	subnetMask := net.CIDRMask(ipnet.Bits(), 32)

	if c.dhcp.CheckPool(pool.Spec.NetworkName) {
		if err := c.dhcp.DeletePool(pool.Spec.NetworkName); err != nil {
			log.Errorf("(ippool.createOrUpdateDHCPPool) while deleting dhcppool [%s]: %s", pool.Spec.NetworkName, err.Error())
			c.metrics.UpdateLogStatus("error")
		}
	}

	// register the new subnet in dhcp. the AddPool validation is the
	// backstop of the up-front validatePoolProjection admission: a pool
	// which reaches this point with an invalid address projection has
	// already destroyed its live dhcp pool above, so the error must
	// surface instead of being silently dropped (the caller requeues and
	// the resync re-runs the registration, and the up-front admission
	// keeps the deterministic defects out of this path entirely)
	if err := c.dhcp.AddPool(
		pool.Spec.NetworkName,
		pool.Spec.IPv4Config.ServerIP,
		net.IP(subnetMask).String(),
		pool.Spec.IPv4Config.Router,
		pool.Spec.IPv4Config.DNS,
		pool.Spec.IPv4Config.DomainName,
		pool.Spec.IPv4Config.DomainSearch,
		pool.Spec.IPv4Config.NTP,
		pool.Spec.IPv4Config.LeaseTime,
		pool.Spec.BindInterface,
	); err != nil {
		return fmt.Errorf("(ippool.createOrUpdateDHCPPool) cannot register the dhcp pool of network [%s]: %s",
			pool.Spec.NetworkName, err.Error())
	}

	return
}

// validatePoolProjection verifies the address fields of the pool spec
// which reach the dhcp wire: the server ip, the router and the dns
// entries must be ipv4 literals (the router may stay unset). an ipv6 or
// unparseable entry passes every earlier check and fails only at the
// reply construction, where it encodes as a zero-length or short dhcp
// option which strict client parsers drop - and a v6 server ip makes the
// server-identifier comparison of every DHCPREQUEST permanently false -
// so the pool would serve a network whose dhcp silently never works. the
// rejection is deterministic and precedes every host, dhcp and allocator
// mutation, so it is unregistrable like the other projection defects.
func validatePoolProjection(pool *kihv1.IPPool) error {
	if ip := net.ParseIP(pool.Spec.IPv4Config.ServerIP); ip == nil || ip.To4() == nil {
		return fmt.Errorf("the server ip [%s] is not a valid ipv4 address", pool.Spec.IPv4Config.ServerIP)
	}
	if router := pool.Spec.IPv4Config.Router; router != "" {
		if ip := net.ParseIP(router); ip == nil || ip.To4() == nil {
			return fmt.Errorf("the router [%s] is not a valid ipv4 address", router)
		}
	}
	for _, dnsServer := range pool.Spec.IPv4Config.DNS {
		if ip := net.ParseIP(dnsServer); ip == nil || ip.To4() == nil {
			return fmt.Errorf("the dns entry [%s] is not a valid ipv4 address", dnsServer)
		}
	}

	return nil
}

// specClaim records one admitted claim of the vmnetcfg claim sweep: the
// exact nic spec entry it was made for, the claiming object and the owner
// identities the pin and the binding restore construct from it. named
// reports whether the macaddress could form an owner reference; an
// unnamed claim is pinned ownerlessly and attributed to its vm, so the
// binding of that vm can retake it once the identity is corrected.
type specClaim struct {
	namespace string
	name      string
	vmRef     string
	ownerRef  string
	mac       string
	ip        string
	named     bool
}

// protectPersistedClaims pins every durable claim of the pool into the
// fresh ipam allocator before the registration publishes it: a recovering
// pool which registers again after the startup gate dropped its retries
// must never expose an allocator state which offers the bound addresses
// of live bindings to fresh allocations. the durable claims come from two
// sources:
//
//   - the ownership ledger the pool status survived with. its records are
//     the authoritative owner decisions of the previous process era, so
//     they are pinned first and a spec claim which disagrees with them is
//     a genuine conflict which must not overwrite the recorded owner.
//     parseable references are normalized to the canonical spelling of
//     util.AllocationRef before they are pinned and republished: main-era
//     records such as "ns/vm [02-AA-BB-CC-DD-01]" describe the same owner
//     as the restoring binding's "ns/vm [02:aa:bb:cc:dd:01]", and only the
//     canonical spelling keeps the ipam owner identity, the republished
//     ledger entry and the binding's identity in agreement. unparseable
//     references keep their conservative protection: the address is pinned
//     without an owner identity (never reclaimable, so never double-bound)
//     and the original record is republished verbatim.
//
//   - the recorded assignments of the vmnetcfg objects (an authoritative
//     cluster-wide snapshot). the ledger alone is not a complete inventory:
//     main could persist a vmnetcfg assignment after the pool status write
//     failed, so an existing vm can claim an address with no ledger entry
//     at all. every persisted nonempty ip of every nic of every namespace
//     referencing this network is pinned under its canonical owner
//     reference, so the protection does not depend on which binding
//     reconciles first or on the ledger having survived.
//
// the snapshot is taken through the api before the pool is published: the
// publication point of the registration is the cache add, and fresh
// allocations require the cached pool, so every claim is pinned strictly
// before any fresh allocation can see the allocator. a snapshot which
// cannot be obtained fails the registration instead of publishing an
// allocator which may hand bound addresses to new vms.
func (c *Controller) protectPersistedClaims(pool *kihv1.IPPool) (map[string]string, error) {
	cPool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(
		c.ctx, pool.Name, metav1.GetOptions{},
	)
	if err != nil {
		// without the persisted status the claims cannot be known: fail
		// the registration instead of publishing an unprotected allocator
		return nil, fmt.Errorf("error while getting IPPool %s: %w", pool.Name, err)
	}
	if err := c.verifyPoolIdentity(cPool); err != nil {
		return nil, err
	}
	claims := make(map[string]string)
	// pinnedIPs records the addresses this protection actually reserved:
	// the spec sweep skips them (the ledger already decided those
	// addresses), so its later re-verification can never drop a claim the
	// authoritative ledger pass pinned
	pinnedIPs := make(map[string]bool)
	for ip, ref := range cPool.Status.IPv4.Allocated {
		if ref == ipam.ExcludedOwner {
			continue
		}

		namespace, vmName, hwAddr, ok := util.ParseAllocationRef(ref)
		if !ok {
			// an unparseable claim cannot be attributed to an owner: while
			// it stays inside the pool range the address is protected
			// unconditionally (a wasted address never double-binds one),
			// and outside the range the allocator cannot hand it out at
			// all. the original record is republished either way
			log.Warnf("(ippool.protectPersistedClaims) IPPool %s carries the unparseable allocation reference %q for ip %s",
				pool.Name, ref, ip)

			claims[ip] = ref

			if ipWithinPoolRange(pool, ip) {
				// the conservative ownerless pin of an unknown historical
				// reference: unattributed, so no binding can ever reclaim it
				if err := c.ipam.ProtectIP(pool.Spec.NetworkName, ip, ""); err != nil {
					return nil, fmt.Errorf("error while protecting the unparseable claim of ip [%s] of IPPool %s in IPAM for network [%s]: %s",
						ip, pool.Name, pool.Spec.NetworkName, err.Error())
				}

				pinnedIPs[ip] = true
			}

			continue
		}

		// the canonical spelling of the parsed owner: the ipam owner token
		// and the republished ledger entry must both agree with the
		// reference the restoring binding constructs, otherwise the same
		// logical owner of a legacy record is rejected as a foreign owner
		ownerRef := util.AllocationRef(namespace, vmName, hwAddr)
		if ownerRef != ref {
			log.Warnf("(ippool.protectPersistedClaims) IPPool %s carries the allocation reference %q for ip %s in a legacy spelling, normalizing it to %q",
				pool.Name, ref, ip, ownerRef)
		}

		// revalidate the recorded owner before the entry is pinned and
		// republished: the ledger is the previous era's decision, but a
		// record whose owner positively removed the binding (the nic is
		// gone from the live spec) or whose objects are authoritatively
		// gone must not be resurrected - a republished orphan permanently
		// consumes the address, because no reconciliation is left which
		// could ever release it. an owner whose absence cannot be
		// established keeps its conservative protection (fail closed: a
		// transiently unreadable object or a vmnetcfg which a live vm is
		// about to reconstruct must not drop a claim the guest may still
		// hold)
		if c.verifyLedgerOwner(pool, namespace, vmName, hwAddr, ip) == ownerGone {
			log.Warnf("(ippool.protectPersistedClaims) IPPool %s carries the allocation record %q for ip %s whose owner is authoritatively gone, dropping it instead of resurrecting it",
				pool.Name, ownerRef, ip)
			c.metrics.UpdateLogStatus("warning")

			continue
		}

		// a claim outside the pool range can never be handed out by the
		// allocator, so publishing it keeps the durable record without an
		// exposure window; the range may grow back (which triggers an
		// application reinitialization) and the next registration pins it
		if !ipWithinPoolRange(pool, ip) {
			log.Warnf("(ippool.protectPersistedClaims) IPPool %s carries the allocation record %q for ip %s outside its pool range, skipping the pin",
				pool.Name, ownerRef, ip)

			claims[ip] = ownerRef

			continue
		}

		if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, ip, ownerRef); err != nil {
			// a claim which fights the exclude pass or an already-reclaimed
			// address must surface: publishing the allocator in this state
			// would offer or drop a bound address
			return nil, fmt.Errorf("error while protecting ip [%s] of IPPool %s in IPAM for network [%s]: %s",
				ip, pool.Name, pool.Spec.NetworkName, err.Error())
		}

		claims[ip] = ownerRef
		pinnedIPs[ip] = true
	}

	// the ledger is not a complete inventory of the durable claims: pin the
	// assignments the vmnetcfg objects record for this network as well. the
	// list is the authoritative cluster-wide snapshot, so the sweep covers
	// every namespace and every nic, and a claim whose ledger entry was
	// lost (a historical partial write) is protected like any other
	vmnetcfgList, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs("").List(
		c.ctx, metav1.ListOptions{},
	)
	if err != nil {
		// an incomplete snapshot must not publish the pool: an unseen claim
		// would be handed to a fresh allocation. the registration fails and
		// its retry re-runs the whole protection
		return nil, fmt.Errorf("error while listing the VirtualMachineNetworkConfigs for the claims of IPPool %s: %w", pool.Name, err)
	}

	// the sweep admits claims through the same rules the binding replay
	// applies, so it never pre-assigns an address to a request which the
	// replay itself rejects, and an established assignment outranks a bare
	// request regardless of the list order:
	//
	//   - an object which is being deleted is cleaned up by its deletion
	//     path, so nothing is pinned on its behalf
	//   - a status-less object created while the previous process era was
	//     still serving (its creation timestamp falls after the pool's
	//     last status update) is the hijack guard case of the binding
	//     replay: the replay rejects it as a possible ip hijack, so the
	//     sweep must not reserve its requested address either - otherwise
	//     the rejected request would preempt the established assignment
	//     which actually owns the address
	//   - a nic whose status entry carries ERROR is skipped by the replay,
	//     so its spec address is not claimed on its behalf either
	//   - a claim whose nic carries a status entry (an assignment the
	//     binding controller already established) outranks a status-less
	//     request, so the list order cannot elevate a bare request above
	//     an established assignment
	established := []specClaim{}
	requests := []specClaim{}

	for i := range vmnetcfgList.Items {
		vmnetcfg := &vmnetcfgList.Items[i]

		if vmnetcfg.DeletionTimestamp != nil {
			continue
		}

		hasOwnedStatus := false
		for _, nic := range vmnetcfg.Status.NetworkConfig {
			if util.QualifyNetworkName(vmnetcfg.Namespace, nic.NetworkName) == pool.Spec.NetworkName {
				hasOwnedStatus = true
				break
			}
		}
		if !hasOwnedStatus &&
			!cPool.Status.LastUpdate.IsZero() &&
			vmnetcfg.CreationTimestamp.After(cPool.Status.LastUpdate.Time) {
			// the binding replay rejects this object as a manually created
			// one which could hijack an existing assignment, so its
			// recorded request is not honored as a claim either
			log.Warnf("(ippool.protectPersistedClaims) VirtualMachineNetworkConfig %s/%s was created after the last status update of IPPool %s while carrying no status, not honoring its recorded addresses as claims (possible ip hijack)",
				vmnetcfg.Namespace, vmnetcfg.Name, pool.Name)

			continue
		}

		for _, v := range vmnetcfg.Spec.NetworkConfig {
			if v.IPAddress == "" || util.QualifyNetworkName(vmnetcfg.Namespace, v.NetworkName) != pool.Spec.NetworkName {
				continue
			}

			nicStatus, hasStatus := "", false
			for _, nic := range vmnetcfg.Status.NetworkConfig {
				if util.CanonicalHWAddr(v.MACAddress) == util.CanonicalHWAddr(nic.MACAddress) &&
					util.QualifyNetworkName(vmnetcfg.Namespace, nic.NetworkName) == pool.Spec.NetworkName {
					nicStatus, hasStatus = nic.Status, true

					break
				}
			}

			if hasStatus && nicStatus == "ERROR" {
				// the replay skips this nic, so its address is not claimed
				// on its behalf: an earlier rejection stays a rejection
				continue
			}

			_, macErr := net.ParseMAC(v.MACAddress)

			claim := specClaim{
				namespace: vmnetcfg.Namespace,
				name:      vmnetcfg.Name,
				vmRef:     fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Spec.VMName),
				// the claim identity carries the canonical mac spelling:
				// the re-verification compares it against a fresh read of
				// the spec, and a spelling drift between the list snapshot
				// and the fresh read (02-AA-BB-CC-DD-01 ->
				// 02:aa:bb:cc:dd:01) must not turn the unchanged logical
				// owner into a removed one whose pin gets dropped
				mac:   util.CanonicalHWAddr(v.MACAddress),
				ip:    v.IPAddress,
				named: macErr == nil,
			}
			if claim.named {
				claim.ownerRef = util.AllocationRef(vmnetcfg.Namespace, vmnetcfg.Spec.VMName, v.MACAddress)
			}

			if hasStatus {
				established = append(established, claim)
			} else {
				requests = append(requests, claim)
			}
		}
	}

	// a pinned spec claim is reserved in the allocator only: it is NOT
	// republished into the pool status. the restoring binding writes its
	// own ledger entry when it reclaims the address, so the record and
	// the reservation can never disagree, and a claim which goes stale
	// (its nic was removed while the pool was still unpublished) leaves
	// no orphan record behind which a fresh helper restart would treat
	// as authoritative and reserve again
	pinnedClaims := []specClaim{}
	// skippedClaims records the claims a decided address displaced: when
	// the winner of an address is dropped during the re-verification, the
	// survivors are re-evaluated so the address never becomes allocatable
	// while a live object still records it (the dropped winner reopens
	// its own pin, so the promotion re-runs the exact admission rules)
	skippedClaims := make(map[string][]specClaim)

	pinClaim := func(claim specClaim) {
		if pinnedIPs[claim.ip] {
			// the ledger already decided this address: a spec claim which
			// disagrees with a recorded owner is a genuine conflict which
			// stays with the recorded owner (the binding restore surfaces
			// it visibly), and an agreeing claim is already pinned. a
			// displaced claimant of a spec-pinned address is recorded as
			// a survivor, because the pin it lost to can still be dropped
			// by the re-verification below
			skippedClaims[claim.ip] = append(skippedClaims[claim.ip], claim)

			return
		}

		if !ipWithinPoolRange(pool, claim.ip) {
			// outside the pool range the allocator can never hand the
			// address out, and the binding restore of such a claim fails
			log.Warnf("(ippool.protectPersistedClaims) VirtualMachineNetworkConfig %s/%s records the ip %s outside the pool range of network %s, skipping the pin",
				claim.namespace, claim.name, claim.ip, pool.Spec.NetworkName)

			return
		}

		if !claim.named {
			// an invalid macaddress cannot form an owner identity, so the
			// binding could never reclaim the address under it: protect it
			// without an owner (never double-bound) instead of leaving it
			// to a fresh allocation while the guest may still run with
			// it. the pin is attributed to the claiming vm, so the binding
			// of that vm retakes it once the macaddress is corrected
			log.Warnf("(ippool.protectPersistedClaims) VirtualMachineNetworkConfig %s/%s records ip %s for the invalid macaddress %q of network %s, protecting the address without an owner identity",
				claim.namespace, claim.name, claim.ip, claim.mac, pool.Spec.NetworkName)

			if err := c.ipam.ProtectIP(pool.Spec.NetworkName, claim.ip, claim.vmRef); err != nil {
				log.Warnf("(ippool.protectPersistedClaims) cannot protect the recorded ip %s of VirtualMachineNetworkConfig %s/%s: %s",
					claim.ip, claim.namespace, claim.name, err.Error())

				return
			}

			pinnedIPs[claim.ip] = true
			pinnedClaims = append(pinnedClaims, claim)

			return
		}

		if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, claim.ip, claim.ownerRef); err != nil {
			// a claim which fights the exclude pass, the ledger pin or
			// another spec claim must not overwrite that ownership and
			// must not block the protection of the remaining claims: the
			// conflicting binding surfaces the conflict visibly when it
			// restores
			log.Warnf("(ippool.protectPersistedClaims) cannot pin the recorded ip %s of VirtualMachineNetworkConfig %s/%s under the owner %q: %s",
				claim.ip, claim.namespace, claim.name, claim.ownerRef, err.Error())

			return
		}

		pinnedIPs[claim.ip] = true
		pinnedClaims = append(pinnedClaims, claim)
	}

	for _, claim := range established {
		pinClaim(claim)
	}
	for _, claim := range requests {
		pinClaim(claim)
	}

	// specStillRecordsClaim reports whether the fresh read of a claiming
	// object still carries the nic the claim was made for: the comparison
	// matches the canonical mac spelling of both sides, so a formatting
	// drift of the unchanged logical owner is not a removal
	specStillRecordsClaim := func(vmnetcfg *kihv1.VirtualMachineNetworkConfig, claim specClaim) bool {
		for _, v := range vmnetcfg.Spec.NetworkConfig {
			if util.CanonicalHWAddr(v.MACAddress) == claim.mac &&
				util.QualifyNetworkName(vmnetcfg.Namespace, v.NetworkName) == pool.Spec.NetworkName && v.IPAddress == claim.ip {
				return true
			}
		}

		return false
	}

	// promoteSurvivors re-evaluates the claims a dropped winner displaced:
	// the drop reopened the address, so a survivor whose live object still
	// records it retakes the protection through the regular admission
	// rules before the registration publishes anything. without the
	// promotion the address would be allocatable although a live object
	// records it, and a fresh allocation could double-bind it. a survivor
	// which verifies as stale is skipped; one which cannot be verified
	// fails the registration (fail closed, the retried registration
	// re-runs the whole protection)
	promoteSurvivors := func(dropped specClaim) error {
		if len(skippedClaims[dropped.ip]) == 0 {
			return nil
		}

		pinnedIPs[dropped.ip] = false

		survivors := skippedClaims[dropped.ip]
		delete(skippedClaims, dropped.ip)

		for _, survivor := range survivors {
			vmnetcfg, getErr := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(survivor.namespace).Get(
				c.ctx, survivor.name, metav1.GetOptions{},
			)
			if getErr != nil {
				if apierrors.IsNotFound(getErr) {
					// the survivor is stale by definition: its object is
					// gone, so nothing records the address on its behalf
					continue
				}

				return fmt.Errorf("error while re-evaluating the displaced claim of VirtualMachineNetworkConfig %s/%s for IPPool %s: %w",
					survivor.namespace, survivor.name, pool.Name, getErr)
			}

			if !specStillRecordsClaim(vmnetcfg, survivor) {
				continue
			}

			log.Warnf("(ippool.protectPersistedClaims) promoting the surviving claim of VirtualMachineNetworkConfig %s/%s for the ip %s of IPPool %s after its winning claimant was dropped",
				survivor.namespace, survivor.name, survivor.ip, pool.Name)

			// the promotion is the survivor's own admission: a further
			// displaced claimant of the same address records as a survivor
			// again, because this pin can also be dropped by a later
			// iteration of the re-verification
			pinClaim(survivor)
		}

		return nil
	}

	// re-verify every pinned spec claim against a fresh read of its object
	// before the registration publishes anything: the list snapshot can
	// capture a nic which a concurrent vm cleanup removed while the pool
	// was still unpublished, and a stale pin would keep the removed nic's
	// address reserved with no reconciliation left to free it (the object
	// does not reference the address anymore). every interleaving
	// converges: a cleanup which lands before the re-read is caught here,
	// a cleanup which lands after it releases the pin itself through the
	// owner-validated release, and the spec pins carry no ledger entry,
	// so even a process crash between the pin and this verification can
	// never resurrect the claim on the next restart. a dropped winner
	// promotes its displaced survivors, so the address keeps the
	// protection of whichever live object still records it
	for _, claim := range pinnedClaims {
		vmnetcfg, getErr := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(claim.namespace).Get(
			c.ctx, claim.name, metav1.GetOptions{},
		)
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				// the object is gone, so its claim is stale by definition
				c.dropSpecPin(pool, claim)

				if err := promoteSurvivors(claim); err != nil {
					return nil, err
				}

				continue
			}

			// an unverifiable claim must fail the registration instead of
			// publishing a pin nobody vouches for anymore; the retried
			// registration re-runs the whole protection
			return nil, fmt.Errorf("error while verifying the recorded claim of VirtualMachineNetworkConfig %s/%s for IPPool %s: %w",
				claim.namespace, claim.name, pool.Name, getErr)
		}

		if !specStillRecordsClaim(vmnetcfg, claim) {
			c.dropSpecPin(pool, claim)

			if err := promoteSurvivors(claim); err != nil {
				return nil, err
			}
		}
	}

	return claims, nil
}

// ownerLiveness classifies the revalidation of a persisted ledger owner:
// the authoritative absence (ownerGone) may drop the record, a live owner
// (ownerLive) keeps it, and an owner whose liveness could not be
// established (ownerUnverified) is interpreted fail-closed by every
// consumer which must not drop a record on one failed read.
type ownerLiveness int

const (
	ownerGone ownerLiveness = iota
	ownerLive
	ownerUnverified
)

// verifyLedgerOwner revalidates the owner of a persisted ledger entry
// before the registration republishes it. the verdict distinguishes the
// authoritative absence, which may drop the record, from the uncertain
// absence, which must keep it:
//
//   - the owning vmnetcfg exists and its spec still records the binding
//     (canonical mac, this network, this address): the owner is live.
//   - the owning vmnetcfg exists but no longer records the binding: the
//     owner positively removed it (a nic edit or a completed move), so
//     the record is stale and must not be resurrected.
//   - the owning vmnetcfg is gone: only a VirtualMachine which is gone as
//     well is the authoritative absence (a live vm's controller recreates
//     its vmnetcfg, and the recreated binding reclaims the address), so
//     the vm existence decides. a missing verifier (tests, a client which
//     could not be built) is unverifiable and the consumer keeps the
//     record.
//   - any read which fails transiently is unverifiable: a claim the guest
//     may still hold must not be dropped because one api read failed, and
//     the next registration revalidates it again.
func (c *Controller) verifyLedgerOwner(pool *kihv1.IPPool, namespace string, vmName string, hwAddr string, ip string) ownerLiveness {
	vmnetcfg, getErr := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(namespace).Get(
		c.ctx, vmName, metav1.GetOptions{},
	)
	if getErr == nil {
		for _, v := range vmnetcfg.Spec.NetworkConfig {
			if util.CanonicalHWAddr(v.MACAddress) == util.CanonicalHWAddr(hwAddr) &&
				util.QualifyNetworkName(vmnetcfg.Namespace, v.NetworkName) == pool.Spec.NetworkName && v.IPAddress == ip {
				return ownerLive
			}
		}

		return ownerGone
	}

	if !apierrors.IsNotFound(getErr) {
		log.Warnf("(ippool.verifyLedgerOwner) cannot verify the owner %s/%s of the recorded ip %s of IPPool %s, keeping the record: %s",
			namespace, vmName, ip, pool.Name, getErr.Error())
		c.metrics.UpdateLogStatus("warning")

		return ownerUnverified
	}

	if c.verifyVM == nil {
		log.Warnf("(ippool.verifyLedgerOwner) cannot verify the virtualmachine %s/%s behind the missing VirtualMachineNetworkConfig of the recorded ip %s of IPPool %s, keeping the record",
			namespace, vmName, ip, pool.Name)

		return ownerUnverified
	}

	vmExists, vmErr := c.verifyVM(namespace, vmName)
	if vmErr != nil {
		log.Warnf("(ippool.verifyLedgerOwner) cannot verify the virtualmachine %s/%s of the recorded ip %s of IPPool %s, keeping the record: %s",
			namespace, vmName, ip, pool.Name, vmErr.Error())
		c.metrics.UpdateLogStatus("warning")

		return ownerUnverified
	}

	if vmExists {
		return ownerLive
	}

	return ownerGone
}

// excludeEntryConflicts verifies the persisted ledger record of an exclude
// entry against its owner and reports whether the record belongs to a
// genuinely live binding, which is a definitive configuration conflict.
// the up-front admission checks of the registration and the update must
// reject an exclude entry only in that case, because a stale record whose
// owner is authoritatively gone (the helper was down while the vm was
// deleted, so no cleanup un-recorded it) would otherwise make the pool
// permanently unregistrable although the same registration's claim
// protection would drop the record. an owner whose liveness cannot be
// verified is neither a conflict nor a stale record: the returned error
// keeps the admission retriable (a plain registration error requeues and
// resyncs, the startup gate stays open), instead of the definitive
// ErrPoolUnregistrable rejection, which would settle the gate on the
// first failed api read and tell the operator to hand-edit a possibly
// healthy object. an unparseable reference stays conservative and blocks:
// its owner cannot be verified at all, so the address must not be offered
// to a guest (fail closed, like the unparseable pins of
// protectPersistedClaims).
func (c *Controller) excludeEntryConflicts(pool *kihv1.IPPool, ip string, ref string) (bool, error) {
	namespace, vmName, hwAddr, ok := util.ParseAllocationRef(ref)
	if !ok {
		log.Warnf("(ippool.excludeEntryConflicts) IPPool %s carries the unparseable allocation reference %q for the exclude entry %s, treating it as a live claim",
			pool.Name, ref, ip)
		c.metrics.UpdateLogStatus("warning")

		return true, nil
	}

	switch c.verifyLedgerOwner(pool, namespace, vmName, hwAddr, ip) {
	case ownerGone:
		log.Warnf("(ippool.excludeEntryConflicts) the exclude entry %s of IPPool %s is recorded for the owner %s/%s whose binding is authoritatively gone, ignoring the stale record",
			ip, pool.Name, namespace, vmName)
		c.metrics.UpdateLogStatus("warning")

		return false, nil
	case ownerUnverified:
		return false, fmt.Errorf("cannot verify the owner %s/%s of the recorded exclude entry %s of IPPool %s",
			namespace, vmName, ip, pool.Name)
	}

	return true, nil
}

// dropSpecPin releases a spec-claim pin whose recorded nic does not exist
// anymore: the re-verification of the claim sweep calls it when the fresh
// read of the claiming object no longer carries the nic the pin was made
// for. the releases are owner-validated, so a successor which took the
// address over in the meantime is never freed with it. the pool is not
// published yet while this runs, so no binding allocation can interfere:
// the only other writers of the allocator are the cleanups, whose releases
// are owner-validated as well and converge with this one in any order.
func (c *Controller) dropSpecPin(pool *kihv1.IPPool, claim specClaim) {
	log.Warnf("(ippool.dropSpecPin) the recorded ip %s of VirtualMachineNetworkConfig %s/%s does not exist anymore, dropping its protection pin",
		claim.ip, claim.namespace, claim.name)

	if claim.named {
		if err := c.ipam.ReleaseIPOwnedBy(pool.Spec.NetworkName, claim.ip, claim.ownerRef); err != nil &&
			!errors.Is(err, ipam.ErrIPForeignOwner) && !util.IsAlreadyReleased(err) {
			log.Errorf("(ippool.dropSpecPin) cannot drop the pin of ip %s of VirtualMachineNetworkConfig %s/%s: %s",
				claim.ip, claim.namespace, claim.name, err.Error())
			c.metrics.UpdateLogStatus("error")
		}

		return
	}

	// the ownerless pin of an unusable-mac claim: nothing but this
	// registration can have touched the unpublished allocator, so the
	// plain release drops exactly the pin the sweep just made
	if err := c.ipam.ReleaseIP(pool.Spec.NetworkName, claim.ip); err != nil && !util.IsAlreadyReleased(err) {
		log.Errorf("(ippool.dropSpecPin) cannot drop the ownerless pin of ip %s of VirtualMachineNetworkConfig %s/%s: %s",
			claim.ip, claim.namespace, claim.name, err.Error())
		c.metrics.UpdateLogStatus("error")
	}
}

// ipWithinPoolRange reports whether an address lies inside the inclusive
// start..end range of the pool specification.
func ipWithinPoolRange(pool *kihv1.IPPool, ip string) bool {
	ipAddr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}

	startAddr, err := netip.ParseAddr(pool.Spec.IPv4Config.Pool.Start)
	if err != nil {
		return false
	}

	endAddr, err := netip.ParseAddr(pool.Spec.IPv4Config.Pool.End)
	if err != nil {
		return false
	}

	return startAddr.Compare(ipAddr) <= 0 && ipAddr.Compare(endAddr) <= 0
}

// resetIPPoolStatus republishes the pool status after a registration: the
// allocation map carries the excluded addresses and the claimed addresses
// protected by the fresh allocator, so restored bindings find their
// ownership records again while the addresses stay unavailable to fresh
// allocations.
func (c *Controller) resetIPPoolStatus(pool *kihv1.IPPool, protectedClaims map[string]string) (uPool *kihv1.IPPool, err error) {
	cPool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(c.ctx, pool.Name, metav1.GetOptions{})
	if err != nil {
		return uPool, err
	}
	if err := c.verifyPoolIdentity(cPool); err != nil {
		return nil, err
	}

	// if the timestamp is not set, set it to the current local time
	if cPool.Status.LastUpdate.IsZero() {
		cPool.Status.LastUpdateBeforeStart = metav1.Now()
	} else {
		// save the last status update to handle the vmnetcfg objects when the program is (re)started
		cPool.Status.LastUpdateBeforeStart = cPool.Status.LastUpdate
	}

	cPool.Status.LastUpdate = metav1.Now()

	allocatedExcludes := make(map[string]string)
	for _, v := range pool.Spec.IPv4Config.Pool.Exclude {
		allocatedExcludes[v] = ipam.ExcludedOwner
	}
	for ip, ref := range protectedClaims {
		allocatedExcludes[ip] = ref
	}
	cPool.Status.IPv4.Allocated = allocatedExcludes
	cPool.Status.IPv4.Used = c.ipam.Used(pool.Spec.NetworkName)
	cPool.Status.IPv4.Available = c.ipam.Available(pool.Spec.NetworkName)

	uPool, err = c.kihClientset.KubevirtiphelperV1().IPPools().UpdateStatus(c.ctx, cPool, metav1.UpdateOptions{})
	if err != nil {
		return uPool, err
	}

	return
}

func (c *Controller) resetIPPoolMetrics(pool *kihv1.IPPool) (err error) {
	cPool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(c.ctx, pool.Name, metav1.GetOptions{})
	if err != nil {
		return
	}
	if err := c.verifyPoolIdentity(cPool); err != nil {
		return err
	}

	c.metrics.UpdateIPPoolUsed(cPool.Name, cPool.Spec.IPv4Config.Subnet, cPool.Spec.NetworkName, cPool.Status.IPv4.Used)
	c.metrics.UpdateIPPoolAvailable(cPool.Name, cPool.Spec.IPv4Config.Subnet, cPool.Spec.NetworkName, cPool.Status.IPv4.Available)

	return
}
