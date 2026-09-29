package util

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// the informer client must not carry the one-shot client timeout: it
// would tear the watch connection down every time it expires and an
// initial list slower than the timeout would never complete
func TestWatchRestConfigStripsTheClientTimeout(t *testing.T) {
	config := &rest.Config{Host: "https://example.com", Timeout: 30 * time.Second}

	watchConfig := WatchRestConfig(config)

	if watchConfig.Timeout != 0 {
		t.Errorf("watch config timeout = %v, want it stripped", watchConfig.Timeout)
	}
	if watchConfig.Host != config.Host {
		t.Errorf("watch config host = %q, want %q preserved", watchConfig.Host, config.Host)
	}
	// the source config stays untouched: the one-shot clients keep their
	// bound
	if config.Timeout != 30*time.Second {
		t.Errorf("source config timeout = %v, want it untouched", config.Timeout)
	}
}

// the one-shot clients built from GetKubeConfig must carry a request
// bound (F08): without it a tcp blackhole against the api hangs the
// webhook's list, csr, secret and webhook-configuration calls forever
func TestGetKubeConfigBoundsOneShotRequests(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://example.com
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`
	if err := os.WriteFile(kubeconfig, []byte(content), 0600); err != nil {
		t.Fatalf("writing test kubeconfig: %s", err)
	}

	config, err := GetKubeConfig(kubeconfig, "")
	if err != nil {
		t.Fatalf("GetKubeConfig: %v", err)
	}
	if config.Timeout != 30*time.Second {
		t.Errorf("config timeout = %v, want the 30s one-shot bound", config.Timeout)
	}
}
