package vmnetcfg

import (
	"errors"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const (
	APP_INIT    = 0
	APP_RUNNING = 1
	APP_RESTART = 2
)

type Controller struct {
	indexer              cache.Indexer
	queue                workqueue.RateLimitingInterface
	informer             cache.Controller
	cache                *kihcache.CacheAllocator
	ipam                 *ipam.IPAllocator
	dhcp                 *dhcp.DHCPAllocator
	metrics              *metrics.MetricsAllocator
	kihClientset         *kihclientset.Clientset
	appStatus            *int
	vmnetcfgCountCurrent *int

	// initAttempted tracks the vmnetcfg objects which the current
	// initialization phase already handled, so an object which cannot
	// complete its sync is still counted by the startup gate
	initAttempted map[string]bool
}

func NewController(
	queue workqueue.RateLimitingInterface,
	indexer cache.Indexer,
	informer cache.Controller,
	cache *kihcache.CacheAllocator,
	ipam *ipam.IPAllocator,
	dhcp *dhcp.DHCPAllocator,
	metrics *metrics.MetricsAllocator,
	kihClientset *kihclientset.Clientset,
	appStatus *int,
	vmnetcfgCountCurrent *int,
) *Controller {
	return &Controller{
		informer:             informer,
		indexer:              indexer,
		queue:                queue,
		cache:                cache,
		ipam:                 ipam,
		dhcp:                 dhcp,
		metrics:              metrics,
		kihClientset:         kihClientset,
		appStatus:            appStatus,
		vmnetcfgCountCurrent: vmnetcfgCountCurrent,
	}
}

// markInitAttempt counts one settled VirtualMachineNetworkConfig object
// for the startup gate: its sync either completed (nics in the ERROR
// status included, their object was processed) or was definitively
// rejected. a transiently failed restore stays uncounted, so the retried
// sync can still protect the existing reservation before the vm
// controller opens new allocations; a definitively broken object still
// counts so it does not block the controller startup until it is removed.
func (c *Controller) markInitAttempt(key string) {
	if *c.appStatus != APP_INIT {
		return
	}

	if c.initAttempted == nil {
		c.initAttempted = make(map[string]bool)
	}

	if _, exists := c.initAttempted[key]; exists {
		return
	}

	c.initAttempted[key] = true

	*c.vmnetcfgCountCurrent++
}

// initSyncSettled reports whether a failed sync can never succeed on a
// retry during the initialization phase: a networkname without a live
// pool registration cannot restore its reservation until the offending
// IPPool is repaired, an invalid macaddress in the spec cannot register a
// lease at all, and an ownership conflict (the pool status records the
// claimed address for another owner) needs one of the claiming objects to
// be edited. the startup gate must not wait for such objects, while every
// other failure is transient and the retried sync must stay able to
// settle the object for the gate.
func (c *Controller) initSyncSettled(vmnetcfg *kihv1.VirtualMachineNetworkConfig, err error) bool {
	if errors.Is(err, util.ErrForeignOwner) {
		return true
	}

	for _, v := range vmnetcfg.Spec.NetworkConfig {
		if _, cacheErr := c.cache.Get("pool", v.NetworkName); cacheErr != nil {
			return true
		}

		if _, macErr := net.ParseMAC(v.MACAddress); macErr != nil {
			return true
		}
	}

	return false
}

func (c *Controller) processNextItem() bool {
	event, quit := c.queue.Get()
	if quit {
		return false
	}

	defer c.queue.Done(event)

	err := c.sync(event.(Event))
	c.handleErr(err, event)

	return true
}

func (c *Controller) sync(event Event) (err error) {
	obj, exists, err := c.indexer.GetByKey(event.key)
	if err != nil {
		log.Errorf("(vmnetcfg.sync) fetching object with key %s from store failed with %v", event.key, err)
		c.metrics.UpdateLogStatus("error")

		return
	}

	if !exists && event.action != DELETE {
		log.Warnf("(vmnetcfg.sync) VirtualMachineNetworkConfig %s does not exist anymore", event.key)
		c.metrics.UpdateLogStatus("warning")
		// the object is gone and cannot produce a sync anymore; the startup
		// gate must not wait for it
		c.markInitAttempt(event.key)

		return
	}

	switch event.action {
	case ADD, UPDATE:
		err = c.updateVirtualMachineNetworkConfig(event.action, obj.(*kihv1.VirtualMachineNetworkConfig))
		if err != nil {
			log.Errorf("(vmnetcfg.sync) failed to update vmnetcfg for %s: %s", event.key, err.Error())
			c.metrics.UpdateLogStatus("error")
		}
		// increase the vmnetcfgCountCurrent if the application is still
		// initializing: the startup gate counts a vmnetcfg as handled once
		// its sync settled. vmnetcfgs with nics in the ERROR status count
		// as well because their sync completed, and a definitively rejected
		// object counts so a broken vmnetcfg does not block the vm
		// controller startup forever. a transiently failed restore stays
		// uncounted instead: the rate-limited retry must stay able to
		// protect the existing reservation before the vm controller opens
		// new allocations
		if event.action == ADD && (err == nil || c.initSyncSettled(obj.(*kihv1.VirtualMachineNetworkConfig), err)) {
			c.markInitAttempt(event.key)
		}
		// case DELETE:
		// 	log.Infof("(vmnetcfg.sync) delete action found!")
	}

	return
}

func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)

		return
	}

	if c.queue.NumRequeues(key) < 5 {
		log.Errorf("(vmnetcfg.handleErr) syncing VirtualMachineNetworkConfig %v: %v", key, err)

		c.queue.AddRateLimited(key)

		return
	}

	c.queue.Forget(key)

	log.Errorf("(vmnetcfg.handleErr) dropping VirtualMachineNetworkConfig %q out of the queue: %v", key, err)
	c.metrics.UpdateLogStatus("error")
}

func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	defer c.queue.ShutDown()
	log.Infof("(vmnetcfg.Run) starting the VirtualMachineNetworkConfig controller")

	go c.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		log.Errorf("(vmnetcfg.Run) timed out waiting for caches to sync")
		c.metrics.UpdateLogStatus("error")

		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Infof("(vmnetcfg.Run) stopping the VirtualMachineNetworkConfig controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
