package app

import (
	"context"
	"errors"
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
	// leaderWatchdog is the leader-election healthz adaptor of the
	// process: the liveness probe and the force-exit fence both check the
	// freshness of the leader lease through it. it is created in Run and
	// registered on every service era's metrics server
	leaderWatchdog *leaderelection.HealthzAdaptor
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
	// the clients built from this config are used by the leader election
	// lease (including the release call on leadership loss) and by the
	// startup gathers: without a timeout a tcp blackhole against the api
	// hangs those calls forever, which would keep a lost leader running
	// its dhcp servers on the segment (the standby acquires after the
	// lease expires and starts a second one). 30s stays below the renew
	// deadline, so the election loop's own deadlines keep winning
	const configTimeout = 30 * time.Second

	if !util.FileExists(h.kubeConfigFile) {
		if config, err = rest.InClusterConfig(); err != nil {
			return
		}
		config.Timeout = configTimeout

		return
	}

	config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: h.kubeConfigFile},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: h.kubeContext},
	).ClientConfig()
	if err == nil {
		config.Timeout = configTimeout
	}

	return
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
	// the healthz adaptor exposes the leader election state to the
	// liveness probe: it fails only while this client still owns the lease
	// record but could not renew it (the lease went stale), so the kubelet
	// restarts a leader which lost the api instead of leaving its dhcp
	// servers answering on the segment
	h.leaderWatchdog = leaderelection.NewLeaderHealthzAdaptor(10 * time.Second)

	// force-exit fence: the liveness probe and the leader election exit are
	// the primary fences, but a stale leader stops serving dhcp and removes
	// its host state a bounded time after the lease went stale even if both
	// are delayed, then exits so the kubelet restarts the pod
	go h.leaderWatchdogLoop(h.leaderWatchdog)

	leaderelection.RunOrDie(mainCtx, leaderelection.LeaderElectionConfig{
		Lock:            h.lock,
		ReleaseOnCancel: true,
		LeaseDuration:   60 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		WatchDog:        h.leaderWatchdog,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				h.onStartedLeading(ctx)
			},
			OnStoppedLeading: func() {
				h.onStoppedLeading()
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

// onStartedLeading runs the service era of the leadership. the era context
// is derived from the leader-election context: when the lease is lost,
// client-go cancels it before the OnStoppedLeading callback runs, so the
// controllers drain first and the host state is cleaned after the era
// joined.
func (h *handler) onStartedLeading(ctx context.Context) {
	eraCtx, eraCancel := context.WithCancel(ctx)

	if err := h.RunServices(eraCtx); err != nil {
		log.Errorf("(app.Run) services failed to start: %s", err)
		h.drainStoppedEra(eraCancel)

		return
	}
	h.appStatus.Store(APP_RUNNING)

	for {
		select {
		case <-ctx.Done():
			// leadership lost: drain the era; onStoppedLeading joins it and
			// cleans the host state before the process exits
			eraCancel()
			h.listenerWg.Wait()

			return
		case <-time.After(time.Second):
		}

		if h.appStatus.Load() == APP_RESTART {
			eraCancel()

			// join the previous controller era before touching shared host
			// state or starting the new era: the old informers, controller
			// workers and gate writers must be gone, so they cannot
			// re-register NIC IPs or DHCP listeners behind the new era's back
			h.listenerWg.Wait()
			h.RemoveLeaderPodLabel()
			if h.metrics != nil {
				h.metrics.Stop()
			}
			h.stopDHCPListeners()
			h.NetworkCleanup()

			time.Sleep(time.Second * 10)

			// the new per-era appStatus (APP_INIT) and startup counters are
			// allocated by RunServices itself
			eraCtx, eraCancel = context.WithCancel(ctx)
			if err := h.RunServices(eraCtx); err != nil {
				log.Errorf("(app.Run) services failed to restart: %s", err)
				h.drainStoppedEra(eraCancel)

				return
			}
			h.appStatus.Store(APP_RUNNING)
		}
	}
}

// drainStoppedEra joins the era and tears the host state down after a
// failed startup, then exits with a non-zero status so the kubelet
// restarts the pod: a leader whose services could not start must not hold
// the lease while serving nothing and looking healthy.
func (h *handler) drainStoppedEra(eraCancel context.CancelFunc) {
	eraCancel()
	h.listenerWg.Wait()
	if h.metrics != nil {
		h.metrics.Stop()
	}
	h.stopDHCPListeners()
	h.RemoveLeaderPodLabel()
	h.NetworkCleanup()

	os.Exit(1)
}

// onStoppedLeading joins the era and cleans the host state after the
// leadership was lost. client-go cancels the leader-election context
// before this callback runs, so the era's informers/controllers are
// already draining: the listeners and the nic addresses are removed only
// after the era joined, so the standby can serve the segment without a
// second server answering in the meantime. the exit status stays 0 for a
// graceful shutdown; the lease-loss is surfaced through the error metric
// and the error-level log.
func (h *handler) onStoppedLeading() {
	log.Errorf("(app.Run) leader lost: %s", h.leaderId)
	if h.metrics != nil {
		h.metrics.UpdateLogStatus("error")
	}

	h.listenerWg.Wait()
	h.stopDHCPListeners()
	h.RemoveLeaderPodLabel()
	h.NetworkCleanup()
}

// leaderWatchdogLoop force-exits a stale leader: a leader which lost the
// api cannot renew its lease, and the standby only acquires once the lease
// expired - the stale leader must stop serving dhcp before that, or two
// servers answer on the same segment with diverging allocators. the check
// fails only while this client still owns the lease record but could not
// renew it (a follower and a healthy leader never fail), and the exit is
// delayed across consecutive failures so a transient api blip does not
// kill a healthy leader.
func (h *handler) leaderWatchdogLoop(adaptor *leaderelection.HealthzAdaptor) {
	var staleCount int

	for {
		time.Sleep(10 * time.Second)

		if adaptor.Check(nil) != nil {
			staleCount++
			if staleCount >= 3 {
				log.Errorf("(app.Run) the leadership lease of %s could not be renewed for too long, stopping the DHCP services and host state and exiting so the kubelet restarts this pod", h.leaderId)
				h.stopDHCPListeners()
				h.NetworkCleanup()
				h.RemoveLeaderPodLabel()

				os.Exit(1)
			}

			continue
		}

		staleCount = 0
	}
}

// initGateOpen reports whether the startup gate has counted enough objects
// to proceed: the comparison tolerates an overshoot (an object created after
// the startup snapshot counts too), so the gate opens when no object is
// still waiting instead of requiring an exact match.
func initGateOpen(current int, target int) bool {
	return current >= target
}

func (h *handler) RunServices(ctx context.Context) error {
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

	// the liveness probe of this pod checks the leader-election freshness
	// and the process state through this server: register the checks on
	// every era, the server is created per era
	if h.leaderWatchdog != nil {
		h.metrics.SetHealthCheck("leaderElection", func() error {
			return h.leaderWatchdog.Check(nil)
		})
	}
	h.metrics.SetHealthCheck("process", func() error { return nil })
	h.metrics.SetReadinessCheck("services", func() error {
		if h.appStatus.Load() != APP_RUNNING {
			return errors.New("application is not running its services")
		}

		return nil
	})

	go h.metrics.Run()

	// add the kubevirtiphelper/leader pod label
	h.addLeaderPodLabel()

	// initialize the pool cache
	h.cache = cache.New()

	// gather the ippool count so we know how many pools we should initialize during startup before initializing the next controller
	IPPoolList, err := retryList(ctx, h.metrics, "the IPPoolList", func(attemptCtx context.Context) ([]v1.IPPool, error) {
		return h.getIPPools(attemptCtx)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// the era was canceled (restart or leadership loss): drain
			// gracefully, the caller joins the era
			return nil
		}

		log.Errorf("(app.RunServices) giving up on %s: %s", "the IPPoolList", err.Error())
		h.metrics.UpdateLogStatus("error")

		return fmt.Errorf("cannot gather the IPPoolList: %s", err.Error())
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
	if err := h.waitForStartupGate(ctx, "IPPool", h.ippoolCountCurrent, h.ippoolCountTarget, 5*time.Second,
		func(tick int, count int) {
			switch {
			case tick == 12:
				log.Warnf("app.RunServices) still waiting for IPPool initialization [%d out of %d] after 1 min.", count, h.ippoolCountTarget)
				h.metrics.UpdateLogStatus("warning")
			case tick == 24:
				log.Errorf("app.RunServices) DHCP services are still NOT running [%d out of %d]! There might be something wrong with one of the IPPools!"+
					" Check above logs for errors and fix them. The startup gives up when the count stops progressing.", count, h.ippoolCountTarget)
				h.metrics.UpdateLogStatus("error")
			}
		},
	); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}

		return err
	}
	log.Infof("(app.RunServices) all DHCP services are started, proceeding with the vmnetcfg controller startup")

	// gather the vmnetcfg count so we know how many network configs we should initialize during startup before initializing the next controller
	vmnetcfgList, err := retryList(ctx, h.metrics, "the VirtualMachineNetworkConfig list", func(attemptCtx context.Context) ([]v1.VirtualMachineNetworkConfig, error) {
		return h.getVmNetCfgs(attemptCtx)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}

		log.Errorf("(app.RunServices) giving up on %s: %s", "the VirtualMachineNetworkConfig list", err.Error())
		h.metrics.UpdateLogStatus("error")

		return fmt.Errorf("cannot gather the VirtualMachineNetworkConfig list: %s", err.Error())
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
	if err := h.waitForStartupGate(ctx, "VirtualMachineNetworkConfiguration", h.vmnetcfgCountCurrent, h.vmnetcfgCountTarget, 10*time.Second,
		func(tick int, count int) {
			switch {
			case tick == 30:
				log.Warnf("app.RunServices) still waiting for VirtualMachineNetworkConfiguration initialization [%d out of %d] after 5 mins.", count, h.vmnetcfgCountTarget)
				h.metrics.UpdateLogStatus("warning")
			case tick == 60:
				log.Warnf("app.RunServices) still waiting for VirtualMachineNetworkConfiguration initialization [%d out of %d] after 10 mins.", count, h.vmnetcfgCountTarget)
				h.metrics.UpdateLogStatus("warning")
			case tick == 90:
				log.Errorf("app.RunServices) VirtualMachineNetworkConfiguration initialization is still not complete [%d out of %d] after > 15 mins! There might be something wrong with the VmNetCfgs count!"+
					" Check above logs for errors and fix them. The startup gives up when the count stops progressing.", count, h.vmnetcfgCountTarget)
				h.metrics.UpdateLogStatus("error")
			}
		},
	); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}

		return err
	}
	log.Infof("(app.RunServices) all VirtualMachineNetworkConfiguration objects are initialized, proceeding with the vm controller startup")

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

	return nil
}

