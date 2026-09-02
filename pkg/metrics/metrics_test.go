package metrics

import (
	"bytes"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
)

func gatherFamily(t *testing.T, m *MetricsAllocator, name string) *dto.MetricFamily {
	t.Helper()
	mfs, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("metric family %s not found", name)
	return nil
}

func metricValue(t *testing.T, mf *dto.MetricFamily, labels map[string]string) (float64, bool) {
	t.Helper()
	for _, m := range mf.GetMetric() {
		got := make(map[string]string)
		for _, lp := range m.GetLabel() {
			got[lp.GetName()] = lp.GetValue()
		}
		matches := true
		for k, v := range labels {
			if got[k] != v {
				matches = false
				break
			}
		}
		if matches {
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func metricCount(t *testing.T, mf *dto.MetricFamily) int {
	t.Helper()
	return len(mf.GetMetric())
}

func TestMetricsAppLogStatus(t *testing.T) {
	m := NewMetricsAllocator()
	m.UpdateLogStatus("info")
	m.UpdateLogStatus("info")
	m.UpdateLogStatus("error")

	fam := gatherFamily(t, m, "kubevirtiphelper_app_logs")
	if v, ok := metricValue(t, fam, map[string]string{LabelLogLevel: "info"}); !ok || v != 2 {
		t.Errorf("info count = %v (found=%v), want 2", v, ok)
	}
	if v, ok := metricValue(t, fam, map[string]string{LabelLogLevel: "error"}); !ok || v != 1 {
		t.Errorf("error count = %v (found=%v), want 1", v, ok)
	}
}

func TestMetricsIPPoolUsedAvailable(t *testing.T) {
	m := NewMetricsAllocator()
	m.UpdateIPPoolUsed("pool-a", "10.0.0.0/24", "net-a", 12)
	m.UpdateIPPoolAvailable("pool-a", "10.0.0.0/24", "net-a", 240)

	labels := map[string]string{
		LabelIPPoolName:  "pool-a",
		LabelSubnet:      "10.0.0.0/24",
		LabelNetworkName: "net-a",
	}

	fam := gatherFamily(t, m, "kubevirtiphelper_ippool_used")
	if v, ok := metricValue(t, fam, labels); !ok || v != 12 {
		t.Errorf("used = %v (found=%v), want 12", v, ok)
	}
	// Updating the same series replaces its value.
	m.UpdateIPPoolUsed("pool-a", "10.0.0.0/24", "net-a", 15)
	fam = gatherFamily(t, m, "kubevirtiphelper_ippool_used")
	if v, ok := metricValue(t, fam, labels); !ok || v != 15 {
		t.Errorf("used after update = %v (found=%v), want 15", v, ok)
	}

	availFam := gatherFamily(t, m, "kubevirtiphelper_ippool_available")
	if v, ok := metricValue(t, availFam, labels); !ok || v != 240 {
		t.Errorf("available = %v (found=%v), want 240", v, ok)
	}
}

func TestMetricsDeleteIPPool(t *testing.T) {
	m := NewMetricsAllocator()
	m.UpdateIPPoolUsed("pool-a", "10.0.0.0/24", "net-a", 12)
	m.UpdateIPPoolAvailable("pool-a", "10.0.0.0/24", "net-a", 240)
	m.UpdateIPPoolUsed("pool-b", "10.0.1.0/24", "net-b", 3)
	m.UpdateIPPoolAvailable("pool-b", "10.0.1.0/24", "net-b", 250)

	labelsA := map[string]string{
		LabelIPPoolName:  "pool-a",
		LabelSubnet:      "10.0.0.0/24",
		LabelNetworkName: "net-a",
	}
	labelsB := map[string]string{
		LabelIPPoolName:  "pool-b",
		LabelSubnet:      "10.0.1.0/24",
		LabelNetworkName: "net-b",
	}

	m.DeleteIPPool("pool-a", "10.0.0.0/24", "net-a")

	usedFam := gatherFamily(t, m, "kubevirtiphelper_ippool_used")
	if _, ok := metricValue(t, usedFam, labelsA); ok {
		t.Error("used series for pool-a still present after delete")
	}
	if v, ok := metricValue(t, usedFam, labelsB); !ok || v != 3 {
		t.Errorf("used series for pool-b lost after delete of pool-a: %v (found=%v)", v, ok)
	}
	availFam := gatherFamily(t, m, "kubevirtiphelper_ippool_available")
	if _, ok := metricValue(t, availFam, labelsA); ok {
		t.Error("available series for pool-a still present after delete")
	}

	// Deleting a pool with no series is a no-op.
	m.DeleteIPPool("ghost", "10.9.9.0/24", "net-ghost")
	usedFam = gatherFamily(t, m, "kubevirtiphelper_ippool_used")
	if v, ok := metricValue(t, usedFam, labelsB); !ok || v != 3 {
		t.Errorf("used series for pool-b lost after ghost delete: %v (found=%v)", v, ok)
	}
}

func TestMetricsVmNetCfgStatusLifecycle(t *testing.T) {
	m := NewMetricsAllocator()
	labels := func(vm, netname, mac, ip, status string) map[string]string {
		return map[string]string{
			LabelVMName:      vm,
			LabelNetworkName: netname,
			LabelMacAddress:  mac,
			LabelIPAddress:   ip,
			LabelStatus:      status,
		}
	}
	m.UpdateVmNetCfgStatus("vm-1", "net-a", "aa:bb:cc:dd:ee:01", "10.0.0.10", "Ready")
	m.UpdateVmNetCfgStatus("vm-1", "net-b", "aa:bb:cc:dd:ee:02", "10.0.0.11", "Ready")
	m.UpdateVmNetCfgStatus("vm-2", "net-a", "aa:bb:cc:dd:ee:03", "10.0.0.12", "Ready")

	fam := gatherFamily(t, m, "kubevirtiphelper_vmnetcfg_status")
	if _, ok := metricValue(t, fam, labels("vm-1", "net-a", "aa:bb:cc:dd:ee:01", "10.0.0.10", "Ready")); !ok {
		t.Error("vm-1 net-a series missing")
	}
	if got := metricCount(t, fam); got != 3 {
		t.Errorf("series count = %d, want 3", got)
	}

	// Deleting a vm removes every series carrying that vm label.
	m.DeleteVmNetCfgStatus("vm-1")

	fam = gatherFamily(t, m, "kubevirtiphelper_vmnetcfg_status")
	if got := metricCount(t, fam); got != 1 {
		t.Errorf("series after delete = %d, want 1", got)
	}
	if _, ok := metricValue(t, fam, labels("vm-1", "net-a", "aa:bb:cc:dd:ee:01", "10.0.0.10", "Ready")); ok {
		t.Error("vm-1 net-a series still present after delete")
	}
	if _, ok := metricValue(t, fam, labels("vm-1", "net-b", "aa:bb:cc:dd:ee:02", "10.0.0.11", "Ready")); ok {
		t.Error("vm-1 net-b series still present after delete")
	}
	if _, ok := metricValue(t, fam, labels("vm-2", "net-a", "aa:bb:cc:dd:ee:03", "10.0.0.12", "Ready")); !ok {
		t.Error("vm-2 series lost after deleting vm-1")
	}

	// Deleting a vm with no series is a no-op.
	m.DeleteVmNetCfgStatus("vm-ghost")
	fam = gatherFamily(t, m, "kubevirtiphelper_vmnetcfg_status")
	if got := metricCount(t, fam); got != 1 {
		t.Errorf("series after ghost delete = %d, want 1", got)
	}
}

type failingCollector struct{}

func (failingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("kubevirtiphelper_failing_collector", "always fails", nil, nil)
}

func (failingCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(
		prometheus.NewDesc("kubevirtiphelper_failing_collector", "always fails", nil, nil),
		errors.New("injected collect failure"),
	)
}

func TestMetricsNewAlias(t *testing.T) {
	m := New()
	m.UpdateLogStatus("info")

	fam := gatherFamily(t, m, "kubevirtiphelper_app_logs")
	if v, ok := metricValue(t, fam, map[string]string{LabelLogLevel: "info"}); !ok || v != 1 {
		t.Errorf("info count = %v (found=%v), want 1", v, ok)
	}
}

func TestMetricsRunWithOccupiedPortReturnsAndStopSucceeds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving ephemeral port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	t.Setenv("METRICS_PORT", strconv.Itoa(port))

	var buf bytes.Buffer
	origOut := log.StandardLogger().Out
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	m := NewMetricsAllocator()
	m.Run()
	m.Stop()

	out := buf.String()
	if !strings.Contains(out, "starting the Metrics service") {
		t.Errorf("Run log missing start message: %s", out)
	}
	if !strings.Contains(out, "address already in use") {
		t.Errorf("Run log missing bind failure: %s", out)
	}
	if !strings.Contains(out, "stopping the Metrics service") {
		t.Errorf("Run log missing stop message: %s", out)
	}
}

func TestMetricsDeleteVmNetCfgStatusAbortsOnGatherError(t *testing.T) {
	m := NewMetricsAllocator()
	m.UpdateVmNetCfgStatus("vm-1", "net-a", "aa:bb:cc:dd:ee:01", "10.0.0.10", "Ready")

	m.registry.MustRegister(failingCollector{})

	var buf bytes.Buffer
	origOut := log.StandardLogger().Out
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	m.DeleteVmNetCfgStatus("vm-1")

	out := buf.String()
	if !strings.Contains(out, "(metrics.DeleteVmNetCfgStatus) error while gathering metrics for vm [vm-1]") {
		t.Errorf("delete log missing gather error: %s", out)
	}

	mfs, err := m.registry.Gather()
	if err == nil {
		t.Fatal("expected gather error from failing collector")
	}
	// The failed gather aborts the delete: the vm series must still exist.
	var vm int
	for _, mf := range mfs {
		if mf.GetName() != "kubevirtiphelper_vmnetcfg_status" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == LabelVMName && lp.GetValue() == "vm-1" {
					vm++
				}
			}
		}
	}
	if vm == 0 {
		t.Error("vm-1 series was deleted despite the gather error")
	}
}
