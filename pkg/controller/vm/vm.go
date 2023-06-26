package vm

import (
	log "github.com/sirupsen/logrus"

	kubevirtV1 "kubevirt.io/api/core/v1"
)

func getNetworkDetails(vm *kubevirtV1.VirtualMachine) {
	for _, nic := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		for _, net := range vm.Spec.Template.Spec.Networks {
			if nic.Name == net.Name {
				log.Infof("(getNetworkDetails) VIRTUAL MACHINE name=%s, networkname=%s, macaddress=%s", vm.ObjectMeta.Name, net.Multus.NetworkName, nic.MacAddress)
			}
		}
	}
}
