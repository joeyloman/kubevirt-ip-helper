package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
}

func TestHandler_Register(t *testing.T) {
	h := Register()
	if h == nil {
		t.Fatal("Register() returned nil")
	}
	if h.appStatus != APP_INIT {
		t.Errorf("fresh handler appStatus = %d, want %d (APP_INIT)", h.appStatus, APP_INIT)
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

const ipPoolListJSON = `{
  "kind": "IPPoolList",
  "apiVersion": "kubevirtiphelper.k8s.binbash.org/v1",
  "metadata": {},
  "items": [
    {
      "metadata": {"name": "pool-a"},
      "spec": {
        "networkname": "net-a",
        "bindinterface": "eth0",
        "ipv4config": {
          "serverip": "192.168.1.1",
          "subnet": "192.168.1.0/24"
        }
      }
    },
    {
      "metadata": {"name": "pool-b"},
      "spec": {
        "networkname": "net-b",
        "bindinterface": "eth1",
        "ipv4config": {
          "serverip": "10.0.0.1",
          "subnet": "10.0.0.0/24"
        }
      }
    }
  ]
}`

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

	t.Run("lists ippools from the API", func(t *testing.T) {
		rec := &requestRecorder{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record(r.URL.Path)
			if r.URL.Path != path {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, ipPoolListJSON)
		}))
		defer srv.Close()

		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL)}
		pools, err := h.getIPPools()
		if err != nil {
			t.Fatalf("getIPPools() unexpected error: %s", err)
		}
		if rec.got() != path {
			t.Errorf("request path = %q, want %q", rec.got(), path)
		}
		if len(pools) != 2 {
			t.Fatalf("got %d pools, want 2", len(pools))
		}
		if pools[0].Spec.NetworkName != "net-a" || pools[0].Spec.IPv4Config.Subnet != "192.168.1.0/24" {
			t.Errorf("unexpected first pool: %+v", pools[0].Spec)
		}
		if pools[1].Spec.NetworkName != "net-b" || pools[1].Spec.IPv4Config.ServerIP != "10.0.0.1" {
			t.Errorf("unexpected second pool: %+v", pools[1].Spec)
		}
	})

	t.Run("API error is wrapped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"boom","reason":"InternalError","code":500}`)
		}))
		defer srv.Close()

		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL)}
		_, err := h.getIPPools()
		if err == nil {
			t.Fatal("getIPPools() expected an error for an API failure")
		}
		if !strings.Contains(err.Error(), "cannot get the IPPoolList") {
			t.Errorf("getIPPools() error = %q, want it wrapped with the list context", err)
		}
	})

	t.Run("missing kubeconfig is wrapped", func(t *testing.T) {
		clearInClusterEnv(t)
		h := &handler{kubeConfigFile: filepath.Join(t.TempDir(), "does-not-exist")}
		_, err := h.getIPPools()
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
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, vmnetcfgListJSON)
		}))
		defer srv.Close()

		h := &handler{kubeConfigFile: writeTestKubeconfig(t, srv.URL)}
		cfgs, err := h.getVmNetCfgs()
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
		_, err := h.getVmNetCfgs()
		if err == nil {
			t.Fatal("getVmNetCfgs() expected an error for an API failure")
		}
		if !strings.Contains(err.Error(), "cannot get the vmnetcfgList") {
			t.Errorf("getVmNetCfgs() error = %q, want it wrapped with the list context", err)
		}
	})
}

