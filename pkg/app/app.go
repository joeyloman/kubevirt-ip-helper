package app

import (
    log "github.com/sirupsen/logrus"

    "k8s.io/client-go/kubernetes"

    kubevirtclientset "kubevirt.io/client-go/kubecli"
)

func Run(kubevirt_clientset *kubevirtclientset.Clientset,k8s_clientset *kubernetes.Clientset) {
    // start watching the vmi events
    watchEvents(kubevirt_clientset, k8s_clientset)
}
