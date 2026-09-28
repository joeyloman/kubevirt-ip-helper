package util

import (
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
