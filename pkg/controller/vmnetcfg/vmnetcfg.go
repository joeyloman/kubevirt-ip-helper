package vmnetcfg

import (
	"net"
	"net/netip"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kviphclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"

	log "github.com/sirupsen/logrus"
)

func (c *Controller) registerVirtualMachineNetworkConfig(vmnetcfg *kihv1.VirtualMachineNetworkConfig) (err error) {
	//log.Infof("(vmnetcfg.registerVirtualMachineNetworkConfig) obj added: [%+v]\n", vmnetcfg)

	log.Infof("(vmnetcfg.registerVirtualMachineNetworkConfig) start")

	// copy the current vmnetcfg object
	newVmNetCfg := vmnetcfg

	// create a new array with VirtualMachineNetworkConfigs
	newVmNetCfgs := []kihv1.NetworkConfig{}

	for _, v := range vmnetcfg.Spec.NetworkConfig {
		// check if the hwaddr is already registered, if so return err
		if c.dhcp.CheckLease(v.MACAddress) {
			log.Errorf("hwaddr %s already exists in the leases, skipping interface", v.MACAddress)
			continue
		}

		// if v.IPAddress is not empty we register it else we get a new one
		ip, err := c.ipam.GetIP(v.NetworkName, v.IPAddress)
		if err != nil {
			log.Errorf("ipam error: %s, skipping interface", err)
			continue
		}

		log.Infof("(vmnetcfg.registerVirtualMachineNetworkConfig) newVmNetCfg ip: [%s]", ip)

		ipnet, err := netip.ParsePrefix(c.ipPoolCache[v.NetworkName].Spec.IPv4Config.Subnet)
		if err != nil {
			return err
		}
		subnetMask := net.CIDRMask(ipnet.Bits(), 32)

		c.dhcp.AddLease(
			v.MACAddress,
			c.ipPoolCache[v.NetworkName].Spec.IPv4Config.ServerIP,
			ip,
			net.IP(subnetMask).String(),
			c.ipPoolCache[v.NetworkName].Spec.IPv4Config.Router,
			c.ipPoolCache[v.NetworkName].Spec.IPv4Config.DNS,
		)
		// if err =! nil {
		// 	log.Errorf("dhcp error: %s, skipping interface", err)
		// 	continue
		// }

		//
		n := kihv1.NetworkConfig{}
		n.IPAddress = ip
		n.MACAddress = v.MACAddress
		n.NetworkName = v.NetworkName
		newVmNetCfgs = append(newVmNetCfgs, n)
	}

	newVmNetCfg.Spec.NetworkConfig = newVmNetCfgs

	//log.Infof("(vmnetcfg.registerVirtualMachineNetworkConfig) newVmNetCfg config: [%+v]", newVmNetCfg)

	// TODO: update vmnetcfg object

	return
}

func RemoveVirtualMachineNetworkConfig(reservation *kihv1.VirtualMachineNetworkConfig, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(RemoveVirtualMachineNetworkConfig) obj removed: [%+v]\n", reservation)

	return err
}

func UpdateVirtualMachineNetworkConfig(old *kihv1.VirtualMachineNetworkConfig, new *kihv1.VirtualMachineNetworkConfig, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(UpdateVirtualMachineNetworkConfig) obj updated: old [%+v] / new [%+v]\n", old, new)

	return err
}

// func printVirtualMachineNetworkConfig(vmNetCfgCache map[string]kihv1.VirtualMachineNetworkConfig) (err error) {
// 	for subnet, pool := range ipPoolCache {
// 		log.Printf("ipPoolCache: key=%s, subnet=%s, network=%s, serverip=%s",
// 			subnet, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, pool.Spec.IPv4Config.ServerIP)
// 	}

// 	return err
// }
