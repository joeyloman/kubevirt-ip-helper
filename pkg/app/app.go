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
	"github.com/joeyloman/kubevirt-ip-helper/pkg/gate"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
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
	kubeConfigFile       string
	kubeContext          string
	namespace            string
	networkScope         util.NetworkScope
	metrics              *metrics.MetricsAllocator
	ippoolEventHandler   *ippool.EventHandler
	vmnetcfgEventHandler *vmnetcfg.EventHandler
	vmEventHandler       *vm.EventHandler
	// listenerWg joins the listener goroutines of the current service era
	// on its shutdown paths: it is allocated once before the leader
	// election starts and never reassigned, so Wait is always safe
	listenerWg *sync.WaitGroup
	lock       *resourcelock.LeaseLock
	leaderId   string
	// led records whether this process ever acquired the leadership
	// lease: client-go fires OnStoppedLeading even when the election
	// never succeeded, so the shutdown paths distinguish a real lease
	// loss (error-level) from the routine shutdown of a standby
	led atomic.Bool
	// leaderWatchdog is the leader-election healthz adaptor of the
	// process: the liveness probe and the force-exit fence both check the
	// freshness of the leader lease through it. it is created in Run and
	// registered on the process-global metrics server
	leaderWatchdog *leaderelection.HealthzAdaptor
	// era holds the shared state of the current service era: RunServices
	// builds it fully and publishes it atomically, so the shutdown paths on
	// other goroutines (OnStoppedLeading, the force-exit fence, the startup
	// drain) always read a consistent snapshot of the serving state
	era atomic.Pointer[eraState]
}

// eraState bundles the shared state of one service era. the event handlers
// of the era keep the pointers they were constructed with, so zombie
// workers of a dying era can never write into the startup gate or the
// status of the next era.
type eraState struct {
	appStatus    *atomic.Int32
	ippoolGate   *gate.Gate
	vmnetcfgGate *gate.Gate
	ipam         *ipam.IPAllocator
	dhcp         *dhcp.DHCPAllocator
	cache        *cache.CacheAllocator
	reconcileMu  *sync.Mutex
}

