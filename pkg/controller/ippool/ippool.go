package ippool

import (
	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kviphclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	goipam "github.com/metal-stack/go-ipam"

	log "github.com/sirupsen/logrus"
)

func registerIPPool(pool *kihv1.IPPool, ipPoolCache map[string]kihv1.IPPool, ipam *goipam.Ipamer) (err error) {
	log.Tracef("(ippool.registerIPPool) poolobj added: [%+v]\n", pool)

	ipPoolCache[pool.Spec.IPv4Config.Subnet] = *pool

	// TODO: add to ipam

	return err
}

func removeIPPool(pool *kihv1.IPPool, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(RemoveIPPool) poolobj removed: [%+v]\n", pool)

	return err
}

func updateIPPool(oldPool *kihv1.IPPool, newPool *kihv1.IPPool, clientset *kviphclientset.Clientset) error {
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
