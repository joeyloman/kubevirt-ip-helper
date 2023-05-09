package kubevirtiphelper

import (
	kviphv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kviphclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"

	log "github.com/sirupsen/logrus"
)

func AllocateIPPool(pool *kviphv1.IPPool, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(AllocateIPPool) poolobj added: [%+v]\n", pool)

	return err
}

func RemoveIPPool(pool *kviphv1.IPPool, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(RemoveIPPool) poolobj removed: [%+v]\n", pool)

	return err
}

func UpdateIPPool(oldPool *kviphv1.IPPool, newPool *kviphv1.IPPool, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(UpdateIPPool) poolobj updated: oldPool [%+v] / newPool [%+v]\n", oldPool, newPool)

	return err
}
