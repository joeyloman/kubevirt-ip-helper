package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/app"

	log "github.com/sirupsen/logrus"
)

var progname string = "kubevirt-ip-helper"

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
	log.Infof("(main) starting %s", progname)

	level, err := log.ParseLevel(os.Getenv("LOGLEVEL"))
	if err != nil {
		log.Warnf("(main) cannot determine loglevel, leaving it on Info")
	} else {
		log.Infof("(main) setting loglevel to %s", level)
		log.SetLevel(level)
	}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())

	mainApp := app.Register()

	// init before the startup cleanup: the cleanup gathers the pools
	// through the api and needs the loaded kube config (in-cluster runs
	// worked regardless, kubeconfig-based runs silently no-op'd before)
	mainApp.Init()

	// This is a workaround for a situation when the process gets killed and
	// doesn't cleanup the IP addresses when SIGINT is catched. If another pod
	// will be the new leader then the IP address get's duplicated on the network.
	// The same applies for the LeaderPodLabel.
	// This startup variant gathers the pools from the api because a fresh
	// process has no local pool cache yet; the shutdown paths clean up from
	// the local era state instead.
	mainApp.StartupNetworkCleanup()

	// canceling the main context releases the leader lease and runs the
	// OnStoppedLeading cleanup (leader label + network state) exactly once;
	// the explicit cleanup workaround for killed processes stays as the
	// StartupNetworkCleanup call at startup.
	// the graceful shutdown can legitimately take tens of seconds (the era
	// join of in-flight syncs plus bounded api calls, against the 30s
	// termination grace period of the pod): a second signal force-exits so
	// an operator can always interrupt a slow drain instead of waiting for
	// the kubelet's SIGKILL. the channel buffers both signals so the second
	// one is never dropped while the goroutine is busy canceling.
	go func() {
		<-sig
		cancel()
		<-sig
		log.Warnf("(main) second signal received, exiting immediately without waiting for the graceful cleanup")
		os.Exit(1)
	}()

	mainApp.Run(ctx)
	cancel()
}
