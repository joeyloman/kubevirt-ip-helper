package app

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	v1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/ippool"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vm"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vmnetcfg"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
)

const (
	APP_INIT    = 0
	APP_RUNNING = 1
	APP_RESTART = 2
)

type handler struct {
	ctx                  context.Context
	kubeConfigFile       string
	kubeContext          string
	namespace            string
	ipam                 *ipam.IPAllocator
	dhcp                 *dhcp.DHCPAllocator
	cache                *cache.CacheAllocator
	metrics              *metrics.MetricsAllocator
	ippoolEventHandler   *ippool.EventHandler
	vmnetcfgEventHandler *vmnetcfg.EventHandler
	vmEventHandler       *vm.EventHandler
	// appStatus and the startup counters are allocated per service era in
	// RunServices: the previous generation keeps its own atomics through the
	// pointers passed into its handlers, so zombie workers of an old era can
	// never write into the gate or status of the new era
	appStatus            *atomic.Int32
	listenerWg           *sync.WaitGroup
	ippoolCountTarget    int
	ippoolCountCurrent   *atomic.Int32
	vmnetcfgCountTarget  int
	vmnetcfgCountCurrent *atomic.Int32
	lock                 *resourcelock.LeaseLock
	leaderId             string
}

func Register() *handler {
	h := &handler{}

	// the shared era state must be usable on a freshly registered handler
	h.appStatus = new(atomic.Int32)
	h.appStatus.Store(APP_INIT)
	h.listenerWg = &sync.WaitGroup{}

	return h
}
func (h *handler) getKubeConfig() (config *rest.Config, err error) {
	if !util.FileExists(h.kubeConfigFile) {
		return rest.InClusterConfig()
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: h.kubeConfigFile},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: h.kubeContext},
	).ClientConfig()
}

func (h *handler) Init() {
	h.kubeConfigFile = os.Getenv("KUBECONFIG")
	if h.kubeConfigFile == "" {
		homedir := os.Getenv("HOME")
		h.kubeConfigFile = filepath.Join(homedir, ".kube", "config")
	}

	h.kubeContext = os.Getenv("KUBECONTEXT")

	ns, nsErr := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if nsErr != nil {
		log.Errorf("(app.Run) cannot determine current namespace (using the default): %s", nsErr.Error())

		h.namespace = "kubevirt-ip-helper"
	} else {
		h.namespace = strings.TrimSpace(string(ns))
	}

	// make sure the leader label is removed in case the pod crashed
	h.RemoveLeaderPodLabel()

	h.appStatus = new(atomic.Int32)
	h.appStatus.Store(APP_INIT)
	h.listenerWg = &sync.WaitGroup{}

	config, err := h.getKubeConfig()
	if err != nil {
		handleErr(err)
	}

	k8s_clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		handleErr(err)
	}

	h.leaderId = uuid.NewString()
	log.Infof("(app.Run) generated leader id: %s", h.leaderId)

	h.lock = &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "kubevirt-ip-helper-lock",
			Namespace: h.namespace,
		},
		Client: k8s_clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: h.leaderId,
		},
	}
}

