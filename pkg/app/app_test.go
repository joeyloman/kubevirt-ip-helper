package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/leaderelection"

	v1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// The tests in this file cover the app handler's configuration, listing,
// leader-label and network-cleanup boundaries using temp files and httptest
// REST endpoints. The blocking parts of the handler are intentionally not
// exercised here:
//   - Run and RunServices are skipped because Run always runs the OnStoppedLeading
//     callback (which calls os.Exit(1)) and RunServices starts the DHCP service,
//     which binds to UDP port 67 and mutates host routing.
//   - Nothing here depends on a cluster, in-cluster credentials, or host
//     networking: the only host side effects are read-only netlink lookups
//     against an interface name that cannot exist ("").

// captureHook records log entries so tests can assert on log-only behaviors.
type captureHook struct {
	mu      sync.Mutex
	entries []log.Entry
}

func (h *captureHook) Levels() []log.Level { return log.AllLevels }

func (h *captureHook) Fire(entry *log.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, *entry)
	return nil
}

func (h *captureHook) contains(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

// attachLogCapture installs a hook that collects every log line (including
// debug lines) and restores the previous level and hooks afterwards.
func attachLogCapture(t *testing.T) *captureHook {
	t.Helper()
	oldLevel := log.GetLevel()
	oldHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := &captureHook{}
	log.AddHook(hook)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.StandardLogger().ReplaceHooks(oldHooks)
	})
	return hook
}

// assertPanics runs fn and returns the recovered value, failing the test if
// fn does not panic.
func assertPanics(t *testing.T, fn func()) interface{} {
	t.Helper()
	var recovered interface{}
	panicked := false
	func() {
		defer func() {
			recovered = recover()
			panicked = true
		}()
		fn()
	}()
	if !panicked {
		t.Fatalf("expected a panic, got none")
	}
	return recovered
}

