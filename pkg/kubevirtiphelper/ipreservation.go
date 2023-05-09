package kubevirtiphelper

import (
	kviphv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kviphclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"

	log "github.com/sirupsen/logrus"
)

func AllocateIPReservation(reservation *kviphv1.IPReservation, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(AllocateIPReservation) reservationobj added: [%+v]\n", reservation)

	return err
}

func RemoveIPReservation(reservation *kviphv1.IPReservation, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(RemoveIPReservation) reservationobj removed: [%+v]\n", reservation)

	return err
}

func UpdateIPReservation(oldReservation *kviphv1.IPReservation, newReservation *kviphv1.IPReservation, clientset *kviphclientset.Clientset) error {
	var err error

	log.Tracef("(UpdateIPReservation) reservationobj updated: oldReservation [%+v] / newReservation [%+v]\n", oldReservation, newReservation)

	return err
}
