package ippool

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"

	log "github.com/sirupsen/logrus"
)

const (
	IPPOOL_NOCHANGE = 0
	IPPOOL_RELOAD   = 1
	IPPOOL_RESTART  = 2
)

func (c *Controller) registerIPPool(pool *kihv1.IPPool) (cleanup bool, err error) {
	log.Infof("(ippool.registerIPPool) [%s] new IPPool added", pool.Name)

	// by default cleanup the pool sub resources
	cleanup = false

	// add the serverip to the bindinterface
	ipnet, err := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet)
	if err != nil {
		return cleanup, fmt.Errorf("error while parsing subnet [%s] for network [%s]: %s",
			pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, err.Error())
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

	// increase the poolCountCurrent if the application is still initializing
	if *c.appStatus == APP_INIT {
		*c.ippoolCountCurrent++
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
			log.Errorf("(ippool.handleIPPoolObjectChange) error while updating dhcppool [%s]: %s", newPool.Spec.NetworkName, err.Error())
			c.metrics.UpdateLogStatus("error")
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
	if c.dhcp.CheckPool(pool.Spec.NetworkName) {
		if err := c.dhcp.DeletePool(pool.Spec.NetworkName); err != nil {
			log.Errorf("(ippool.createOrUpdateDHCPPool) while deleting dhcppool [%s]: %s", pool.Spec.NetworkName, err.Error())
			c.metrics.UpdateLogStatus("error")
		}
	}

	// convert the subnetmask
	ipnet, err := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet)
	if err != nil {
		// abort update
		return
	}
	subnetMask := net.CIDRMask(ipnet.Bits(), 32)

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
