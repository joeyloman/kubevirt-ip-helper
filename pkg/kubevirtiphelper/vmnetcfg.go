package kubevirtiphelper

import (
	kviphv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kviphclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"

	log "github.com/sirupsen/logrus"
)

func AllocateVirtualMachineNetworkConfig(reservation *kviphv1.VirtualMachineNetworkConfig, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(AllocateVirtualMachineNetworkConfig) obj added: [%+v]\n", reservation)

	return err
}

func RemoveVirtualMachineNetworkConfig(reservation *kviphv1.VirtualMachineNetworkConfig, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(RemoveVirtualMachineNetworkConfig) obj removed: [%+v]\n", reservation)

	return err
}

func UpdateVirtualMachineNetworkConfig(old *kviphv1.VirtualMachineNetworkConfig, new *kviphv1.VirtualMachineNetworkConfig, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(UpdateVirtualMachineNetworkConfig) obj updated: old [%+v] / new [%+v]\n", old, new)

	return err
}
