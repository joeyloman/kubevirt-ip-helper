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
		kubeconfig_file,
		kubeconfig_context,
	)

	configHandler.Init()
	if err := configHandler.Run(certRenewalPeriod); err != nil {
		log.Fatalf("(webhook) the initial credential load failed: %s", err.Error())
	}
	admissionHandler.Init()
	serviceHandler.Init()
	scheduler.StartCertRenewalScheduler(ctx, configHandler, serviceHandler, certRenewalPeriod)
	go serviceHandler.Run()
	go Run()

	log.Infof("%s is running", progname)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	received := <-sig
	log.Infof("%s received %v, shutting down", progname, received)

	// stop the renewal scheduling first (F14): no tick may restart the
	// admission server under the shutdown drain, and the cancellation
	// also aborts the api calls of a renewal which is still running
	cancel()

	// drain the admission server with a fresh bounded context (F14):
	// the process context is already canceled, Shutdown stops accepting
	// new requests - the readiness probe withdraws with the closed
	// listener - and waits for the in-flight requests within the drain
	// budget (F08), terminating the ones which outlive it
	if err := serviceHandler.Stop(); err != nil {
		log.Errorf("(webhook) the graceful drain did not complete within its budget: %s", err.Error())
	}

	os.Exit(0)
}

func Run() {
	for {
		time.Sleep(time.Second)
	}
}