// writeTestKubeconfig writes a minimal kubeconfig pointing at serverURL to a
// temp file and returns its path.
func writeTestKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`, serverURL)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing test kubeconfig: %s", err)
	}
	return path
}

// clearInClusterEnv guarantees getKubeConfig's in-cluster fallback fails
// deterministically, regardless of the environment the test runs in.
func clearInClusterEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	// an inherited kube context from a developer or ci environment would
	// silently select a nonexistent fixture context
	t.Setenv("KUBECONTEXT", "")
}

func TestHandler_Register(t *testing.T) {
	h := Register()
	if h == nil {
		t.Fatal("Register() returned nil")
	}
	if h.listenerWg == nil {
		t.Error("fresh handler listenerWg is nil, want an allocated WaitGroup")
	}
	if h.era.Load() != nil {
		t.Error("fresh handler era is set, want no era before the leadership is acquired")
	}
	if h.kubeConfigFile != "" {
		t.Errorf("fresh handler kubeConfigFile = %q, want empty", h.kubeConfigFile)
	}
}

func TestHandler_getKubeConfig(t *testing.T) {
	t.Run("existing valid kubeconfig", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL)}
		cfg, err := h.getKubeConfig()
		if err != nil {
			t.Fatalf("getKubeConfig() unexpected error: %s", err)
		}
		if cfg.Host != srv.URL {
			t.Errorf("config Host = %q, want %q", cfg.Host, srv.URL)
		}
	})

	t.Run("missing kubeconfig falls back to in-cluster and fails outside a cluster", func(t *testing.T) {
		clearInClusterEnv(t)
		h := &handler{kubeConfigFile: filepath.Join(t.TempDir(), "does-not-exist")}
		_, err := h.getKubeConfig()
		if err == nil {
			t.Fatal("getKubeConfig() expected an error for a missing kubeconfig outside a cluster")
		}
		if !strings.Contains(err.Error(), "in-cluster") {
			t.Errorf("getKubeConfig() error = %q, want an in-cluster configuration error", err)
		}
	})

	t.Run("directory path is treated as missing", func(t *testing.T) {
		clearInClusterEnv(t)
		h := &handler{kubeConfigFile: t.TempDir()}
		_, err := h.getKubeConfig()
		if err == nil {
			t.Fatal("getKubeConfig() expected an error when kubeConfigFile is a directory")
		}
	})

	t.Run("malformed kubeconfig is rejected", func(t *testing.T) {
		kubeConfigPath := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(kubeConfigPath, []byte("not: [valid yaml"), 0600); err != nil {
			t.Fatalf("writing malformed kubeconfig: %s", err)
		}
		h := &handler{kubeConfigFile: kubeConfigPath}
		if _, err := h.getKubeConfig(); err == nil {
			t.Fatal("getKubeConfig() expected an error for a malformed kubeconfig")
		}
	})

	t.Run("unknown kube context is rejected", func(t *testing.T) {
		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, "http://127.0.0.1:1"),
			kubeContext:    "does-not-exist",
		}
		if _, err := h.getKubeConfig(); err == nil {
			t.Fatal("getKubeConfig() expected an error for an unknown kubeContext")
		}
	})
}

// requestRecorder implements a minimal kih REST server for the generated
// clientset, recording the request path it served.
type requestRecorder struct {
	mu   sync.Mutex
	path string
}

func (r *requestRecorder) record(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = path
}

func (r *requestRecorder) got() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path
}

func testNetworkScope(t *testing.T) util.NetworkScope {
	t.Helper()
	scope, err := util.NewNetworkScope("kubevirt-ip-helper", "management")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func testNetworkPool(name, namespace, networkName string) v1.IPPool {
	return v1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			util.NetworkLabel: networkName, util.NetworkNamespaceLabel: namespace,
		}},
		Spec: v1.IPPoolSpec{
			NetworkName:   namespace + "/" + networkName,
			BindInterface: "net1",
			IPv4Config:    v1.IPv4Config{ServerIP: "192.168.1.1", Subnet: "192.168.1.0/24"},
		},
	}
}

const vmnetcfgListJSON = `{
  "kind": "VirtualMachineNetworkConfigList",
  "apiVersion": "kubevirtiphelper.k8s.binbash.org/v1",
  "metadata": {},
  "items": [
    {
      "metadata": {"name": "vm-a"},
      "spec": {
        "vmname": "vm1",
        "networkconfig": [
          {"ipaddress": "10.0.0.5", "macaddress": "aa:bb:cc:dd:ee:ff", "networkname": "net-a"}
        ]
      }
    }
  ]
}`

func TestHandler_getIPPools(t *testing.T) {
	const path = "/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools"

	t.Run("discovers only this network including selected invalid pools", func(t *testing.T) {
		scope := testNetworkScope(t)
		own := testNetworkPool("own", scope.Namespace(), scope.Name())
		invalid := testNetworkPool("invalid", scope.Namespace(), scope.Name())
		invalid.Spec.NetworkName = scope.Name()
		otherName := testNetworkPool("other-name", scope.Namespace(), "storage")
		otherNamespace := testNetworkPool("other-namespace", "tenant", scope.Name())
		unlabelled := testNetworkPool("unlabelled", scope.Namespace(), scope.Name())
		unlabelled.Labels = nil
		all := []v1.IPPool{own, invalid, otherName, otherNamespace, unlabelled}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			selector, err := labels.Parse(r.URL.Query().Get("labelSelector"))
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			list := v1.IPPoolList{}
			for _, pool := range all {
				if selector.Matches(labels.Set(pool.Labels)) {
					list.Items = append(list.Items, pool)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)
		}))
		defer srv.Close()
		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL), networkScope: scope}
		pools, err := h.getIPPools(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(pools, []v1.IPPool{own, invalid}) {
			t.Fatalf("startup discovery = %+v; want own and selected invalid pools only", pools)
		}
	})

	t.Run("unresolved identity never discovers pools", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL)}
		if _, err := h.getIPPools(context.Background()); err == nil {
			t.Fatal("unresolved identity accepted")
		}
		if requests.Load() != 0 {
			t.Fatal("unresolved identity reached discovery API")
		}
	})

	t.Run("API error is wrapped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"boom","reason":"InternalError","code":500}`)
		}))
		defer srv.Close()

		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL), networkScope: testNetworkScope(t)}
		_, err := h.getIPPools(context.Background())
		if err == nil {
			t.Fatal("getIPPools() expected an error for an API failure")
		}
		if !strings.Contains(err.Error(), "cannot get the IPPoolList") {
			t.Errorf("getIPPools() error = %q, want it wrapped with the list context", err)
		}
	})

	t.Run("missing kubeconfig is wrapped", func(t *testing.T) {
		clearInClusterEnv(t)
		h := &handler{kubeConfigFile: filepath.Join(t.TempDir(), "does-not-exist"), networkScope: testNetworkScope(t)}
		_, err := h.getIPPools(context.Background())
		if err == nil {
			t.Fatal("getIPPools() expected an error without a kubeconfig")
		}
		if !strings.Contains(err.Error(), "cannot get kubeRestConfig") {
			t.Errorf("getIPPools() error = %q, want it wrapped with the config context", err)
		}
	})
}

