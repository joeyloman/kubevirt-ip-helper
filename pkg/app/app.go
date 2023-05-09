package app

import (
	// log "github.com/sirupsen/logrus"

	"k8s.io/client-go/kubernetes"

	"kubevirt.io/client-go/kubecli"
	//dhcpd "github.com/joeyloman/kubevirt-ip-helper/dhcpd"
)

func Run(kubevirt_kubecli kubecli.KubevirtClient, k8s_clientset *kubernetes.Clientset) {
	// start the dhcpd service
	//go dhcpd.DhcpdRun()

	// start watching the vmi events
	watchEvents(kubevirt_kubecli, k8s_clientset)
}
