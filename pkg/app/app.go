package app

import (
	// log "github.com/sirupsen/logrus"

	kviphclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"

	"k8s.io/client-go/kubernetes"

	"kubevirt.io/client-go/kubecli"
	//dhcpd "github.com/joeyloman/kubevirt-ip-helper/dhcpd"
)

func Run(kubevirt_kubecli kubecli.KubevirtClient, kviph_clientset *kviphclientset.Clientset, k8s_clientset *kubernetes.Clientset) {
	// start the dhcpd service
	//go dhcpd.DhcpdRun()

	// start watching the vmi events
	watchEvents(kubevirt_kubecli, kviph_clientset, k8s_clientset)
}