func TestHandler_getVmNetCfgs(t *testing.T) {
	const path = "/apis/kubevirtiphelper.k8s.binbash.org/v1/virtualmachinenetworkconfigs"

	t.Run("lists vmnetcfgs from the API", func(t *testing.T) {
		rec := &requestRecorder{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record(r.URL.Path)
			if r.URL.Path != path {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.URL.Query().Get("labelSelector") != "" {
				t.Error("VMNetCfg startup discovery must remain unfiltered")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, vmnetcfgListJSON)
		}))
		defer srv.Close()

		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL)}
		cfgs, err := h.getVmNetCfgs(context.Background())
		if err != nil {
			t.Fatalf("getVmNetCfgs() unexpected error: %s", err)
		}
		if rec.got() != path {
			t.Errorf("request path = %q, want %q", rec.got(), path)
		}
		if len(cfgs) != 1 {
			t.Fatalf("got %d vmnetcfgs, want 1", len(cfgs))
		}
		if cfgs[0].Spec.VMName != "vm1" {
			t.Errorf("unexpected vmname: %q", cfgs[0].Spec.VMName)
		}
		if len(cfgs[0].Spec.NetworkConfig) != 1 || cfgs[0].Spec.NetworkConfig[0].IPAddress != "10.0.0.5" {
			t.Errorf("unexpected network config: %+v", cfgs[0].Spec.NetworkConfig)
		}
	})

	t.Run("API error is wrapped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL)}
		_, err := h.getVmNetCfgs(context.Background())
		if err == nil {
			t.Fatal("getVmNetCfgs() expected an error for an API failure")
		}
		if !strings.Contains(err.Error(), "cannot get the vmnetcfgList") {
			t.Errorf("getVmNetCfgs() error = %q, want it wrapped with the list context", err)
		}
	})
}

func TestHandler_NetworkCleanup(t *testing.T) {
	t.Run("removes the addresses of the locally registered pools without the api", func(t *testing.T) {
		hook := attachLogCapture(t)

		cacheAllocator := cache.New()
		if err := cacheAllocator.Add(&v1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
			Spec: v1.IPPoolSpec{
				NetworkName:   "net-a",
				BindInterface: "",
				IPv4Config: v1.IPv4Config{
					ServerIP: "192.168.1.1",
					Subnet:   "192.168.1.0/24",
				},
			},
		}); err != nil {
			t.Fatalf("adding the pool to the cache: %s", err)
		}
		if err := cacheAllocator.Add(&v1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-bad"},
			Spec: v1.IPPoolSpec{
				NetworkName:   "net-bad",
				BindInterface: "eth0",
				IPv4Config: v1.IPv4Config{
					ServerIP: "not-an-ip",
					Subnet:   "not-a-subnet",
				},
			},
		}); err != nil {
			t.Fatalf("adding the bad pool to the cache: %s", err)
		}

		// an unreachable kubeconfig proves the api is never contacted
		h := &handler{kubeConfigFile: filepath.Join(t.TempDir(), "kubeconfig")}
		h.era.Store(&eraState{cache: cacheAllocator})
		h.NetworkCleanup() // must not panic

		// the good pool is processed from the local cache and its removal is
		// attempted (no interface named "" can exist, so the removal fails
		// and is logged at debug), while the bad subnet only skips its own
		// pool
		if !hook.contains("removing the IP4 address [192.168.1.1/24] on nic [] for network [net-a]") {
			t.Errorf("expected the cached pool to be cleaned up, got:\n%s", hook.entriesText())
		}
		if !hook.contains("error while removing IP4 address [192.168.1.1/24] from bind interface [] for network [net-a]") {
			t.Errorf("expected the debug log for the missing interface, got:\n%s", hook.entriesText())
		}
		if !hook.contains("error while parsing subnet [not-a-subnet]") {
			t.Errorf("expected a log entry about the unparsable subnet, got:\n%s", hook.entriesText())
		}
		if hook.contains("app.StartupNetworkCleanup") {
			t.Errorf("the shutdown cleanup must not gather pools from the api, got:\n%s", hook.entriesText())
		}
	})

	t.Run("no era means no cleanup and no api call", func(t *testing.T) {
		hook := attachLogCapture(t)
		h := &handler{
			// an unreachable kubeconfig proves the api is never contacted
			kubeConfigFile: filepath.Join(t.TempDir(), "kubeconfig"),
			namespace:      "testns",
		}
		h.NetworkCleanup() // must not panic

		if len(hook.entries) != 0 {
			t.Errorf("expected no cleanup logs without an era, got:\n%s", hook.entriesText())
		}
	})
}

