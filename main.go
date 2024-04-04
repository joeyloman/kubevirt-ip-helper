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

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())

	mainApp := app.Register()

	// This is a workaround for a situation when the process gets killed and
	// doesn't cleanup the IP addresses when SIGINT is catched. If another pod
	// will be the new leader then the IP address get's duplicated on the network.
	// The same applies for the LeaderPodLabel.
	mainApp.NetworkCleanup()

	go func() {
		<-sig
		cancel()
		mainApp.RemoveLeaderPodLabel()
		mainApp.NetworkCleanup()
		os.Exit(1)
	}()

	mainApp.Init()
	mainApp.Run(ctx)
	cancel()
}
