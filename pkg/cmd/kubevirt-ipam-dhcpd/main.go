package main

import (
	"flag"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/joeyloman/kubevirt-ipam-dhcpd/pkg/app"
	"github.com/joeyloman/kubevirt-ipam-dhcpd/pkg/file"
	log "github.com/sirupsen/logrus"

	"kubevirt.io/client-go/kubecli"
)

// https://github.com/kubevirt/client-go/blob/v0.59.0/examples/listvms/list-vms.go

var progname string = "harvester-ipam-dhcpd"

func init() {
	// Log as JSON instead of the default ASCII formatter.
	formatter := &log.TextFormatter{
		FullTimestamp: true,
	}
	log.SetFormatter(formatter)
	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)
}

func main() {
	homedir := os.Getenv("HOME")
	kubeconfig_file := filepath.Join(homedir, ".kube", "config")

	log.Infof("(main) starting %s ..", progname)

	var config *rest.Config = nil
	if file.FileExists(kubeconfig_file) {
		// uses kubeconfig
		kubeconfig := flag.String("kubeconfig", kubeconfig_file, "(optional) absolute path to the kubeconfig file")
		flag.Parse()
		config_kube, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(err.Error())
		}
		config = config_kube
	} else {
		// creates the in-cluster config
		config_rest, err := rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
		config = config_rest
	}

	// create the default k8s clientset
	k8s_clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// create the kubefip clientset
	kubevirt_kubecli, err := kubecli.GetKubevirtClientFromRESTConfig(config)
	if err != nil {
		panic(err.Error())
	}

	app.Run(kubevirt_kubecli, k8s_clientset)
}