func TestHandler_StartupNetworkCleanup(t *testing.T) {
	scope := testNetworkScope(t)
	removeIP := network.RemoveIpFromNic
	t.Cleanup(func() { network.RemoveIpFromNic = removeIP })
	t.Run("refuses invalid identity before network mutation and continues own cleanup", func(t *testing.T) {
		own := testNetworkPool("own", scope.Namespace(), scope.Name())
		foreign := testNetworkPool("foreign", scope.Namespace(), "storage")
		wrongNamespace := testNetworkPool("other-namespace", "tenant", scope.Name())
		unqualified := testNetworkPool("bare", scope.Namespace(), scope.Name())
		unqualified.Spec.NetworkName = scope.Name()
		mismatch := testNetworkPool("mismatch", scope.Namespace(), scope.Name())
		mismatch.Spec.NetworkName = foreign.Spec.NetworkName
		unlabelled := testNetworkPool("unlabelled", scope.Namespace(), scope.Name())
		unlabelled.Labels = nil
		badSubnet := testNetworkPool("bad-subnet", scope.Namespace(), scope.Name())
		badSubnet.Spec.IPv4Config.Subnet = "invalid"
		pools := []v1.IPPool{foreign, wrongNamespace, unqualified, mismatch, unlabelled, badSubnet, own}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Deliberately return foreign rows too: cleanup must still validate
			// each row before touching host addresses.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v1.IPPoolList{Items: pools})
		}))
		defer srv.Close()
		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL), networkScope: scope}
		var removals []string
		network.RemoveIpFromNic = func(nic, address string) error {
			removals = append(removals, nic+" "+address)
			return nil
		}
		h.StartupNetworkCleanup()
		if !reflect.DeepEqual(removals, []string{"net1 192.168.1.1/24"}) {
			t.Fatalf("host address mutations = %v, want only the valid own pool", removals)
		}
	})

	t.Run("failed discovery never mutates host addresses", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL), networkScope: scope}
		network.RemoveIpFromNic = func(nic, address string) error {
			t.Error("failed discovery reached host mutation")
			return nil
		}
		h.StartupNetworkCleanup()
	})
}

func TestHandler_stopDHCPListeners(t *testing.T) {
	t.Run("stops from the local registry without the api", func(t *testing.T) {
		hook := attachLogCapture(t)

		h := &handler{
			// an unreachable kubeconfig proves the api is never contacted
			kubeConfigFile: filepath.Join(t.TempDir(), "kubeconfig"),
			namespace:      "testns",
		}
		h.era.Store(&eraState{dhcp: dhcp.New()})
		h.stopDHCPListeners() // must not panic

		if len(hook.entries) != 0 {
			t.Errorf("the registry stop needs no api and logs nothing on an empty registry, got:\n%s", hook.entriesText())
		}
	})

	t.Run("no era means no listeners of this process", func(t *testing.T) {
		hook := attachLogCapture(t)
		h := &handler{}
		h.stopDHCPListeners() // must not panic

		if len(hook.entries) != 0 {
			t.Errorf("expected no logs without an era, got:\n%s", hook.entriesText())
		}
	})
}

// podStore is a tiny in-memory pod API: GET and PUT against
// /api/v1/namespaces/<ns>/pods/<name>.
type podStore struct {
	mu        sync.Mutex
	gets      int
	updates   int
	pods      map[string]*corev1.Pod
	conflicts int
}

func newPodStore(pods ...*corev1.Pod) *podStore {
	s := &podStore{pods: make(map[string]*corev1.Pod)}
	for _, p := range pods {
		s.pods[p.Name] = p
	}
	return s
}

func (s *podStore) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/api/v1/namespaces/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 3 || parts[0] == "" || parts[1] != "pods" || parts[2] == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		name := parts[2]

		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			s.gets++
			p, ok := s.pods[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writePodJSON(w, p.DeepCopy())
		case http.MethodPut:
			s.updates++
			// simulate a concurrent modification: the next n replaces
			// answer with a resource-version conflict so the caller's
			// retry-on-conflict path is exercised
			if s.conflicts > 0 {
				s.conflicts--

				writeKubeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "the object has been modified")

				return
			}
			var p corev1.Pod
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.pods[name] = p.DeepCopy()
			writePodJSON(w, s.pods[name])
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func writePodJSON(w http.ResponseWriter, p *corev1.Pod) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func writeKubeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(&metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   reason,
		Message:  message,
		Code:     int32(code),
	})
}

func (s *podStore) pod(name string) *corev1.Pod {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pods[name].DeepCopy()
}

func (s *podStore) counts() (gets, updates int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.updates
}

const leaderLabel = "kubevirtiphelper/leader"