func (h *handler) Run(mainCtx context.Context) {
	// create a new context for this, otherwise it will be cancelled during pool updates (this need to be the same as the main context)
	leaderelection.RunOrDie(mainCtx, leaderelection.LeaderElectionConfig{
		Lock:            h.lock,
		ReleaseOnCancel: true,
		LeaseDuration:   60 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(mainCtx context.Context) {
				var ctx context.Context
				var cancel context.CancelFunc

				ctx, cancel = context.WithCancel(context.Background())

				h.RunServices(ctx)
				h.appStatus.Store(APP_RUNNING)

				// keep the main thread alive
				for {
					time.Sleep(time.Second)
					if h.appStatus.Load() == APP_RESTART {
						cancel()

						// join the previous controller era before touching shared host
						// state or starting the new era: the old informers, controller
						// workers and gate writers must be gone, so they cannot
						// re-register NIC IPs or DHCP listeners behind the new era's back
						h.listenerWg.Wait()
						h.RemoveLeaderPodLabel()
						h.metrics.Stop()
						h.stopDHCPListeners()
						h.NetworkCleanup()

						time.Sleep(time.Second * 10)

						// the new per-era appStatus (APP_INIT) and startup counters are
						// allocated by RunServices itself
						ctx, cancel = context.WithCancel(context.Background())
						h.RunServices(ctx)
						h.appStatus.Store(APP_RUNNING)
					}
				}
			},
			OnStoppedLeading: func() {
				log.Infof("(app.Run) leader lost: %s", h.leaderId)
				h.RemoveLeaderPodLabel()
				h.NetworkCleanup()

				// run to the end of RunOrDie and let main return, so a
				// graceful shutdown exits with status 0 instead of being
				// surfaced as a crash by the kubelet
			},
			OnNewLeader: func(identity string) {
				if identity == h.leaderId {
					return
				}
				log.Infof("(app.Run) new leader elected: %s", identity)
			},
		},
	})
}

// initGateOpen reports whether the startup gate has counted enough objects
// to proceed: the comparison tolerates an overshoot (an object created after
// the startup snapshot counts too), so the gate opens when no object is
// still waiting instead of requiring an exact match.
func initGateOpen(current int, target int) bool {
	return current >= target
}