func Register() *handler {
	return &handler{listenerWg: &sync.WaitGroup{}}
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
	h.init("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
}

// init keeps the namespace-file boundary explicit for isolated startup tests.
func (h *handler) init(namespaceFile string) {
	h.kubeConfigFile = os.Getenv("KUBECONFIG")
	if h.kubeConfigFile == "" {
		homedir := os.Getenv("HOME")
		h.kubeConfigFile = filepath.Join(homedir, ".kube", "config")
	}

	h.kubeContext = os.Getenv("KUBECONTEXT")

	ns, nsErr := os.ReadFile(namespaceFile)
	if nsErr != nil {
		log.Errorf("(app.Init) cannot determine current namespace (using the default): %s", nsErr.Error())
		h.namespace = "kubevirt-ip-helper"
	} else {
		h.namespace = strings.TrimSpace(string(ns))
	}
	podName, err := os.Hostname()
	if err != nil || podName == "" {
		handleErr(fmt.Errorf("cannot determine current pod name: %v", err))
	}
	// Reject an invalid namespace before constructing an API path. The actual
	// network name is read only from the own Pod, never from a fallback.
	if problems := validation.IsDNS1123Label(h.namespace); len(problems) != 0 {
		handleErr(fmt.Errorf("invalid own Pod namespace %q: %s", h.namespace, strings.Join(problems, "; ")))
	}

	config, err := h.getKubeConfig()
	if err != nil {
		handleErr(err)
	}

	k8s_clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		handleErr(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pod, err := k8s_clientset.CoreV1().Pods(h.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		handleErr(fmt.Errorf("cannot read own Pod for network identity: %w", err))
	}
	h.networkScope, err = util.NewNetworkScope(h.namespace, pod.Labels[util.NetworkLabel])
	if err != nil {
		handleErr(fmt.Errorf("invalid own Pod network identity: %w", err))
	}

	// Only a validated identity may mutate the Pod or enter network cleanup
	// and election. A live label edit never changes this process's scope.
	h.RemoveLeaderPodLabel()

	h.leaderId = uuid.NewString()
	log.Infof("(app.Run) generated leader id: %s", h.leaderId)

	h.lock = &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      h.networkScope.LeaseName(),
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

	// the metrics and health endpoints are process-global: a standby which
	// never acquires the leadership must still answer the liveness probe of
	// its pod (the server used to start per service era, so the kubelet
	// killed every standby shortly after it started)
	h.metrics = metrics.New()
	h.registerHealthChecks()
	go h.metrics.Run()

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

// registerHealthChecks wires the process-global health endpoints: the
// leaderElection check passes for a non-leader (client-go reports
// unhealthy only for a lease owner which cannot renew), so a standby stays
// live, and the services check reports a pod whose service era is being
// built or torn down as not-ready without failing its liveness probe.
//
// the readiness check deliberately does not gate on the leadership: the
// replica count of the deployment is a number of *pods*, and a
// leadership-gated readiness keeps every non-leader replica not-ready
// forever, so the deployment can never reach its desired availability -
// availableReplicas stays below replicas, the rollout reports
// ProgressDeadlineExceeded at the progress deadline and every release
// (kubectl rollout status, helm --wait, a gitops health gate) is reported
// as failed although the leader serves. selecting the serving pod is the
// job of the metrics service, which routes to the leader through the pod
// label, not of the readiness endpoint. a standby which never ran a
// service era is therefore a healthy, ready member of the deployment, and
// only a pod which is mid-rebuild (APP_INIT) or mid-teardown (APP_RESTART)
// reports not-ready.
func (h *handler) registerHealthChecks() {
	h.metrics.SetHealthCheck("leaderElection", func() error {
		return h.leaderWatchdog.Check(nil)
	})
	h.metrics.SetHealthCheck("process", func() error { return nil })
	h.metrics.SetReadinessCheck("services", func() error {
		era := h.era.Load()
		if era == nil {
			// this process never acquired the leadership: a healthy standby
			return nil
		}

		if era.appStatus.Load() != APP_RUNNING {
			return errors.New("application is not running its services")
		}

		return nil
	})
}

// onStartedLeading runs the service era of the leadership. the era context
// is derived from the leader-election context: when the lease is lost,
// client-go cancels it before the OnStoppedLeading callback runs, so the
// controllers drain first and the host state is cleaned after the era
// joined.
func (h *handler) onStartedLeading(ctx context.Context) {
	// this callback only runs after the lease was acquired: the flag
	// separates its shutdown (a real lease loss, error-level) from the
	// stopped-leading callback of a standby which never led
	h.led.Store(true)

	eraCtx, eraCancel := context.WithCancel(ctx)

	if err := h.RunServices(eraCtx); err != nil {
		log.Errorf("(app.Run) services failed to start: %s", err)
		h.drainStoppedEra(eraCancel)

		return
	}
	// the leadership may have been lost while the services started
	// (RunServices returns nil on a canceled era): a dead era must not be
	// published as running - the readiness would briefly serve a pod
	// which leads nothing, and onStoppedLeading owns the cleanup
	if ctx.Err() != nil {
		eraCancel()

		return
	}
	h.era.Load().appStatus.Store(APP_RUNNING)

	for {
		select {
		case <-ctx.Done():
			// leadership lost: client-go releases the lease as soon as
			// renew returns, so the standby can acquire and open its own
			// listeners while this process still drains. the dhcp
			// listeners are therefore stopped and the allocator closed
			// BEFORE the era join: the closed allocator fences a draining
			// worker which would otherwise re-open a listener behind the
			// teardown (the ippool controller's repair path re-serves a
			// pool whose listener died). onStoppedLeading joins the
			// drained era and finishes the host-state cleanup.
			eraCancel()
			h.stopDHCPListeners()
			h.listenerWg.Wait()

			return
		case <-time.After(time.Second):
		}
		era := h.era.Load()
		if era != nil && era.appStatus.Load() == APP_RESTART {
			eraCancel()

			// stop the listeners and close the allocator before the era
			// join: a draining worker must not re-open a listener the
			// restart teardown just closed
			h.stopDHCPListeners()

			// join the previous controller era before touching shared host
			// state or starting the new era: the old informers, controller
			// workers and gate writers must be gone, so they cannot
			// re-register NIC IPs or DHCP listeners behind the new era's back
			h.listenerWg.Wait()
			h.RemoveLeaderPodLabel()
			h.NetworkCleanup()

			// the restart backoff is interruptible: the leadership may be
			// lost while it runs, and restarting on a canceled era would
			// publish a dead era and re-add the leader label after the
			// shutdown path removed it. onStoppedLeading completes the
			// cleanup for this path
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second * 10):
			}

			// the new per-era appStatus (APP_INIT) and startup counters are
			// allocated by RunServices itself
			eraCtx, eraCancel = context.WithCancel(ctx)
			if err := h.RunServices(eraCtx); err != nil {
				log.Errorf("(app.Run) services failed to restart: %s", err)
				h.drainStoppedEra(eraCancel)

				return
			}
			if ctx.Err() != nil {
				eraCancel()

				return
			}
			h.era.Load().appStatus.Store(APP_RUNNING)
		}
	}
}

// drainStoppedEra joins the era and tears the host state down after a
// failed startup, then exits with a non-zero status so the kubelet
// restarts the pod: a leader whose services could not start must not hold
// the lease while serving nothing and looking healthy.
func (h *handler) drainStoppedEra(eraCancel context.CancelFunc) {
	eraCancel()
	// stop the listeners and close the allocator before the era join: a
	// draining worker must not re-open a listener behind the teardown
	h.stopDHCPListeners()
	h.listenerWg.Wait()
	h.RemoveLeaderPodLabel()
	h.NetworkCleanup()

	os.Exit(1)
}

// onStoppedLeading joins the era and cleans the host state after the
// leadership was lost. the dhcp listeners were already stopped and the
// allocator closed at era-cancel time (see onStartedLeading): client-go
// releases the lease as soon as renew returns, so the standby may acquire
// and open its own listeners while this process drains, and the only way
// to keep a single server on the segment is to stop answering BEFORE the
// lease can pass. the stopDHCPListeners call below stays as an idempotent
// backstop for the paths which never ran a service era (a standby which
// never led). the exit status stays 0 for a graceful shutdown; the
// lease-loss is surfaced through the error metric and the error-level log.
func (h *handler) onStoppedLeading() {
	if !h.led.Load() {
		// client-go registers OnStoppedLeading as a deferred callback of
		// the election run and fires it even when acquire never
		// succeeded: every routine standby shutdown (a rollout
		// scale-down, a node drain, a SIGTERM of a never-leader) would
		// otherwise produce an error-level 'leader lost' log and an
		// error-metric increment - false alerts for any monitoring wired
		// to those signals. the cleanup backstops below still run: they
		// are all no-ops for a process which never led.
		log.Infof("(app.Run) election stopped without this process ever leading (standby shutdown)")
	} else {
		log.Errorf("(app.Run) leader lost: %s", h.leaderId)
		if h.metrics != nil {
			h.metrics.UpdateLogStatus("error")
		}
	}

	h.listenerWg.Wait()
	h.stopDHCPListeners()
	h.RemoveLeaderPodLabel()
	h.NetworkCleanup()
}

// leaderWatchdogLoop force-exits a stale leader: a leader which lost the
// api cannot renew its lease, and the standby acquires once the lease
// expired. this fence is the backstop for a wedged election loop (the
// primary fences are the election loop's own renew deadline and the
// liveness probe): the check fails only while this client still owns the
// lease record but could not renew it (a follower and a healthy leader
// never fail), and the force-exit lands shortly after the lease-expiry
// horizon, so the standby may already hold the lease - but the dhcp
// listeners and the nic addresses are torn down from the local state (the
// dhcp registry and the pool cache, no api calls), so the stale leader
// stops answering dhcp within seconds of the first failed checks.
func (h *handler) leaderWatchdogLoop(adaptor *leaderelection.HealthzAdaptor) {
	var staleCount int

	for {
		time.Sleep(5 * time.Second)

		if adaptor.Check(nil) != nil {
			staleCount++
			if staleCount >= 2 {
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

func (h *handler) RunServices(ctx context.Context) error {
	// allocate the shared state of this service era and publish it through
	// the atomic era pointer: the handlers constructed below keep the
	// pointers they were constructed with, so zombie workers of a previous
	// era can never write into the startup gate or the status of this era,
	// while the shutdown paths on other goroutines always see the current
	// era. the metrics and health endpoints are process-global (started in
	// Run) and are never restarted per era
	era := &eraState{
		appStatus:    new(atomic.Int32),
		ippoolGate:   gate.New(),
		vmnetcfgGate: gate.New(),
		ipam:         ipam.New(),
		dhcp:         dhcp.New(),
		cache:        cache.New(),
		reconcileMu:  &sync.Mutex{},
	}
	era.appStatus.Store(APP_INIT)
	h.era.Store(era)

	// add the kubevirtiphelper/leader pod label. the write is bound to
	// the era context: a canceled era must not re-add the label after a
	// shutdown path removed it
	h.addLeaderPodLabel(ctx)
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
	// record the exact startup snapshot of the gate: the pool names the
	// LIST saw are the keys which must settle before the startup proceeds,
	// so a pool created after the snapshot can never substitute for an
	// unvisited pre-existing one
	ippoolGateKeys := make([]string, 0, len(IPPoolList))
	for i := range IPPoolList {
		ippoolGateKeys = append(ippoolGateKeys, IPPoolList[i].Name)
	}
	era.ippoolGate.SetTarget(ippoolGateKeys)

	// initialize the ippoolEventListener handler
	h.ippoolEventHandler = ippool.NewEventHandler(
		ctx,
		era.ipam,
		era.dhcp,
		h.metrics,
		era.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
		era.appStatus,
		era.ippoolGate,
		h.networkScope,
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
	if err := h.waitForStartupGate(ctx, "IPPool", era.ippoolGate, 5*time.Second,
		func(tick int, settled int, target int) {
			switch {
			case tick == 12:
				log.Warnf("app.RunServices) still waiting for IPPool initialization [%d out of %d] after 1 min.", settled, target)
				h.metrics.UpdateLogStatus("warning")
			case tick == 24:
				log.Errorf("app.RunServices) DHCP services are still NOT running [%d out of %d]! There might be something wrong with one of the IPPools!"+
					" Check above logs for errors and fix them. The startup gives up when the count stops progressing.", settled, target)
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
	// record the exact startup snapshot of the gate: the keys the LIST saw
	// are the keys which must settle before the startup proceeds, so a
	// vmnetcfg created after the snapshot can never substitute for an
	// unvisited pre-existing one
	vmnetcfgGateKeys := make([]string, 0, len(vmnetcfgList))
	for i := range vmnetcfgList {
		vmnetcfgGateKeys = append(vmnetcfgGateKeys, vmnetcfgList[i].Namespace+"/"+vmnetcfgList[i].Name)
	}
	era.vmnetcfgGate.SetTarget(vmnetcfgGateKeys)

	// initialize the vmnetcfgEventListener handler
	h.vmnetcfgEventHandler = vmnetcfg.NewEventHandler(
		ctx,
		era.ipam,
		era.dhcp,
		h.metrics,
		era.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
		era.appStatus,
		era.vmnetcfgGate,
		h.networkScope,
		era.reconcileMu,
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
	if err := h.waitForStartupGate(ctx, "VirtualMachineNetworkConfiguration", era.vmnetcfgGate, 10*time.Second,
		func(tick int, settled int, target int) {
			switch {
			case tick == 30:
				log.Warnf("app.RunServices) still waiting for VirtualMachineNetworkConfiguration initialization [%d out of %d] after 5 mins.", settled, target)
				h.metrics.UpdateLogStatus("warning")
			case tick == 60:
				log.Warnf("app.RunServices) still waiting for VirtualMachineNetworkConfiguration initialization [%d out of %d] after 10 mins.", settled, target)
				h.metrics.UpdateLogStatus("warning")
			case tick == 90:
				log.Errorf("app.RunServices) VirtualMachineNetworkConfiguration initialization is still not complete [%d out of %d] after > 15 mins! There might be something wrong with the VmNetCfgs count!"+
					" Check above logs for errors and fix them. The startup gives up when the count stops progressing.", settled, target)
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
		ctx,
		era.ipam,
		era.dhcp,
		h.metrics,
		era.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
		nil,
		h.networkScope,
		era.reconcileMu,
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

// waitForStartupGate blocks until every object of the startup snapshot
// settled (the membership gate opens only on the exact snapshot keys, so
// an object created after the snapshot can never substitute for an
// unvisited pre-existing one) or the era context is canceled, and fails
// the startup when the settlement stops advancing for
// startupStallTimeout. the reporting callback is invoked once per tick so
// the caller keeps its escalating progress logs.
func (h *handler) waitForStartupGate(ctx context.Context, what string, startupGate *gate.Gate, tick time.Duration, report func(tick int, settled int, target int)) error {
	lastSettled := -1
	var stalledSince time.Time
	tickCount := 0

	for {
		if startupGate.Open() {
			return nil
		}

		settled, target := startupGate.Settled(), startupGate.Target()
		if settled != lastSettled {
			lastSettled = settled
			stalledSince = time.Now()
		}
		if !stalledSince.IsZero() && time.Since(stalledSince) > startupStallTimeout {
			log.Errorf("app.RunServices) %s initialization has not progressed for %s [%d out of %d], giving up so the pod restarts",
				what, startupStallTimeout, settled, target)
			h.metrics.UpdateLogStatus("error")

			return fmt.Errorf("%s initialization has not progressed for %s (%d/%d objects)", what, startupStallTimeout, settled, target)
		}

		tickCount++
		if report != nil {
			report(tickCount, settled, target)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tick):
		}
	}
}

// startupListTimeout bounds one startup snapshot gather of retryList. an
// unbounded retry would hold the leadership lease forever when the LIST
// fails permanently while the coordination api stays reachable (a deleted
// CRD answers 404, an RBAC regression answers 403): nothing else fences
// that state - the lease keeps renewing, so the liveness probe passes, the
// stall fence of the startup gate is never reached and the standby can
// never acquire. the give-up runs the drainStoppedEra path, which exits
// the process so the kubelet restarts the pod and the fresh attempt
// (possibly served by a standby) starts with a released lease. the bound
// stays under startupStallTimeout so one give-up cycle plus one gate
// stall fits the 30-minute progress deadline of the deployment. it is a
// variable so the test can shrink it.
var startupListTimeout = 10 * time.Minute

// startupRetryDelay is the short backoff between the first retryList
// attempts (the later ones back off to a minute). it is a variable so the
// test can shrink it.
var startupRetryDelay = 5 * time.Second

// retryList repeatedly gathers a startup snapshot until it succeeds, the
// era context is cancelled, or the startupListTimeout budget is exhausted:
// a transient api error must never leave the leader running without
// controllers (the silent early return made the whole ip management dead
// while the pod looked healthy), but a permanently failing gather must
// also give up so the lease is released instead of being renewed forever
// by a leader which serves nothing. the first attempts run on a short
// interval to heal quickly, the later ones back off to a minute so a
// sustained outage does not spam the api. every attempt is additionally
// bounded: a gather which hangs on a tcp blackhole must not block the era
// join (the parent context only aborts between attempts).
func retryList[T any](ctx context.Context, m *metrics.MetricsAllocator, what string, gather func(ctx context.Context) (T, error)) (result T, err error) {
	deadline := time.Now().Add(startupListTimeout)
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

		if time.Now().After(deadline) {
			log.Errorf("(app.RunServices) %s still cannot be gathered after %s, giving up so the pod restarts and the leadership lease is released: %s",
				what, startupListTimeout, err.Error())
			m.UpdateLogStatus("error")

			return result, fmt.Errorf("%s still cannot be gathered after %s: %w", what, startupListTimeout, err)
		}

		m.UpdateLogStatus("error")

		delay := startupRetryDelay
		if attempt >= 5 {
			delay = time.Minute
		}

		if attempt == 10 {
			log.Errorf("(app.RunServices) %s still cannot be gathered after %d attempts; the cluster api may be unreachable: the controllers cannot serve without the startup snapshot, retrying until the startup retry budget is exhausted", what, attempt)
		}

		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (h *handler) getIPPools(ctx context.Context) (IPPools []v1.IPPool, err error) {
	if h.networkScope.NetworkName() == "" {
		return nil, fmt.Errorf("cannot discover IPPools without a network identity")
	}
	kubeRestConfig, err := h.getKubeConfig()
	if err != nil {
		return IPPools, fmt.Errorf("cannot get kubeRestConfig: %s", err.Error())
	}

	kihClientset, err := kihclientset.NewForConfig(kubeRestConfig)
	if err != nil {
		return IPPools, fmt.Errorf("cannot get kihClientset: %s", err.Error())
	}

	IPPoolList, err := kihClientset.KubevirtiphelperV1().IPPools().List(ctx, metav1.ListOptions{LabelSelector: h.networkScope.Selector()})
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

// NetworkCleanup removes the server ip of every pool registered in this
// process era from its bind interface, sourced from the local pool cache:
// the shutdown paths must never depend on the api (a stale leader which
// lost the api is exactly the case these fences exist for). the startup
// cleanup of a previously killed process is a separate concern: a fresh
// process has no local pool cache yet, so StartupNetworkCleanup gathers
// the pools from the api instead.
func (h *handler) NetworkCleanup() {
	era := h.era.Load()
	if era == nil || era.cache == nil {
		// this process never ran services (it never acquired the
		// leadership): it holds no server addresses of its own to remove
		return
	}

	for _, pool := range era.cache.List("pool") {
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

// StartupNetworkCleanup removes only this network's server addresses after a
// previously killed process. A fresh process gathers selected pools from the API;
// an unreachable API skips the workaround, never widens discovery.
func (h *handler) StartupNetworkCleanup() {
	// bound the gather: the cleanup runs before the leader election starts
	// where a hang must not block the startup
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	IPPoolList, err := h.getIPPools(ctx)
	if err != nil {
		log.Errorf("(app.StartupNetworkCleanup) %s", err.Error())

		return
	}

	for _, pool := range IPPoolList {
		if !h.networkScope.MatchesPool(&pool) {
			log.Errorf("(app.StartupNetworkCleanup) refusing pool [%s]: labels and spec.networkname must identify [%s]",
				pool.Name, h.networkScope.NetworkName())
			continue
		}
		// remove the IP address from the bind interface
		ipnet, err := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet)
		if err != nil {
			log.Errorf("(app.StartupNetworkCleanup) error while parsing subnet [%s] during network cleanup for network [%s]: %s",
				pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, err.Error())

			continue
		}
		ip4 := fmt.Sprintf("%s/%d", pool.Spec.IPv4Config.ServerIP, ipnet.Bits())

		log.Debugf("(app.StartupNetworkCleanup) removing the IP4 address [%s] on nic [%s] for network [%s]",
			ip4, pool.Spec.BindInterface, pool.Spec.NetworkName)

		if err := network.RemoveIpFromNic(pool.Spec.BindInterface, ip4); err != nil {
			// this is defined as a debug log because the ip could have been already removed and this will cause an error
			log.Debugf("(app.StartupNetworkCleanup) error while removing IP4 address [%s] from bind interface [%s] for network [%s]: %s",
				ip4, pool.Spec.BindInterface, pool.Spec.NetworkName, err.Error())
		}
	}
}

// stopDHCPListeners stops every DHCP listener this process era started,
// straight from the dhcp registry: the shutdown paths must never depend on
// the api (a stale leader which lost the api is exactly the case these
// fences exist for), so the pool list is not gathered here anymore.
func (h *handler) stopDHCPListeners() {
	era := h.era.Load()
	if era == nil || era.dhcp == nil {
		// this process never ran services (it never acquired the
		// leadership): there are no listeners of its own to stop
		return
	}

	era.dhcp.StopAll()
}

// The addLeaderPodLabel and removeLeaderPodLabel funtions are managing the kubevirtiphelper/leader label.
// This label is used by the metrics-service to determine the active leader.
// If the function(s) fail the application should ignore it and still service DHCP requests.
func (h *handler) addLeaderPodLabel(ctx context.Context) {
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

	// bound the api calls: a hang must never block the startup phase. the
	// bound derives from the caller's era context, so a canceled era
	// aborts the label write instead of completing it behind the shutdown
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