func TestHandler_addLeaderPodLabel(t *testing.T) {
	t.Run("adds the leader label to the current pod", func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil {
			t.Fatalf("os.Hostname(): %s", err)
		}
		store := newPodStore(&corev1.Pod{
			TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: hostname, Namespace: "testns", Labels: map[string]string{"app": "demo"}},
		})
		srv := httptest.NewServer(store.handler())
		defer srv.Close()

		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}
		h.addLeaderPodLabel(context.Background())

		p := store.pod(hostname)
		if p == nil {
			t.Fatal("pod was not stored")
		}
		if got := p.Labels[leaderLabel]; got != "active" {
			t.Errorf("leader label = %q, want %q", got, "active")
		}
		if got := p.Labels["app"]; got != "demo" {
			t.Errorf("pre-existing label app = %q, want %q", got, "demo")
		}
		gets, updates := store.counts()
		if gets != 1 || updates != 1 {
			t.Errorf("got %d gets and %d updates, want 1 and 1", gets, updates)
		}
	})

	t.Run("retries the label when the pod is concurrently modified", func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil {
			t.Fatalf("os.Hostname(): %s", err)
		}
		store := newPodStore(&corev1.Pod{
			TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: hostname, Namespace: "testns", Labels: map[string]string{"app": "demo"}},
		})
		store.conflicts = 1
		srv := httptest.NewServer(store.handler())
		defer srv.Close()

		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}
		h.addLeaderPodLabel(context.Background())

		p := store.pod(hostname)
		if got := p.Labels[leaderLabel]; got != "active" {
			t.Errorf("leader label = %q, want %q after the conflict retry", got, "active")
		}
		if got := p.Labels["app"]; got != "demo" {
			t.Errorf("pre-existing label app = %q, want %q", got, "demo")
		}
		gets, updates := store.counts()
		if gets != 2 || updates != 2 {
			t.Errorf("got %d gets and %d updates, want 2 and 2 after one conflict retry", gets, updates)
		}
	})

	t.Run("never writes the label of a canceled era", func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil {
			t.Fatalf("os.Hostname(): %s", err)
		}
		store := newPodStore(&corev1.Pod{
			TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: hostname, Namespace: "testns", Labels: map[string]string{"app": "demo"}},
		})
		srv := httptest.NewServer(store.handler())
		defer srv.Close()

		hook := attachLogCapture(t)
		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}

		// the era context is already canceled: the leadership was lost
		// during the restart backoff, and a label write which completed
		// behind the shutdown would keep the metrics service routing to
		// a pod which leads nothing
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h.addLeaderPodLabel(ctx)

		p := store.pod(hostname)
		if p == nil {
			t.Fatal("pod was not stored")
		}
		if got, labeled := p.Labels[leaderLabel]; labeled {
			t.Errorf("leader label = %q, want unset: a canceled era must not label its pod", got)
		}
		if _, updates := store.counts(); updates != 0 {
			t.Errorf("updates = %d, want 0 for a canceled era", updates)
		}
		if !hook.contains("cannot set the leader pod label") {
			t.Error("the refused label write must be surfaced as an error log")
		}
	})

	t.Run("logs and continues when the pod cannot be fetched", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		hook := attachLogCapture(t)
		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}
		h.addLeaderPodLabel(context.Background()) // must not panic

		if !hook.contains("cannot set the leader pod label") {
			t.Errorf("expected an error about the label update, got:\n%s", hook.entriesText())
		}
	})

	t.Run("logs and continues when the kubeconfig is invalid", func(t *testing.T) {
		clearInClusterEnv(t)
		hook := attachLogCapture(t)
		h := &handler{kubeConfigFile: filepath.Join(t.TempDir(), "does-not-exist")}
		h.addLeaderPodLabel(context.Background()) // must not panic

		if !hook.contains("cannot get kubeRestConfig") {
			t.Errorf("expected an error about the kubeconfig, got:\n%s", hook.entriesText())
		}
	})
}