func (h *handler) RunServices(ctx context.Context) {
	var logStartupStateCheck int

	// TODO: follow best practice by removing the ctx from the struct
	// register the new context
	h.ctx = ctx

	// allocate the shared state of this service era: handlers of a previous
	// era keep the pointers they were constructed with, so zombie writes can
	// no longer reach the startup gate or the status seen by the main loop
	h.appStatus = new(atomic.Int32)
	h.appStatus.Store(APP_INIT)
	h.ippoolCountCurrent = new(atomic.Int32)
	h.vmnetcfgCountCurrent = new(atomic.Int32)

	// initialize the ipam service
	h.ipam = ipam.New()

	// initialize the dhcp service
	h.dhcp = dhcp.New()

	// initialize the metrics service
	h.metrics = metrics.New()
	go h.metrics.Run()

	// add the kubevirtiphelper/leader pod label
	h.addLeaderPodLabel()

	// initialize the pool cache
	h.cache = cache.New()

	// gather the ippool count so we know how many pools we should initialize during startup before initializing the next controller
	IPPoolList, err := retryList(h.ctx, h.metrics, "the IPPoolList", func() ([]v1.IPPool, error) { return h.getIPPools() })
	if err != nil {
		log.Errorf("(app.RunServices) giving up on %s: %s", "the IPPoolList", err.Error())
		h.metrics.UpdateLogStatus("error")

		return
	}
	h.ippoolCountTarget = len(IPPoolList)
	h.ippoolCountCurrent.Store(0)

	// initialize the ippoolEventListener handler
	h.ippoolEventHandler = ippool.NewEventHandler(
		h.ctx,
		h.ipam,
		h.dhcp,
		h.metrics,
		h.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
		h.appStatus,
		h.ippoolCountCurrent,
	)
	if err := h.ippoolEventHandler.Init(); err != nil {
		handleErr(err)
	}
	h.listenerWg.Add(1)
	go func() {
		defer h.listenerWg.Done()
		h.ippoolEventHandler.EventListener()
	}()

	// wait for the ippool handler to gather all the pools before proceeding the vmnetcfg controller
	// this prevents race conditions
	logStartupStateCheck = 0
	for {
		if !initGateOpen(int(h.ippoolCountCurrent.Load()), h.ippoolCountTarget) {
			time.Sleep(time.Second * 5)

			if logStartupStateCheck == 12 {
				log.Warnf("app.RunServices) still waiting for IPPool initialization [%d out of %d] after 1 min.", h.ippoolCountCurrent.Load(), h.ippoolCountTarget)
				logStartupStateCheck++
				h.metrics.UpdateLogStatus("warning")
			} else if logStartupStateCheck == 24 {
				log.Errorf("app.RunServices) DHCP services are still NOT running [%d out of %d]! There might be something wrong with one of the IPPools!"+
					" Check above logs for errors and fix them. Then restart the application!",
					h.ippoolCountCurrent.Load(), h.ippoolCountTarget)
				// log again in 65 secs
				logStartupStateCheck = 13
				h.metrics.UpdateLogStatus("error")
			} else {
				logStartupStateCheck++
			}
		} else {
			log.Infof("(app.RunServices) all DHCP services are started, proceeding with the vmnetcfg controller startup")

			break
		}
	}

	// gather the vmnetcfg count so we know how many network configs we should initialize during startup before initializing the next controller
	vmnetcfgList, err := retryList(h.ctx, h.metrics, "the VirtualMachineNetworkConfig list", func() ([]v1.VirtualMachineNetworkConfig, error) { return h.getVmNetCfgs() })
	if err != nil {
		log.Errorf("(app.RunServices) giving up on %s: %s", "the VirtualMachineNetworkConfig list", err.Error())
		h.metrics.UpdateLogStatus("error")

		return
	}
	h.vmnetcfgCountTarget = len(vmnetcfgList)
	h.vmnetcfgCountCurrent.Store(0)

	// initialize the vmnetcfgEventListener handler
	h.vmnetcfgEventHandler = vmnetcfg.NewEventHandler(
		h.ctx,
		h.ipam,
		h.dhcp,
		h.metrics,
		h.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
		h.appStatus,
		h.vmnetcfgCountCurrent,
	)
	if err := h.vmnetcfgEventHandler.Init(); err != nil {
		handleErr(err)
	}
	h.listenerWg.Add(1)
	go func() {
		defer h.listenerWg.Done()
		h.vmnetcfgEventHandler.EventListener()
	}()

	// wait for the vmnetcfg handler to gather all the network configs before proceeding the vm controller
	// this prevents race conditions
	logStartupStateCheck = 0
	for {
		if !initGateOpen(int(h.vmnetcfgCountCurrent.Load()), h.vmnetcfgCountTarget) {
			time.Sleep(time.Second * 10)

			if logStartupStateCheck == 30 {
				log.Warnf("app.RunServices) still waiting for VirtualMachineNetworkConfiguration initialization [%d out of %d] after 5 mins.", h.vmnetcfgCountCurrent.Load(), h.vmnetcfgCountTarget)
				logStartupStateCheck++
				h.metrics.UpdateLogStatus("warning")
			} else if logStartupStateCheck == 60 {
				log.Warnf("app.RunServices) still waiting for VirtualMachineNetworkConfiguration initialization [%d out of %d] after 10 mins.", h.vmnetcfgCountCurrent.Load(), h.vmnetcfgCountTarget)
				logStartupStateCheck++
				h.metrics.UpdateLogStatus("warning")
			} else if logStartupStateCheck == 90 {
				log.Errorf("app.RunServices) VirtualMachineNetworkConfiguration initialization is still not complete [%d out of %d] after > 15 mins! There might be something wrong with the VmNetCfgs count!"+
					" Check above logs for errors and fix them. Then restart the application!",
					h.vmnetcfgCountCurrent.Load(), h.vmnetcfgCountTarget)
				// log again in 60 secs
				logStartupStateCheck = 84
				h.metrics.UpdateLogStatus("error")
			} else {
				logStartupStateCheck++
			}
		} else {
			log.Infof("(app.RunServices) all VirtualMachineNetworkConfiguration objects are initialized, proceeding with the vm controller startup")

			break
		}
	}

	// initialize the vmEventListener handler
	h.vmEventHandler = vm.NewEventHandler(
		h.ctx,
		h.ipam,
		h.dhcp,
		h.metrics,
		h.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
		nil,
	)
	if err := h.vmEventHandler.Init(); err != nil {
		handleErr(err)
	}
	h.listenerWg.Add(1)
	go func() {
		defer h.listenerWg.Done()
		h.vmEventHandler.EventListener()
	}()

	// the vm controller is the last service and has no dependencies
	// so no need to wait until it's initialized completely
	// the 1 sec sleep is just to log the next line after the vm controller thread is started
	time.Sleep(time.Second * 1)
	log.Infof("(app.RunServices) all services are successfully initialized and started")
}

