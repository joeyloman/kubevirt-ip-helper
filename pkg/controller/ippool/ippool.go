package ippool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"time"

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
	// the pool sub-resources (dhcp pool, ipam subnet, cache entry) are all
	// keyed by the networkname, and the allocators start empty on every
	// (re)start: a live dhcp pool under this networkname therefore belongs
	// to another IPPool registration. any later failure path would tear
	// down or silently replace its live allocations, so reject before
	// any sub-resource of this pool is created.
	if c.dhcp.CheckPool(pool.Spec.NetworkName) {
		return cleanup, fmt.Errorf("networkname [%s] is already registered by another IPPool, not touching its live state: %w", pool.Spec.NetworkName, ErrPoolUnregistrable)
	}

	ip4 := fmt.Sprintf("%s/%d", pool.Spec.IPv4Config.ServerIP, ipnet.Bits())
	if err := network.AddIpToNic(pool.Spec.BindInterface, ip4); err != nil {
		return cleanup, fmt.Errorf("error while adding IP4 address [%s] to bind interface [%s] for network [%s]: %s",
			ip4, pool.Spec.BindInterface, pool.Spec.NetworkName, err.Error())
	}

	log.Debugf("(ippool.registerIPPool) added IP4 address [%s] to nic [%s] for network [%s]",
		ip4, pool.Spec.BindInterface, pool.Spec.NetworkName)

	// from here pool sub resources needs to be cleaned up when something goes wrong
	cleanup = true

	// create the new dhcp pool
	if err := c.createOrUpdateDHCPPool(pool); err != nil {
		return cleanup, fmt.Errorf("error while registering DHCP pool for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// start a dhcp service thread
	if err := c.dhcp.Run(pool.Spec.BindInterface, pool.Spec.IPv4Config.ServerIP); err != nil {
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
	// excluded addresses
	if err = c.cache.Add(rPool); err != nil {
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
	if *c.appStatus == APP_INIT {
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

	if oldPool.Spec.NetworkName != newPool.Spec.NetworkName && c.dhcp.CheckPool(newPool.Spec.NetworkName) {
		return fmt.Errorf("(ippool.handleIPPoolObjectChange) rejecting update for [%s]: the networkname [%s] is already registered by another IPPool, keeping the currently registered configuration",
			oldPool.Spec.NetworkName, newPool.Spec.NetworkName)
	}

	for {
		if *c.appStatus != APP_RESTART {
			break
		}

		log.Warnf("(ippool.handleIPPoolObjectChange) application is still in restarting state, waiting until it's reinitialized..")
		time.Sleep(time.Second * 5)
	}

	// the following pool changes need a restart
	if oldPool.Spec.IPv4Config.ServerIP != newPool.Spec.IPv4Config.ServerIP ||
		oldPool.Spec.IPv4Config.Subnet != newPool.Spec.IPv4Config.Subnet ||
		oldPool.Spec.IPv4Config.Pool.Start != newPool.Spec.IPv4Config.Pool.Start ||
		oldPool.Spec.IPv4Config.Pool.End != newPool.Spec.IPv4Config.Pool.End ||
		!reflect.DeepEqual(oldPool.Spec.IPv4Config.Pool.Exclude, newPool.Spec.IPv4Config.Pool.Exclude) ||
		oldPool.Spec.IPv4Config.Router != newPool.Spec.IPv4Config.Router ||
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
		*c.appStatus = APP_RESTART

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

	if c.cache.Check(newPool) {
		if err := c.cache.Delete("pool", newPool.Spec.NetworkName); err != nil {
			return fmt.Errorf("(ippool.handleIPPoolObjectChange) failed to delete pool %s from cache: %s", newPool.Name, err.Error())
		}
	}

	if err := c.cache.Add(newPool); err != nil {
		return fmt.Errorf("(ippool.handleIPPoolObjectChange) failed to add pool %s to cache: %s", newPool.Name, err.Error())
	}

	return
}

func (c *Controller) stopDHCPListener(pool *kihv1.IPPool) {
	if err := c.dhcp.Stop(pool.Spec.BindInterface); err != nil {
		log.Errorf("(ippool.stopDHCPListener) error while shutting down DHCP listener running on nic [%s] for network [%s]: %s",
			pool.Spec.BindInterface, pool.Spec.NetworkName, err.Error())
		c.metrics.UpdateLogStatus("error")
	}
}

func (c *Controller) cleanupIPPoolObjects(pool *kihv1.IPPool) (err error) {
	log.Debugf("(ippool.cleanupIPPoolObjects) [%s] starting cleanup of IPPool", pool.Name)

	c.stopDHCPListener(pool)
	c.ipam.DeleteSubnet(pool.Spec.NetworkName)
	c.dhcp.DeletePool(pool.Spec.NetworkName)
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
	// validate the projection first: only a subnet which parses may replace
	// the active dhcp pool, otherwise a rejected update would destroy the
	// working configuration
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

	// register the new subnet in dhcp
	c.dhcp.AddPool(
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
	)

	return
}

// protectPersistedClaims pins the ownership records the pool status
// survived with into the fresh ipam allocator: a recovering pool which
// registers again after the startup gate dropped its retries must never
// publish an allocator state which offers the bound addresses of live
// bindings to fresh allocations. every claim reference the status carries
// is re-used verbatim, so the restoring vmnetcfg binding reclaims its own
// recorded address idempotently while a foreign allocation is rejected.
func (c *Controller) protectPersistedClaims(pool *kihv1.IPPool) (map[string]string, error) {
	cPool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(
		context.TODO(), pool.Name, metav1.GetOptions{},
	)
	if err != nil {
		// without the persisted status the claims cannot be known: fail
		// the registration instead of publishing an unprotected allocator
		return nil, fmt.Errorf("error while getting IPPool %s: %w", pool.Name, err)
	}

	claims := make(map[string]string)
	for ip, ref := range cPool.Status.IPv4.Allocated {
		if ref == ipam.ExcludedOwner {
			continue
		}

		if _, _, _, ok := util.ParseAllocationRef(ref); !ok {
			// an unparseable claim cannot be attributed to an owner: while
			// it stays inside the pool range the address is protected
			// unconditionally (a wasted address never double-binds one),
			// and outside the range the allocator cannot hand it out at
			// all. the original record is republished either way
			log.Warnf("(ippool.protectPersistedClaims) IPPool %s carries the unparseable allocation reference %q for ip %s",
				pool.Name, ref, ip)

			claims[ip] = ref

			if ipWithinPoolRange(pool, ip) {
				if _, err := c.ipam.GetIP(pool.Spec.NetworkName, ip); err != nil {
					return nil, fmt.Errorf("error while protecting the unparseable claim of ip [%s] of IPPool %s in IPAM for network [%s]: %s",
						ip, pool.Name, pool.Spec.NetworkName, err.Error())
				}
			}

			continue
		}

		// a claim outside the pool range can never be handed out by the
		// allocator, so publishing it keeps the durable record without an
		// exposure window; the range may grow back (which triggers an
		// application reinitialization) and the next registration pins it
		if !ipWithinPoolRange(pool, ip) {
			log.Warnf("(ippool.protectPersistedClaims) IPPool %s carries the allocation record %q for ip %s outside its pool range, skipping the pin",
				pool.Name, ref, ip)

			claims[ip] = ref

			continue
		}

		if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, ip, ref); err != nil {
			// a claim which fights the exclude pass or an already-reclaimed
			// address must surface: publishing the allocator in this state
			// would offer or drop a bound address
			return nil, fmt.Errorf("error while protecting ip [%s] of IPPool %s in IPAM for network [%s]: %s",
				ip, pool.Name, pool.Spec.NetworkName, err.Error())
		}

		claims[ip] = ref
	}

	return claims, nil
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
	cPool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(context.TODO(), pool.Name, metav1.GetOptions{})
	if err != nil {
		return uPool, err
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

	uPool, err = c.kihClientset.KubevirtiphelperV1().IPPools().UpdateStatus(context.TODO(), cPool, metav1.UpdateOptions{})
	if err != nil {
		return uPool, err
	}

	return
}

func (c *Controller) resetIPPoolMetrics(pool *kihv1.IPPool) (err error) {
	cPool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(context.TODO(), pool.Name, metav1.GetOptions{})
	if err != nil {
		return
	}

	c.metrics.UpdateIPPoolUsed(cPool.Name, cPool.Spec.IPv4Config.Subnet, cPool.Spec.NetworkName, cPool.Status.IPv4.Used)
	c.metrics.UpdateIPPoolAvailable(cPool.Name, cPool.Spec.IPv4Config.Subnet, cPool.Spec.NetworkName, cPool.Status.IPv4.Available)

	return
}
