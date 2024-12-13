package vmnetcfg

import (
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

		return
	}

	switch event.action {
	case ADD:
		err := c.updateVirtualMachineNetworkConfig(event.action, obj.(*kihv1.VirtualMachineNetworkConfig))
		if err != nil {
			log.Errorf("(vmnetcfg.sync) failed to update vmnetcfg for %s: %s", obj.(*kihv1.VirtualMachineNetworkConfig).GetName(), err.Error())
			c.metrics.UpdateLogStatus("error")
		}

		// increase the vmnetcfgCountCurrent if the application is still initializing
		// it's important that all processed vmnetcfgs are count, even if they are in error state
		// otherwise the application will not become operational
		if *c.appStatus == APP_INIT {
			*c.vmnetcfgCountCurrent++
		}
	case UPDATE:
		err := c.updateVirtualMachineNetworkConfig(event.action, obj.(*kihv1.VirtualMachineNetworkConfig))
		if err != nil {
			log.Errorf("(vmnetcfg.sync) failed to update vmnetcfg for %s: %s", obj.(*kihv1.VirtualMachineNetworkConfig).GetName(), err.Error())
			c.metrics.UpdateLogStatus("error")
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