// startupStallTimeout bounds the startup gate: when the counted objects
// stop advancing for this long, no retry will heal the startup (a wedged
// informer, an object which never settles), so the gate fails and the pod
// restarts instead of sitting on the leader lease while serving nothing.
const startupStallTimeout = 15 * time.Minute

// waitForStartupGate blocks until the counted objects of a startup
// snapshot settle (current >= target) or the era context is canceled, and
// fails the startup when the count stops advancing for startupStallTimeout.
// the reporting callback is invoked once per tick so the caller keeps its
// escalating progress logs.
func (h *handler) waitForStartupGate(ctx context.Context, what string, current *atomic.Int32, target int, tick time.Duration, report func(tick int, count int)) error {
	var lastCount int32 = -1
	var stalledSince time.Time
	tickCount := 0

	for {
		count := current.Load()
		if initGateOpen(int(count), target) {
			return nil
		}

		if count != lastCount {
			lastCount = count
			stalledSince = time.Now()
		}
		if !stalledSince.IsZero() && time.Since(stalledSince) > startupStallTimeout {
			log.Errorf("app.RunServices) %s initialization has not progressed for %s [%d out of %d], giving up so the pod restarts",
				what, startupStallTimeout, count, target)
			h.metrics.UpdateLogStatus("error")

			return fmt.Errorf("%s initialization has not progressed for %s (%d/%d objects)", what, startupStallTimeout, count, target)
		}

		tickCount++
		if report != nil {
			report(tickCount, int(count))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tick):
		}
	}
}