func TestHandler_RemoveLeaderPodLabel(t *testing.T) {
	t.Run("removes the leader label and keeps the others", func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil {
			t.Fatalf("os.Hostname(): %s", err)
		}
		store := newPodStore(&corev1.Pod{
			TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      hostname,
				Namespace: "testns",
				Labels:    map[string]string{"app": "demo", leaderLabel: "active"},
			},
		})
		srv := httptest.NewServer(store.handler())
		defer srv.Close()

		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}
		h.RemoveLeaderPodLabel()

		p := store.pod(hostname)
		if p == nil {
			t.Fatal("pod was not stored")
		}
		if _, ok := p.Labels[leaderLabel]; ok {
			t.Errorf("leader label was not removed: %v", p.Labels)
		}
		if got := p.Labels["app"]; got != "demo" {
			t.Errorf("pre-existing label app = %q, want %q", got, "demo")
		}
		gets, updates := store.counts()
		if gets != 1 || updates != 1 {
			t.Errorf("got %d gets and %d updates, want 1 and 1", gets, updates)
		}
	})

	t.Run("retries the removal when the pod is concurrently modified", func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil {
			t.Fatalf("os.Hostname(): %s", err)
		}
		store := newPodStore(&corev1.Pod{
			TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      hostname,
				Namespace: "testns",
				Labels:    map[string]string{"app": "demo", leaderLabel: "active"},
			},
		})
		store.conflicts = 1
		srv := httptest.NewServer(store.handler())
		defer srv.Close()

		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}
		h.RemoveLeaderPodLabel()

		p := store.pod(hostname)
		if _, ok := p.Labels[leaderLabel]; ok {
			t.Errorf("leader label was not removed after the conflict retry: %v", p.Labels)
		}
		if got := p.Labels["app"]; got != "demo" {
			t.Errorf("pre-existing label app = %q, want %q", got, "demo")
		}
		gets, updates := store.counts()
		if gets != 2 || updates != 2 {
			t.Errorf("got %d gets and %d updates, want 2 and 2 after one conflict retry", gets, updates)
		}
	})

	t.Run("leaves a pod without the leader label unchanged", func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil {
			t.Fatalf("os.Hostname(): %s", err)
		}
		store := newPodStore(&corev1.Pod{
			TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: hostname, Namespace: "testns", Labels: map[string]string{"app": "demo"}},
		})
		srv := httptest.NewServer(store.handler())
		defer srv.Close()

		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}
		h.RemoveLeaderPodLabel()

		p := store.pod(hostname)
		if p == nil {
			t.Fatal("pod was not stored")
		}
		if len(p.Labels) != 1 || p.Labels["app"] != "demo" {
			t.Errorf("labels changed when there was no leader label: %v", p.Labels)
		}
	})

	t.Run("logs and continues when the pod cannot be fetched", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		hook := attachLogCapture(t)
		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}
		h.RemoveLeaderPodLabel() // must not panic

		if !hook.contains("cannot remove the leader pod label") {
			t.Errorf("expected an error about the label update, got:\n%s", hook.entriesText())
		}
	})
}

// TestOnStoppedLeadingNeverLedStaysQuiet pins the shutdown classification:
// client-go registers OnStoppedLeading as a deferred callback of the
// election run and fires it even when acquire never succeeded, so every
// routine standby shutdown (a rollout scale-down, a node drain, a SIGTERM
// of a never-leader) used to produce an error-level 'leader lost' log and
// an error-metric increment - false alerts for any monitoring wired to
// those signals. a process which never led logs its routine shutdown at
// info level without the error metric; a real lease loss keeps the
// error-level signal.
func TestOnStoppedLeadingNeverLedStaysQuiet(t *testing.T) {
	t.Run("standby which never led logs info without the error metric", func(t *testing.T) {
		clearInClusterEnv(t)
		hook := attachLogCapture(t)

		h := &handler{metrics: metrics.New(), kubeConfigFile: filepath.Join(t.TempDir(), "does-not-exist"), listenerWg: &sync.WaitGroup{}}
		h.onStoppedLeading()

		if hook.contains("leader lost") {
			t.Error("a standby which never led must not log a lease loss")
		}
		if !hook.contains("standby shutdown") {
			t.Error("the never-led shutdown must be logged as the routine standby case")
		}
		for _, entry := range hook.entries {
			if entry.Level == log.ErrorLevel && strings.Contains(entry.Message, "leader lost") {
				t.Errorf("the never-led shutdown logged at error level: %s", entry.Message)
			}
		}
	})

	t.Run("a real lease loss keeps the error-level signal", func(t *testing.T) {
		clearInClusterEnv(t)
		hook := attachLogCapture(t)

		h := &handler{metrics: metrics.New(), leaderId: "test-leader", kubeConfigFile: filepath.Join(t.TempDir(), "does-not-exist"), listenerWg: &sync.WaitGroup{}}
		h.led.Store(true)
		h.onStoppedLeading()

		if !hook.contains("leader lost: test-leader") {
			t.Error("a real lease loss must keep the error-level lease-loss log")
		}
	})
}

