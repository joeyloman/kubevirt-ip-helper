package ippool

import (
	"net"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	log "github.com/sirupsen/logrus"
)

func (c *Controller) registerIPPool(pool *kihv1.IPPool) (err error) {
	log.Tracef("(ippool.registerIPPool) poolobj added: [%+v]", pool)

	c.ipPoolCache[pool.Spec.NetworkName] = *pool

	ifaces, err := util.ListInterfaces()
	if err != nil {
		return
	}
	log.Infof("network interfaces: %+v", ifaces)

	// TODO: get the nic from the serverip
	nic, err := util.GetNicFromIp(net.ParseIP(pool.Spec.IPv4Config.ServerIP))
	if err != nil {
		return
	}

	log.Infof("(ippool.registerIPPool) nic found: [%s]", nic)

	// TODO: start a new DHCP service listener on that nic
	//c.dhcp.Run("eth0")

	return c.ipam.NewSubnet(
		pool.Spec.NetworkName,
		pool.Spec.IPv4Config.Subnet,
		pool.Spec.IPv4Config.Pool.Start,
		pool.Spec.IPv4Config.Pool.End,
	)
}

func (c *Controller) removeIPPool(pool *kihv1.IPPool) (err error) {
	log.Tracef("(RemoveIPPool) poolobj removed: [%+v]\n", pool)

	// TODO: get the nic from the serverip

	// TODO: stop the DHCP service listener from that nic

	c.ipam.DeleteSubnet(pool.Spec.NetworkName)

	return
}

func updateIPPool(oldPool *kihv1.IPPool, newPool *kihv1.IPPool) error {
	var err error

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
