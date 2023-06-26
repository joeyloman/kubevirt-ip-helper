package app

import (
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"

	"context"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/ippool"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vm"
)

type EventListeners struct {
	ctx                 context.Context
	vmEventListener     *vm.EventListener
	ippoolEventListener *ippool.EventListener
}

func NewEventListeners(ctx context.Context) *EventListeners {
	log.Infof("(app.NewEventListeners) start")

	return &EventListeners{
		ctx: ctx,
	}
}

func (e *EventListeners) Run(ctx context.Context) {
	log.Infof("(app.Run) start")

	// TODO: flag parse

	// temp
	homedir := os.Getenv("HOME")
	kubeconfig_file := filepath.Join(homedir, ".kube", "config")

	e.vmEventListener = vm.NewEventListener(ctx, kubeconfig_file, "", nil, nil)
	if err := e.vmEventListener.Init(); err != nil {
		handleErr(err)
	}
	go e.vmEventListener.Listener()

	e.ippoolEventListener = ippool.NewEventListener(ctx, kubeconfig_file, "", nil, nil)
	if err := e.ippoolEventListener.Init(); err != nil {
		handleErr(err)
	}
	go e.ippoolEventListener.Listener()

	for {
		time.Sleep(time.Second)
	}
}

func handleErr(err error) {
	log.Errorf("(app.handleErr) %s", err.Error())
}