func TestHandler_Init(t *testing.T) {
	podName, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	namespaceFile := func(t *testing.T, namespace string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "namespace")
		if err := os.WriteFile(path, []byte(namespace), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Run("resolves identity before cleanup and retains it after a label edit", func(t *testing.T) {
		scope := testNetworkScope(t)
		store := newPodStore(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: podName, Namespace: scope.Namespace(),
			Labels: map[string]string{util.NetworkLabel: scope.Name(), leaderLabel: "active"},
		}})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools" {
				store.handler().ServeHTTP(w, r)
				return
			}
			selector, err := labels.Parse(r.URL.Query().Get("labelSelector"))
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			list := v1.IPPoolList{}
			for _, pool := range []v1.IPPool{
				testNetworkPool("original", scope.Namespace(), scope.Name()),
				testNetworkPool("relabeled", scope.Namespace(), "storage"),
			} {
				if selector.Matches(labels.Set(pool.Labels)) {
					list.Items = append(list.Items, pool)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)
		}))
		defer srv.Close()
		t.Setenv("KUBECONFIG", writeTestKubeconfig(t, srv.URL))
		t.Setenv("KUBECONTEXT", "test")
		h := Register()
		h.init(namespaceFile(t, scope.Namespace()+"\n"))
		if h.lock == nil || h.lock.LeaseMeta.Name != scope.LeaseName() || h.lock.LeaseMeta.Namespace != scope.Namespace() {
			t.Fatalf("wrong election destination: %+v", h.lock)
		}
		if _, found := store.pod(podName).Labels[leaderLabel]; found {
			t.Fatal("stale leader label retained")
		}
		store.mu.Lock()
		store.pods[podName].Labels[util.NetworkLabel] = "storage"
		store.mu.Unlock()
		pools, err := h.getIPPools(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(pools) != 1 || pools[0].Name != "original" {
			t.Fatalf("Pod relabel changed the running process's discovery: %+v", pools)
		}
	})

	t.Run("missing namespace file retains canonical namespace lookup", func(t *testing.T) {
		scope := testNetworkScope(t)
		store := newPodStore(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: podName, Namespace: scope.Namespace(), Labels: map[string]string{util.NetworkLabel: scope.Name()},
		}})
		srv := httptest.NewServer(store.handler())
		defer srv.Close()
		t.Setenv("KUBECONFIG", writeTestKubeconfig(t, srv.URL))
		t.Setenv("KUBECONTEXT", "test")
		h := Register()
		h.init(filepath.Join(t.TempDir(), "absent"))
		if h.networkScope != scope {
			t.Fatalf("network identity = %+v, want %+v", h.networkScope, scope)
		}
	})

	for _, tc := range []struct {
		name      string
		namespace string
		label     string
		apiError  bool
	}{
		{name: "missing label", namespace: "kubevirt-ip-helper"},
		{name: "invalid label", namespace: "kubevirt-ip-helper", label: "MANAGEMENT"},
		{name: "oversize label", namespace: "kubevirt-ip-helper", label: strings.Repeat("a", 64)},
		{name: "own Pod unavailable", namespace: "kubevirt-ip-helper", label: "management", apiError: true},
		{name: "empty namespace", label: "management"},
		{name: "invalid namespace", namespace: "INVALID", label: "management"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, forbidden atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/"+tc.namespace+"/pods/"+podName {
					forbidden.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if tc.apiError {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				writePodJSON(w, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: podName, Namespace: tc.namespace, Labels: map[string]string{util.NetworkLabel: tc.label},
				}})
			}))
			defer srv.Close()
			t.Setenv("KUBECONFIG", writeTestKubeconfig(t, srv.URL))
			t.Setenv("KUBECONTEXT", "test")
			path := namespaceFile(t, tc.namespace)
			h := Register()
			assertPanics(t, func() { h.init(path) })
			if forbidden.Load() != 0 {
				t.Fatal("invalid identity caused mutation, pool discovery or election access")
			}
			if h.lock != nil || h.era.Load() != nil {
				t.Fatal("invalid identity initialized election or services")
			}
			if (tc.namespace == "" || tc.namespace == "INVALID") && requests.Load() != 0 {
				t.Fatal("invalid namespace reached API")
			}
		})
	}

	t.Run("missing kubeconfig stops startup", func(t *testing.T) {
		t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
		clearInClusterEnv(t)
		h := Register()
		assertPanics(t, func() { h.init(namespaceFile(t, "kubevirt-ip-helper")) })
		if h.lock != nil {
			t.Fatal("missing kubeconfig initialized election")
		}
	})
}

func TestHandleErr(t *testing.T) {
	recovered := assertPanics(t, func() { handleErr(errors.New("test failure")) })
	if !strings.Contains(fmt.Sprint(recovered), "test failure") {
		t.Errorf("panic = %v, want it to contain the original error", recovered)
	}
}

func (h *captureHook) entriesText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var sb strings.Builder
	for _, e := range h.entries {
		fmt.Fprintf(&sb, "%s: %s\n", e.Level, e.Message)
	}
	return sb.String()
}

