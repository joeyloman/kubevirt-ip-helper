package app

import (
	// log "github.com/sirupsen/logrus"

	"k8s.io/client-go/kubernetes"

	"kubevirt.io/client-go/kubecli"
)

func Run(kubevirt_kubecli kubecli.KubevirtClient, k8s_clientset *kubernetes.Clientset) {
	// start watching the vmi events
	watchEvents(kubevirt_kubecli, k8s_clientset)
}
