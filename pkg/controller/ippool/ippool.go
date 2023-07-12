package ippool

import (
	"net"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	log "github.com/sirupsen/logrus"
)

func (c *Controller) registerIPPool(pool *kihv1.IPPool) (err error) {
	log.Infof("(ippool.registerIPPool) new IPPool added [%s]", pool.Name)

	c.ipPoolCache[pool.Spec.NetworkName] = *pool

	// DEBUG
	ifaces, err := util.ListInterfaces()
	if err != nil {
		return
	}
	log.Infof("(ippool.registerIPPool) network interfaces: %+v", ifaces)
	// end DEBUG

	nic, err := util.GetNicFromIp(net.ParseIP(pool.Spec.IPv4Config.ServerIP))
	if err != nil {
		return
	}

	log.Infof("(ippool.registerIPPool) nic found: [%s]", nic)

	// start a dhcp service thread if the serverip is bound to a nic
	if nic != "" {
		c.dhcp.Run(nic, pool.Spec.IPv4Config.ServerIP)
	}

	return c.ipam.NewSubnet(
		pool.Spec.NetworkName,
		pool.Spec.IPv4Config.Subnet,
		pool.Spec.IPv4Config.Pool.Start,
		pool.Spec.IPv4Config.Pool.End,
	)
}

func (c *Controller) cleanupIPPoolObjects(pool kihv1.IPPool) (err error) {
	log.Infof("(ippool.cleanupIPPoolObjects) starting cleanup of IPPool %s", pool.Name)

	nic, err := util.GetNicFromIp(net.ParseIP(pool.Spec.IPv4Config.ServerIP))
	if err != nil {
		return
	}

	log.Infof("(ippool.cleanupIPPoolObjects) nic found: [%s]", nic)

	if nic != "" {
		err := c.dhcp.Stop(nic)
		if err != nil {
			log.Errorf("(ippool.cleanupIPPoolObjects) error while stopping DHCP service on nic %s", err.Error())
		}
	}

	c.ipam.DeleteSubnet(pool.Spec.NetworkName)

	return
}

func updateIPPool(oldPool *kihv1.IPPool, newPool *kihv1.IPPool) error {
	var err error

	// TODO

	log.Tracef("(UpdateIPPool) poolobj updated: oldPool [%+v] / newPool [%+v]\n", oldPool, newPool)

	return err
}

func printIPPoolcache(ipPoolCache map[string]kihv1.IPPool) (err error) {
	for subnet, pool := range ipPoolCache {
		log.Printf("ipPoolCache: key=%s, subnet=%s, network=%s, serverip=%s",
			subnet, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, pool.Spec.IPv4Config.ServerIP)
	}

	return err
}