// retryList repeatedly gathers a startup snapshot until it succeeds or the
// era context is cancelled: a transient api error must never leave the
// leader running without controllers (the silent early return made the
// whole ip management dead while the pod looked healthy). the first
// attempts run on a short interval to heal quickly, the later ones back off
// to a minute so a sustained outage does not spam the api.
func retryList[T any](ctx context.Context, m *metrics.MetricsAllocator, what string, gather func() (T, error)) (result T, err error) {
	for attempt := 1; ; attempt++ {
		result, err = gather()
		if err == nil {
			return result, nil
		}

		m.UpdateLogStatus("error")

		delay := time.Second * 5
		if attempt >= 5 {
			delay = time.Minute
		}

		if attempt == 10 {
			log.Errorf("(app.RunServices) %s still cannot be gathered after %d attempts; the cluster api may be unreachable: the controllers cannot serve without the startup snapshot, keeping the retry alive", what, attempt)
		}

		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
}

func (h *handler) getIPPools() (IPPools []v1.IPPool, err error) {
	kubeRestConfig, err := h.getKubeConfig()
	if err != nil {
		return IPPools, fmt.Errorf("cannot get kubeRestConfig: %s", err.Error())
	}

	kihClientset, err := kihclientset.NewForConfig(kubeRestConfig)
	if err != nil {
		return IPPools, fmt.Errorf("cannot get kihClientset: %s", err.Error())
	}

	IPPoolList, err := kihClientset.KubevirtiphelperV1().IPPools().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return IPPools, fmt.Errorf("cannot get the IPPoolList: %s", err.Error())
	}

	return IPPoolList.Items, err
}

func (h *handler) getVmNetCfgs() (vmnetcfgs []v1.VirtualMachineNetworkConfig, err error) {
	kubeRestConfig, err := h.getKubeConfig()
	if err != nil {
		return vmnetcfgs, fmt.Errorf("cannot get kubeRestConfig: %s", err.Error())
	}

	kihClientset, err := kihclientset.NewForConfig(kubeRestConfig)
	if err != nil {
		return vmnetcfgs, fmt.Errorf("cannot get kihClientset: %s", err.Error())
	}

	vmnetcfgList, err := kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return vmnetcfgs, fmt.Errorf("cannot get the vmnetcfgList: %s", err.Error())
	}

	return vmnetcfgList.Items, err
}

func (h *handler) NetworkCleanup() {
	IPPoolList, err := h.getIPPools()
	if err != nil {
		log.Errorf("(app.NetworkCleanup) %s", err.Error())

		return
	}

	for _, pool := range IPPoolList {
		// remove the IP address from the bind interface
		ipnet, err := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet)
		if err != nil {
			log.Errorf("(app.NetworkCleanup) error while parsing subnet [%s] during network cleanup for network [%s]: %s",
				pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, err.Error())

			continue
		}
		ip4 := fmt.Sprintf("%s/%d", pool.Spec.IPv4Config.ServerIP, ipnet.Bits())

		log.Debugf("(app.NetworkCleanup) removing the IP4 address [%s] on nic [%s] for network [%s]",
			ip4, pool.Spec.BindInterface, pool.Spec.NetworkName)

		if err := network.RemoveIpFromNic(pool.Spec.BindInterface, ip4); err != nil {
			// this is defined as a debug log because the ip could have been already removed and this will cause an error
			log.Debugf("(app.NetworkCleanup) error while removing IP4 address [%s] from bind interface [%s] for network [%s]: %s",
				ip4, pool.Spec.BindInterface, pool.Spec.NetworkName, err.Error())
		}
	}
}