// The health endpoints are process-global, so a standby replica must stay
// live (a non-leader never fails the leaderElection check) and ready (a
// leadership-gated readiness would keep the deployment from ever reaching its
// desired availability), while a pod whose era is being built or torn down
// reports not-ready. This pins the contract the pod's kubelet probes and the
// deployment rollout rely on, against the real HTTP server.
func TestRegisterHealthChecksStandbyAndEraStates(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a metrics port: %s", err)
	}
	metricsPort := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved metrics port: %s", err)
	}
	t.Setenv("METRICS_PORT", strconv.Itoa(metricsPort))

	h := Register()
	h.leaderWatchdog = leaderelection.NewLeaderHealthzAdaptor(10 * time.Second)
	h.metrics = metrics.New()
	h.registerHealthChecks()
	go h.metrics.Run()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", metricsPort)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the metrics server never came up: %s", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	getStatusCode := func(path string) int {
		t.Helper()
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %s", path, err)
		}
		defer resp.Body.Close()

		return resp.StatusCode
	}

	// a standby never acquires the leadership: liveness must pass (this is
	// the check which used to kill every standby) and readiness must pass as
	// well - a non-leader replica which is never ready keeps the deployment
	// from reaching its desired availability, so every rollout of a
	// multi-replica deployment ends in ProgressDeadlineExceeded although the
	// leader serves
	if code := getStatusCode("/healthz"); code != http.StatusOK {
		t.Errorf("standby /healthz = %d, want %d", code, http.StatusOK)
	}
	if code := getStatusCode("/ready"); code != http.StatusOK {
		t.Errorf("standby /ready = %d, want %d", code, http.StatusOK)
	}

	// an era which is still initializing serves no leases yet
	era := &eraState{appStatus: new(atomic.Int32)}
	era.appStatus.Store(APP_INIT)
	h.era.Store(era)
	if code := getStatusCode("/ready"); code != http.StatusServiceUnavailable {
		t.Errorf("initializing /ready = %d, want %d", code, http.StatusServiceUnavailable)
	}

	// an era which is reinitializing has torn its services down
	era.appStatus.Store(APP_RESTART)
	if code := getStatusCode("/ready"); code != http.StatusServiceUnavailable {
		t.Errorf("restarting /ready = %d, want %d", code, http.StatusServiceUnavailable)
	}

	// once the era runs its services the pod becomes ready
	era.appStatus.Store(APP_RUNNING)
	if code := getStatusCode("/ready"); code != http.StatusOK {
		t.Errorf("running /ready = %d, want %d", code, http.StatusOK)
	}

	h.metrics.Stop()
}

// TestRetryListGivesUpAfterTheStartupBudget pins the lease-release fence
// of the startup snapshot: an unbounded retry would renew the leadership
// lease forever while the LIST fails permanently but the coordination api
// stays reachable (a deleted CRD, an RBAC regression) - nothing else
// fences that state, because the liveness probe passes, the startup gate
// stall fence is never reached and the standby can never acquire. the
// give-up returns the error so RunServices fails, the drainStoppedEra
// path exits the process and the kubelet restarts the pod.
func TestRetryListGivesUpAfterTheStartupBudget(t *testing.T) {
	hook := attachLogCapture(t)

	oldTimeout := startupListTimeout
	oldDelay := startupRetryDelay
	startupListTimeout = time.Nanosecond
	startupRetryDelay = time.Millisecond
	defer func() {
		startupListTimeout = oldTimeout
		startupRetryDelay = oldDelay
	}()

	m := metrics.NewMetricsAllocator()
	gather := func(ctx context.Context) (string, error) {
		return "", errors.New("boom")
	}

	_, err := retryList(context.Background(), m, "the test list", gather)
	if err == nil {
		t.Fatal("retryList returned nil error, want the give-up after the budget")
	}
	if !strings.Contains(err.Error(), "still cannot be gathered after") {
		t.Errorf("give-up error = %q, want the budget-give-up classification", err.Error())
	}
	if !hook.contains("giving up so the pod restarts and the leadership lease is released") {
		t.Error("the give-up must log the lease-release rationale")
	}
}

// TestRetryListHealsTransientFailures pins the retry contract the budget
func TestRetryListHealsTransientFailures(t *testing.T) {
	m := metrics.NewMetricsAllocator()

	oldTimeout := startupListTimeout
	oldDelay := startupRetryDelay
	startupListTimeout = time.Minute
	startupRetryDelay = time.Millisecond
	defer func() {
		startupListTimeout = oldTimeout
		startupRetryDelay = oldDelay
	}()
	attempts := 0
	gather := func(ctx context.Context) (int, error) {
		attempts++
		if attempts < 3 {
			return 0, errors.New("transient")
		}

		return 42, nil
	}

	result, err := retryList(context.Background(), m, "the test list", gather)
	if err != nil {
		t.Fatalf("retryList failed on a healable gather: %s", err)
	}
	if result != 42 {
		t.Errorf("result = %d, want the gathered 42", result)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// TestRetryListReturnsOnCanceledEra pins the drain contract: a canceled
// era context must win over both the retry loop and the budget, so the
// graceful drain (leadership loss, restart) is never blocked by the
// gather.
func TestRetryListReturnsOnCanceledEra(t *testing.T) {
	m := metrics.NewMetricsAllocator()
	ctx, cancel := context.WithCancel(context.Background())

	gather := func(ctx context.Context) (int, error) {
		cancel()

		return 0, errors.New("boom")
	}

	_, err := retryList(ctx, m, "the test list", gather)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("retryList on a canceled era = %v, want context.Canceled", err)
	}
}
