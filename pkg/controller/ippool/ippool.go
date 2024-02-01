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
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	log "github.com/sirupsen/logrus"
)

const (
	APP_INIT    = 0
	APP_RUNNING = 1
	APP_RESTART = 2

	IPPOOL_NOCHANGE = 0
	IPPOOL_RELOAD   = 1
	IPPOOL_RESTART  = 2
)

func (c *Controller) registerIPPool(pool *kihv1.IPPool) (err error) {
	log.Infof("(ippool.registerIPPool) [%s] new IPPool added", pool.Name)

	// DEBUG
	// ifaces, err := util.ListInterfaces()
	// if err != nil {
	// 	return
	// }
	// log.Debugf("(ippool.registerIPPool) network interfaces: %+v", ifaces)
	// end DEBUG

	nic, err := util.GetNicFromIp(net.ParseIP(pool.Spec.IPv4Config.ServerIP))
	if err != nil {
		return
	}

	log.Debugf("(ippool.registerIPPool) [%s] nic found: [%s]", pool.Name, nic)

	if err := c.createOrUpdateDHCPPool(pool); err != nil {
		log.Errorf("(ippool.handleIPPoolObjectChange) error while updating dhcppool [%s]: %s", pool.Spec.NetworkName, err)
	}

	// start a dhcp service thread if the serverip is bound to a nic
	if nic != "" {
		c.dhcp.Run(nic, pool.Spec.IPv4Config.ServerIP)
	}

	// register the new subnet in ipam
	if err = c.ipam.NewSubnet(
		pool.Spec.NetworkName,
		pool.Spec.IPv4Config.Subnet,
		pool.Spec.IPv4Config.Pool.Start,
		pool.Spec.IPv4Config.Pool.End,
	); err != nil {
		return
	}

	// mark the exclude ips as used
	for _, v := range pool.Spec.IPv4Config.Pool.Exclude {
		ip, err := c.ipam.GetIP(pool.Spec.NetworkName, v)
		if err != nil {
			return fmt.Errorf("(ippool.registerIPPool) [%s] ipam error while excluding ip [%s]: %s",
				pool.Name, v, err)
		}

		// maybe unnecesarry check, but just to make sure
		if ip != v {
			return fmt.Errorf("(ippool.registerIPPool) [%s] got ip [%s] from ipam, but it doesn't match the exclude ip [%s]",
				pool.Name, ip, v)
		}
	}

	// rebuild the pool status after restarting the process
	rPool, err := c.resetIPPoolStatus(pool)
	if err != nil {
		return
	}

	// reset the pool metrics after restarting the process
	if err = c.resetIPPoolMetrics(pool); err != nil {
		return
	}

	// cache the pool with an empty status
	if err = c.cache.Add(rPool); err != nil {
		return
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

		c.stopDHCPListeners()

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
			log.Errorf("(ippool.handleIPPoolObjectChange) error while updating dhcppool [%s]: %s", newPool.Spec.NetworkName, err)
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

func (c *Controller) stopDHCPListeners() (err error) {
	log.Debugf("(ippool.stopDHCPListeners) stopping DHCP listeners")

	poolList, err := c.kihClientset.KubevirtiphelperV1().IPPools().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("(ippool.stopDHCPListeners) cannot get the IPPool list: %s", err.Error())
	}

	for _, pool := range poolList.Items {
		nic, err2 := util.GetNicFromIp(net.ParseIP(pool.Spec.IPv4Config.ServerIP))
		if err != nil {
			return err2
		}

		log.Debugf("(ippool.stopDHCPListeners) [%s] nic found: [%s]", pool.Name, nic)

		if nic != "" {
			err := c.dhcp.Stop(nic)
			if err != nil {
				log.Errorf("(ippool.stopDHCPListeners) [%s] error while stopping DHCP listener on nic %s",
					pool.Name, err.Error())
			}
		}
	}

	return
}

func (c *Controller) cleanupIPPoolObjects(pool *kihv1.IPPool) (err error) {
	log.Debugf("(ippool.cleanupIPPoolObjects) [%s] starting cleanup of IPPool", pool.Name)

	nic, err := util.GetNicFromIp(net.ParseIP(pool.Spec.IPv4Config.ServerIP))
	if err != nil {
		return
	}

	log.Debugf("(ippool.cleanupIPPoolObjects) [%s] nic found: [%s]", pool.Name, nic)

	if nic != "" {
		err := c.dhcp.Stop(nic)
		if err != nil {
			log.Errorf("(ippool.cleanupIPPoolObjects) [%s] error while stopping DHCP service on nic %s",
				pool.Name, err.Error())
		}
	}

	c.ipam.DeleteSubnet(pool.Spec.NetworkName)
	c.dhcp.DeletePool(pool.Spec.NetworkName)
	c.metrics.DeleteIPPool(pool.Name, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName)
	c.cache.Delete("pool", pool.Spec.NetworkName)

	return
}

func (c *Controller) createOrUpdateDHCPPool(pool *kihv1.IPPool) (err error) {
	if c.dhcp.CheckPool(pool.Spec.NetworkName) {
		if err := c.dhcp.DeletePool(pool.Spec.NetworkName); err != nil {
			log.Errorf("(ippool.createOrUpdateDHCPPool) while deleting dhcppool [%s]: %s", pool.Spec.NetworkName, err)
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
	)

	return
}

func (c *Controller) resetIPPoolStatus(pool *kihv1.IPPool) (uPool *kihv1.IPPool, err error) {
	cPool, err := c.kihClientset.KubevirtiphelperV1().IPPools().Get(context.TODO(), pool.Name, metav1.GetOptions{})
	if err != nil {
		return uPool, fmt.Errorf("(ippool.resetIPPoolStatus) [%s] cannot get IPPool: %s", pool.Name, err.Error())
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
		return uPool, fmt.Errorf("(ippool.resetIPPoolStatus) [%s] cannot update status of IPPool: %s",
			cPool.Name, err.Error())
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
