package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

func TestIPAMUsageHandlerRequiresServingEra(t *testing.T) {
	t.Run("standby", func(t *testing.T) {
		h := Register()
		rec := httptest.NewRecorder()

		h.ipamUsageHandler(rec, httptest.NewRequest(http.MethodGet, "/ipam", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})

	t.Run("initializing era", func(t *testing.T) {
		h := Register()
		status := new(atomic.Int32)
		status.Store(APP_INIT)
		h.era.Store(&eraState{appStatus: status, ipam: ipam.New()})
		rec := httptest.NewRecorder()

		h.ipamUsageHandler(rec, httptest.NewRequest(http.MethodGet, "/ipam", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

func TestIPAMUsageHandlerServesAllocationSnapshot(t *testing.T) {
	allocator := ipam.New()
	if err := allocator.NewSubnet("net-a", "192.168.60.0/30", "192.168.60.1", "192.168.60.2"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}
	if _, err := allocator.GetIP("net-a", "192.168.60.2"); err != nil {
		t.Fatalf("GetIP: %v", err)
	}

	status := new(atomic.Int32)
	status.Store(APP_RUNNING)
	h := Register()
	h.era.Store(&eraState{appStatus: status, ipam: allocator})
	rec := httptest.NewRecorder()

	h.ipamUsageHandler(rec, httptest.NewRequest(http.MethodGet, "/ipam", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got ipamUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(got.Networks) != 1 {
		t.Fatalf("networks = %d, want 1", len(got.Networks))
	}
	network := got.Networks[0]
	if network.Name != "net-a" || network.Total != 2 || network.Used != 1 || network.Available != 1 {
		t.Errorf("network = %+v, want net-a with total 2, used 1, available 1", network)
	}
	if len(network.Allocated) != 1 || network.Allocated[0] != "192.168.60.2" {
		t.Errorf("allocated = %v, want [192.168.60.2]", network.Allocated)
	}
}

func TestIPAMUsageHandlerRejectsNonGET(t *testing.T) {
	h := Register()
	rec := httptest.NewRecorder()

	h.ipamUsageHandler(rec, httptest.NewRequest(http.MethodPost, "/ipam", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want %q", allow, http.MethodGet)
	}
}

// TestIPAMEndpointOverMetricsServer exercises the real HTTP wiring: the
// route is registered on the process-global metrics server and answers a
// standby with 503, then serves the allocation snapshot once the era is
// running.
func TestIPAMEndpointOverMetricsServer(t *testing.T) {
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
	h.metrics = metrics.New()
	h.metrics.HandleFunc("/ipam", h.ipamUsageHandler)
	go h.metrics.Run()
	defer h.metrics.Stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/ipam", metricsPort)
	get := func() *http.Response {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err := http.Get(url)
			if err == nil {
				return resp
			}
			if time.Now().After(deadline) {
				t.Fatalf("the metrics server never came up: %s", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	resp := get()
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("standby status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	allocator := ipam.New()
	if err := allocator.NewSubnet("net-a", "192.168.70.0/30", "192.168.70.1", "192.168.70.2"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}
	status := new(atomic.Int32)
	status.Store(APP_RUNNING)
	h.era.Store(&eraState{appStatus: status, ipam: allocator})

	resp = get()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("serving status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var got ipamUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(got.Networks) != 1 || got.Networks[0].Name != "net-a" {
		t.Errorf("networks = %+v, want net-a", got.Networks)
	}
}
