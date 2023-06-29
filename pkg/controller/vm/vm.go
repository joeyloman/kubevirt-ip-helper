package vm

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubevirtV1 "kubevirt.io/api/core/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

func (c *Controller) createVirtualMachineNetworkConfigObject(vm *kubevirtV1.VirtualMachine) (err error) {
	var netCfgs []kihv1.VirtualMachineNetworkConfigs

	log.Infof("(vm.createVirtualMachineNetworkConfig) creating vmnetcfg object")

	newVmNetCfg := kihv1.VirtualMachineNetworkConfig{}
	newVmNetCfg.ObjectMeta.Name = vm.ObjectMeta.Name
	newVmNetCfg.ObjectMeta.Namespace = vm.ObjectMeta.Namespace
	newVmNetCfg.Spec.VMName = vm.ObjectMeta.Name

	for _, nic := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		for _, net := range vm.Spec.Template.Spec.Networks {
			if nic.Name == net.Name {
				if net.Multus == nil {
					log.Warnf("(vm.createVirtualMachineNetworkConfig) unsupported network type found!")
				} else if nic.MacAddress == "" {
					log.Errorf("(vm.createVirtualMachineNetworkConfig) no mac address found for vm [%s/%s]",
						vm.ObjectMeta.Namespace, vm.ObjectMeta.Name)
				} else if net.Multus.NetworkName == "" {
					log.Errorf("(vm.createVirtualMachineNetworkConfig) no networkname found for vm [%s/%s]",
						vm.ObjectMeta.Namespace, vm.ObjectMeta.Name)
				} else {
					netCfg := kihv1.VirtualMachineNetworkConfigs{}
					netCfg.MACAddress = nic.MacAddress
					netCfg.NetworkName = net.Multus.NetworkName
					netCfgs = append(netCfgs, netCfg)
				}
			}
		}
	}

	if len(netCfgs) < 1 {
		errMsg := fmt.Sprintf("(vm.createVirtualMachineNetworkConfig) no network configuration found for vm [%s/%s]",
			vm.ObjectMeta.Namespace, vm.ObjectMeta.Name)

		return errors.New(errMsg)
	}

	newVmNetCfg.Spec.VirtualMachineNetworkConfigs = netCfgs

	// TODO: remove
	log.Infof("(vm.createVirtualMachineNetworkConfig) returning: %+v", newVmNetCfg)

	vmNetCfgObj, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(newVmNetCfg.Namespace).Create(context.TODO(), &newVmNetCfg, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("(vm.createVirtualMachineNetworkConfig) cannot create VirtualMachineNetworkConfig object for vm [%s/%s]: %s",
			vm.ObjectMeta.Namespace, vm.ObjectMeta.Name, err.Error())

		return
	}

	log.Infof("(vm.createVirtualMachineNetworkConfig) succesfully created vmnetcfg object [%s/%s] for vm [%s/%s]",
		vmNetCfgObj.ObjectMeta.Namespace, vmNetCfgObj.ObjectMeta.Name, vm.ObjectMeta.Namespace, vm.ObjectMeta.Name)

	// TODO: create the VirtualMachineNetworkConfig object
	// err := vmnetcfg.CreateVirtualMachineNetworkConfigObject(newVmNetCfg)
	// if err != nil {
	// 	return
	// }

	return
}

func getNetworkDetails(vm *kubevirtV1.VirtualMachine, vmNetCfgCache map[string]kihv1.VirtualMachineNetworkConfig) {
	for _, nic := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		for _, net := range vm.Spec.Template.Spec.Networks {
			if nic.Name == net.Name && net.Multus != nil {
				log.Infof("(getNetworkDetails) VIRTUAL MACHINE name=%s, networkname=%s, macaddress=%s",
					vm.ObjectMeta.Name, net.Multus.NetworkName, nic.MacAddress)

				// TODO: check if the mac address already exists in ???

				// TODO: check if the networkname is in the IPPools cache

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
				//(*vmNetCfgCache)[nic.MacAddress] = vmNetCfgObj
				vmNetCfgCache[nic.MacAddress] = vmNetCfgObj
			}
		}
	}
}

func tempPrintRegisteredVMs(vmNetCfgCache map[string]kihv1.VirtualMachineNetworkConfig) {
	//for mac, res := range *vmNetCfgCache {
	for mac, res := range vmNetCfgCache {
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
