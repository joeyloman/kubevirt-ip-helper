package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/webhook/admission"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/webhook/config"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/webhook/scheduler"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/webhook/service"
	log "github.com/sirupsen/logrus"
)

var progname string = "kubevirt-ip-helper-webhook"

var certRenewalPeriod int64

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
	var kubeconfig_file string

	level, err := log.ParseLevel(os.Getenv("LOGLEVEL"))
	if err == nil {
		log.SetLevel(level)
	}

	certRenewalPeriod, err := strconv.ParseInt(os.Getenv("CERTRENEWALPERIOD"), 10, 64)
	if err != nil || certRenewalPeriod == 0 {
		// default the cert renewal expire interval to 30 days
		certRenewalPeriod = 30 * 24 * 60
	}

	kubeconfig_file = os.Getenv("KUBECONFIG")
	if kubeconfig_file == "" {
		homedir := os.Getenv("HOME")
		kubeconfig_file = filepath.Join(homedir, ".kube", "config")
	}

	kubeconfig_context := os.Getenv("KUBECONTEXT")

	ctx, cancel := context.WithCancel(context.Background())

	configHandler := config.Register(
		ctx,
		kubeconfig_file,
		kubeconfig_context,
		"kubevirt-ip-helper-webhook",
		"kubevirt-ip-helper",
	)

	admissionHandler := admission.Register(
		ctx,
		kubeconfig_file,
		kubeconfig_context,
		"kubevirt-ip-helper-webhook",
		"kubevirt-ip-helper",
		"kubevirt-ip-helper-validator",
	)

	serviceHandler := service.Register(
		ctx,
	)

	configHandler.Init()
	configHandler.Run(certRenewalPeriod)
	admissionHandler.Init()
	scheduler.StartCertRenewalScheduler(configHandler, serviceHandler, certRenewalPeriod)
	go serviceHandler.Run()
	go Run()

	log.Infof("%s is running", progname)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	cancel()
	os.Exit(1)
}

func Run() {
	for {
		time.Sleep(time.Second)
	}
}