// retryList repeatedly gathers a startup snapshot until it succeeds or the
// era context is cancelled: a transient api error must never leave the
// leader running without controllers (the silent early return made the
// whole ip management dead while the pod looked healthy). the first
// attempts run on a short interval to heal quickly, the later ones back off
// to a minute so a sustained outage does not spam the api. every attempt is
// additionally bounded: a gather which hangs on a tcp blackhole must not
// block the era join (the parent context only aborts between attempts).
func retryList[T any](ctx context.Context, m *metrics.MetricsAllocator, what string, gather func(ctx context.Context) (T, error)) (result T, err error) {
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, err = gather(attemptCtx)
		cancel()

		if err == nil {
			return result, nil
		}

		if ctx.Err() != nil {
			// the era was canceled (restart or leadership loss): drain
			// gracefully instead of retrying against a dead context
			return result, ctx.Err()
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
			return result, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (h *handler) getIPPools(ctx context.Context) (IPPools []v1.IPPool, err error) {
	kubeRestConfig, err := h.getKubeConfig()
	if err != nil {
		return IPPools, fmt.Errorf("cannot get kubeRestConfig: %s", err.Error())
	}

	kihClientset, err := kihclientset.NewForConfig(kubeRestConfig)
	if err != nil {
		return IPPools, fmt.Errorf("cannot get kihClientset: %s", err.Error())
	}

	IPPoolList, err := kihClientset.KubevirtiphelperV1().IPPools().List(ctx, metav1.ListOptions{})
	if err != nil {
		return IPPools, fmt.Errorf("cannot get the IPPoolList: %s", err.Error())
	}

	return IPPoolList.Items, err
}

func (h *handler) getVmNetCfgs(ctx context.Context) (vmnetcfgs []v1.VirtualMachineNetworkConfig, err error) {
	kubeRestConfig, err := h.getKubeConfig()
	if err != nil {
		return vmnetcfgs, fmt.Errorf("cannot get kubeRestConfig: %s", err.Error())
	}

	kihClientset, err := kihclientset.NewForConfig(kubeRestConfig)
	if err != nil {
		return vmnetcfgs, fmt.Errorf("cannot get kihClientset: %s", err.Error())
	}

	vmnetcfgList, err := kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return vmnetcfgs, fmt.Errorf("cannot get the vmnetcfgList: %s", err.Error())
	}

	return vmnetcfgList.Items, err
}

func (h *handler) NetworkCleanup() {
	// bound the gather: the cleanup runs on the shutdown paths where a hang
	// must not block the exit
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	IPPoolList, err := h.getIPPools(ctx)
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
	if h.dhcp == nil {
		// this pod never ran services (it never acquired the leadership):
		// there are no listeners of its own to stop
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	IPPoolList, err := h.getIPPools(ctx)
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

	// bound the api calls: a hang must never block the startup phase
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// the pod is mutated by the kubelet and the label callbacks run during
	// the startup phase: apply the label with a retry on resource-version
	// conflicts so the first attempt does not lose the race
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		curPod, err := k8sClientset.CoreV1().Pods(h.namespace).Get(ctx, podName, metav1.GetOptions{})
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

		_, err = k8sClientset.CoreV1().Pods(h.namespace).Update(ctx, newPod, metav1.UpdateOptions{})

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

	// bound the api calls: a hang must never block the shutdown paths
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// the pod is mutated by the kubelet and the label callbacks run during
	// the startup phase: remove the label with a retry on resource-version
	// conflicts so the first attempt does not lose the race
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		curPod, err := k8sClientset.CoreV1().Pods(h.namespace).Get(ctx, podName, metav1.GetOptions{})
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

		_, err = k8sClientset.CoreV1().Pods(h.namespace).Update(ctx, newPod, metav1.UpdateOptions{})

		return err
	}); err != nil {
		log.Errorf("(app.RemoveLeaderPodLabel) cannot remove the leader pod label: %s", err.Error())

		return
	}
}

func handleErr(err error) {
	log.Panicf("(app.handleErr) %s", err.Error())
}
