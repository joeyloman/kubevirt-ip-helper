package vmnetcfg

import (
	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kviphclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"

	log "github.com/sirupsen/logrus"
)

// func createVirtualMachineNetworkConfigObject(vmNetCfg *kihv1.VirtualMachineNetworkConfig) (err error) {
// 	log.Tracef("(vmnetcfg.createVirtualMachineNetworkConfigObject) obj added: [%+v]\n", vmNetCfg)

// 	return
// }

func registerVirtualMachineNetworkConfig(vmnetcfg *kihv1.VirtualMachineNetworkConfig, vmNetCfgCache map[string]kihv1.VirtualMachineNetworkConfig) (err error) {
	log.Tracef("(AllocateVirtualMachineNetworkConfig) obj added: [%+v]\n", vmnetcfg)

	//vmNetCfgCache[vmnetcfg.Spec.VirtualMachineNetworkConfigs] = *vmnetcfg

	return err
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