func TestHandler_NetworkCleanup(t *testing.T) {
	const cleanupPoolsJSON = `{
  "kind": "IPPoolList",
  "apiVersion": "kubevirtiphelper.k8s.binbash.org/v1",
  "metadata": {},
  "items": [
    {
      "metadata": {"name": "pool-bad"},
      "spec": {
        "networkname": "net-bad",
        "bindinterface": "eth0",
        "ipv4config": {
          "serverip": "not-an-ip",
          "subnet": "not-a-subnet"
        }
      }
    },
    {
      "metadata": {"name": "pool-ok"},
      "spec": {
        "networkname": "net-ok",
        "bindinterface": "",
        "ipv4config": {
          "serverip": "192.168.1.1",
          "subnet": "192.168.1.0/24"
        }
      }
    }
  ]
}`

	t.Run("tolerates unparsable subnets and skips missing interfaces", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, cleanupPoolsJSON)
		}))
		defer srv.Close()

		hook := attachLogCapture(t)
		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, srv.URL),
			namespace:      "testns",
		}
		h.NetworkCleanup()

		if !hook.contains("error while parsing subnet [not-a-subnet]") {
			t.Errorf("expected a log entry about the unparsable subnet, got:\n%s", hook.entriesText())
		}
		// The IPC removal for the next pool must still run: the first pool's
		// subnet error must not abort the loop.
		if !hook.contains("removing the IP4 address [192.168.1.1/24] on nic [] for network [net-ok]") {
			t.Errorf("expected the valid pool to be processed after the bad one, got:\n%s", hook.entriesText())
		}
		// No interface named "" can exist, so removal fails without touching
		// any host interface; that failure is expected and logged at debug.
		if !hook.contains("error while removing IP4 address [192.168.1.1/24] from bind interface []") {
			t.Errorf("expected the debug log for the missing interface, got:\n%s", hook.entriesText())
		}
	})

	t.Run("proceeds when the API is unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()

		hook := attachLogCapture(t)
		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, url),
			namespace:      "testns",
		}
		h.NetworkCleanup() // must not panic

		if !hook.contains("app.NetworkCleanup") {
			t.Errorf("expected an error logged for the unreachable API, got:\n%s", hook.entriesText())
		}
	})

	t.Run("proceeds on an invalid kubeconfig", func(t *testing.T) {
		hook := attachLogCapture(t)
		h := &handler{
			kubeConfigFile: filepath.Join(t.TempDir(), "kubeconfig"),
			namespace:      "testns",
		}
		badFile := h.kubeConfigFile
		if err := os.WriteFile(badFile, []byte("not: [valid"), 0600); err != nil {
			t.Fatalf("writing malformed kubeconfig: %s", err)
		}
		h.NetworkCleanup() // must not panic

		if !hook.contains("app.NetworkCleanup") {
			t.Errorf("expected an error logged for the invalid kubeconfig, got:\n%s", hook.entriesText())
		}
	})
}

func TestHandler_stopDHCPListeners(t *testing.T) {
	t.Run("proceeds when the API is unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()

		hook := attachLogCapture(t)
		h := &handler{
			kubeConfigFile: writeTestKubeconfig(t, url),
			namespace:      "testns",
		}
		h.stopDHCPListeners() // must not panic

		if !hook.contains("app.stopDHCPListeners") {
			t.Errorf("expected an error logged for the unreachable API, got:\n%s", hook.entriesText())
		}
	})
}

// podStore is a tiny in-memory pod API: GET and PUT against
// /api/v1/namespaces/<ns>/pods/<name>.
type podStore struct {
	mu      sync.Mutex
	gets    int
	updates int
	pods    map[string]*corev1.Pod
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
		h.addLeaderPodLabel()

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
		h.addLeaderPodLabel() // must not panic

		if !hook.contains("cannot get current pod object") {
			t.Errorf("expected an error about the pod fetch, got:\n%s", hook.entriesText())
		}
	})

	t.Run("logs and continues when the kubeconfig is invalid", func(t *testing.T) {
		clearInClusterEnv(t)
		hook := attachLogCapture(t)
		h := &handler{kubeConfigFile: filepath.Join(t.TempDir(), "does-not-exist")}
		h.addLeaderPodLabel() // must not panic

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

		if !hook.contains("cannot get current pod object") {
			t.Errorf("expected an error about the pod fetch, got:\n%s", hook.entriesText())
		}
	})
}

func TestHandler_Init(t *testing.T) {
	t.Run("initializes with a valid kubeconfig", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler()) // pod lookup inside Init returns 404, logged only
		defer srv.Close()
		cfg := writeTestKubeconfig(t, srv.URL)
		t.Setenv("KUBECONFIG", cfg)
		clearInClusterEnv(t)

		h := Register()
		h.Init()

		if h.kubeConfigFile != cfg {
			t.Errorf("kubeConfigFile = %q, want %q", h.kubeConfigFile, cfg)
		}
		if h.appStatus != APP_INIT {
			t.Errorf("appStatus = %d, want %d (APP_INIT)", h.appStatus, APP_INIT)
		}
		if h.leaderId == "" {
			t.Error("leaderId is empty after Init")
		}
		if h.lock == nil {
			t.Fatal("lock is nil after Init")
		}
		if h.lock.LeaseMeta.Name != "kubevirt-ip-helper-lock" {
			t.Errorf("lock name = %q, want %q", h.lock.LeaseMeta.Name, "kubevirt-ip-helper-lock")
		}
		if h.lock.LockConfig.Identity != h.leaderId {
			t.Errorf("lock identity = %q, want leader id %q", h.lock.LockConfig.Identity, h.leaderId)
		}
	})

	t.Run("panics when no kubeconfig is available", func(t *testing.T) {
		t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
		clearInClusterEnv(t)

		h := Register()
		recovered := assertPanics(t, func() { h.Init() })
		if !strings.Contains(fmt.Sprint(recovered), "app.handleErr") {
			t.Errorf("panic = %v, want it to contain the handleErr context", recovered)
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
