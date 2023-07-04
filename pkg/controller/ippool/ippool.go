package ippool

import (
	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"

	log "github.com/sirupsen/logrus"
)

func (c *Controller) registerIPPool(pool *kihv1.IPPool) (err error) {
	log.Tracef("(ippool.registerIPPool) poolobj added: [%+v]\n", pool)

	c.ipPoolCache[pool.Spec.NetworkName] = *pool

	return c.ipam.NewSubnet(
		pool.Spec.NetworkName,
		pool.Spec.IPv4Config.Subnet,
		pool.Spec.IPv4Config.Pool.Start,
		pool.Spec.IPv4Config.Pool.End,
	)
}

func (c *Controller) removeIPPool(pool *kihv1.IPPool) (err error) {
	log.Tracef("(RemoveIPPool) poolobj removed: [%+v]\n", pool)

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
