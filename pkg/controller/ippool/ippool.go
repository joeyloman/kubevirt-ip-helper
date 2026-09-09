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
		// a range configuration which can never become valid is a definitive
		// rejection: classify it unregistrable so the startup gate counts the
		// pool instead of waiting forever. the 'already exists' conflict stays
		// a retryable plain error: a half-cleaned registration can still heal
		// through the teardown of the failed attempt.
		if errors.Is(err, ipam.ErrSubnetInvalid) {
			return cleanup, fmt.Errorf("error while allocating a new subnet in IPAM for network [%s]: %s: %w",
				pool.Spec.NetworkName, err.Error(), ErrPoolUnregistrable)
		}

		return cleanup, fmt.Errorf("error while allocating a new subnet in IPAM for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// mark the exclude ips as used
	for _, v := range pool.Spec.IPv4Config.Pool.Exclude {
		ip, err := c.ipam.GetIP(pool.Spec.NetworkName, v)
		if err != nil {
			return cleanup, fmt.Errorf("error while excluding ip [%s] in IPAM for network [%s]: %s", v, pool.Spec.NetworkName, err.Error())
		}

		// maybe unnecesarry check, but just to make sure
		if ip != v {
			return cleanup, fmt.Errorf("error got ip [%s] from IPAM, but it doesn't match the exclude ip [%s] for network [%s]",
				ip, v, pool.Spec.NetworkName)
		}
	}

	// rebuild the pool status after restarting the process
	rPool, err := c.resetIPPoolStatus(pool)
	if err != nil {
		return cleanup, fmt.Errorf("error while restting IPPool status for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// reset the pool metrics after restarting the process
	if err = c.resetIPPoolMetrics(pool); err != nil {
		return cleanup, fmt.Errorf("error while restting IPPool metrics for network [%s]: %s", pool.Spec.NetworkName, err.Error())
	}

	// cache the pool with an empty status
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

func (c *Controller) resetIPPoolStatus(pool *kihv1.IPPool) (uPool *kihv1.IPPool, err error) {
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
		allocatedExcludes[v] = "EXCLUDED"
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