func (h *handler) stopDHCPListeners() {
	IPPoolList, err := h.getIPPools()
	if err != nil {
		log.Errorf("(app.stopDHCPListeners) %s", err.Error())

		return
	}

	for _, pool := range IPPoolList {
		if err := h.dhcp.Stop(pool.Spec.NetworkName); err != nil {
			// this is defined as a debug log because some listeners could have been already stopped and this will cause an error
			log.Debugf("(app.stopDHCPListeners) error while shutting down DHCP listener running on nic [%s] for network [%s]: %s",
				pool.Spec.BindInterface, pool.Spec.NetworkName, err.Error())
		}
	}
}

// The addLeaderPodLabel and removeLeaderPodLabel funtions are managing the kubevirtiphelper/leader label.
// This label is used by the metrics-service to determine the active leader.
// If the function(s) fail the application should ignore it and still service DHCP requests.
func (h *handler) addLeaderPodLabel() {
	podName, err := os.Hostname()
	if err != nil {
		log.Errorf("(app.addLeaderPodLabel) cannot get current pod name: %s", err.Error())

		return
	}

	kubeRestConfig, err := h.getKubeConfig()
	if err != nil {
		log.Errorf("(app.addLeaderPodLabel) cannot get kubeRestConfig: %s", err.Error())

		return
	}

	k8sClientset, err := kubernetes.NewForConfig(kubeRestConfig)
	if err != nil {
		log.Errorf("(app.addLeaderPodLabel) cannot get kihClientset: %s", err.Error())

		return
	}

	// the pod is mutated by the kubelet and the label callbacks run during
	// the startup phase: apply the label with a retry on resource-version
	// conflicts so the first attempt does not lose the race
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		curPod, err := k8sClientset.CoreV1().Pods(h.namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		newPod := curPod.DeepCopy()
		newLabels := make(map[string]string)
		for k, v := range newPod.Labels {
			newLabels[k] = v
		}
		newLabels["kubevirtiphelper/leader"] = "active"
		newPod.Labels = newLabels

		_, err = k8sClientset.CoreV1().Pods(h.namespace).Update(context.TODO(), newPod, metav1.UpdateOptions{})

		return err
	}); err != nil {
		log.Errorf("(app.addLeaderPodLabel) cannot set the leader pod label: %s", err.Error())

		return
	}
}

func (h *handler) RemoveLeaderPodLabel() {
	podName, err := os.Hostname()
	if err != nil {
		log.Errorf("(app.RemoveLeaderPodLabel) cannot get current pod name: %s", err.Error())

		return
	}

	kubeRestConfig, err := h.getKubeConfig()
	if err != nil {
		log.Errorf("(app.RemoveLeaderPodLabel) cannot get kubeRestConfig: %s", err.Error())

		return
	}

	k8sClientset, err := kubernetes.NewForConfig(kubeRestConfig)
	if err != nil {
		log.Errorf("(app.RemoveLeaderPodLabel) cannot get kihClientset: %s", err.Error())

		return
	}

	// the pod is mutated by the kubelet and the label callbacks run during
	// the startup phase: remove the label with a retry on resource-version
	// conflicts so the first attempt does not lose the race
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		curPod, err := k8sClientset.CoreV1().Pods(h.namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		newPod := curPod.DeepCopy()
		newLabels := make(map[string]string)
		for k, v := range newPod.Labels {
			if k != "kubevirtiphelper/leader" {
				newLabels[k] = v
			}
		}
		newPod.Labels = newLabels

		_, err = k8sClientset.CoreV1().Pods(h.namespace).Update(context.TODO(), newPod, metav1.UpdateOptions{})

		return err
	}); err != nil {
		log.Errorf("(app.RemoveLeaderPodLabel) cannot remove the leader pod label: %s", err.Error())

		return
	}
}

func handleErr(err error) {
	log.Panicf("(app.handleErr) %s", err.Error())
}
