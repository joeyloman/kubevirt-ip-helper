package vm

import (
	log "github.com/sirupsen/logrus"

	kubevirtV1 "kubevirt.io/api/core/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

func getNetworkDetails(vm *kubevirtV1.VirtualMachine, vmNetCfgCache *map[string]kihv1.VirtualMachineNetworkConfig) {
	for _, nic := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		for _, net := range vm.Spec.Template.Spec.Networks {
			if nic.Name == net.Name && net.Multus != nil {
				log.Infof("(getNetworkDetails) VIRTUAL MACHINE name=%s, networkname=%s, macaddress=%s",
					vm.ObjectMeta.Name, net.Multus.NetworkName, nic.MacAddress)

				// DHCP example
				vmNetCfgObj := kihv1.VirtualMachineNetworkConfig{}
				vmNetCfgObj.Spec.VMName = vm.ObjectMeta.Name

				var VirtualMachineNetworkConfig1 []kihv1.VirtualMachineNetworkConfigs

				vmObj := kihv1.VirtualMachineNetworkConfigs{}
				//vmObj.IPAddress = "192.168.10.20"
				vmObj.MACAddress = nic.MacAddress
				vmObj.NetworkName = net.Multus.NetworkName

				VirtualMachineNetworkConfig1 = append(VirtualMachineNetworkConfig1, vmObj)
				vmNetCfgObj.Spec.VirtualMachineNetworkConfigs = VirtualMachineNetworkConfig1
				(*vmNetCfgCache)[nic.MacAddress] = vmNetCfgObj
			}
		}
	}
}

func tempPrintRegisteredVMs(vmNetCfgCache *map[string]kihv1.VirtualMachineNetworkConfig) {
	for mac, res := range *vmNetCfgCache {
		for i := 0; i < len(res.Spec.VirtualMachineNetworkConfigs); i++ {
			log.Printf("VM in vmNetCfgCache: key=%s, ip=%s, hwaddr=%s, netname=%s",
				mac,
				res.Spec.VirtualMachineNetworkConfigs[i].IPAddress,
				res.Spec.VirtualMachineNetworkConfigs[i].MACAddress,
				res.Spec.VirtualMachineNetworkConfigs[i].NetworkName,
			)
		}
	}
}
