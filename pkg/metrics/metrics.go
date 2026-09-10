package metrics

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	LabelLogLevel    = "loglevel"
	LabelIPPoolName  = "ippool"
	LabelSubnet      = "subnet"
	LabelNetworkName = "network"
	LabelVMName      = "vm"
	LabelMacAddress  = "mac"
	LabelIPAddress   = "ip"
	LabelStatus      = "status"
)

type MetricsAllocator struct {
	httpServer                      http.Server
	kubevirtiphelperAppLogs         *prometheus.GaugeVec
	kubevirtiphelperIPPoolUsed      *prometheus.GaugeVec
	kubevirtiphelperIPPoolAvailable *prometheus.GaugeVec
	kubevirtiphelperVmNetCfgStatus  *prometheus.GaugeVec
	registry                        *prometheus.Registry
	// healthChecks are registered by the application and evaluated by the
	// /healthz and /ready endpoints: the liveness probe of the pod checks
	// the leader-election freshness through them, so a stale leader is
	// restarted by the kubelet instead of serving dhcp forever
	healthMutex  sync.Mutex
	healthChecks []healthCheck
}

type healthCheck struct {
	name  string
	check func() error
	// readiness-only checks (the application's readiness registration)
	// run for the /ready endpoint only, so a pod which is not yet serving
	// stays not-ready without failing its liveness probe
	readiness bool
}

func NewMetricsAllocator() *MetricsAllocator {
	m := &MetricsAllocator{
		kubevirtiphelperAppLogs: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kubevirtiphelper_app_logs",
				Help: "Important log entries of the application",
			},
			[]string{
				LabelLogLevel,
			},
		),
		kubevirtiphelperIPPoolUsed: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kubevirtiphelper_ippool_used",
				Help: "Amount of IP addresses which are in use",
			},
			[]string{
				LabelIPPoolName,
				LabelSubnet,
				LabelNetworkName,
			},
		),
		kubevirtiphelperIPPoolAvailable: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kubevirtiphelper_ippool_available",
				Help: "Amount of IP addresses which are available",
			},
			[]string{
				LabelIPPoolName,
				LabelSubnet,
				LabelNetworkName,
			},
		),
		kubevirtiphelperVmNetCfgStatus: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kubevirtiphelper_vmnetcfg_status",
				Help: "Status of the vmnetcfg objects",
			},
			[]string{
				LabelVMName,
				LabelNetworkName,
				LabelMacAddress,
				LabelIPAddress,
				LabelStatus,
			},
		),
	}

	m.registry = prometheus.NewRegistry()
	m.registry.MustRegister(m.kubevirtiphelperAppLogs)
	m.registry.MustRegister(m.kubevirtiphelperIPPoolUsed)
	m.registry.MustRegister(m.kubevirtiphelperIPPoolAvailable)
	m.registry.MustRegister(m.kubevirtiphelperVmNetCfgStatus)

	return m
}

func (m *MetricsAllocator) UpdateLogStatus(loglevel string) {
	m.kubevirtiphelperAppLogs.With(prometheus.Labels{
		LabelLogLevel: loglevel,
	}).Inc()
}

func (m *MetricsAllocator) UpdateIPPoolUsed(ippoolName string, subnet string, networkName string, used int) {
	m.kubevirtiphelperIPPoolUsed.With(prometheus.Labels{
		LabelIPPoolName:  ippoolName,
		LabelSubnet:      subnet,
		LabelNetworkName: networkName,
	}).Set(float64(used))
}

func (m *MetricsAllocator) UpdateIPPoolAvailable(ippoolName string, subnet string, networkName string, available int) {
	m.kubevirtiphelperIPPoolAvailable.With(prometheus.Labels{
		LabelIPPoolName:  ippoolName,
		LabelSubnet:      subnet,
		LabelNetworkName: networkName,
	}).Set(float64(available))
}

func (m *MetricsAllocator) DeleteIPPool(ippoolName string, subnet string, networkName string) {
	m.kubevirtiphelperIPPoolUsed.Delete(prometheus.Labels{
		LabelIPPoolName:  ippoolName,
		LabelSubnet:      subnet,
		LabelNetworkName: networkName,
	})

	m.kubevirtiphelperIPPoolAvailable.Delete(prometheus.Labels{
		LabelIPPoolName:  ippoolName,
		LabelSubnet:      subnet,
		LabelNetworkName: networkName,
	})
}

func (m *MetricsAllocator) UpdateVmNetCfgStatus(vmName string, networkName string, macAddr string, ipAddr string, status string) {
	m.kubevirtiphelperVmNetCfgStatus.With(prometheus.Labels{
		LabelVMName:      vmName,
		LabelNetworkName: networkName,
		LabelMacAddress:  macAddr,
		LabelIPAddress:   ipAddr,
		LabelStatus:      status,
	}).Set(float64(1))
}

func (m *MetricsAllocator) DeleteVmNetCfgStatus(vmName string) {
	var vmnetCfgMetrics []prometheus.Labels
	var labelFound bool

	// gather all metrics so we make sure we delete all of them
	gatherer := prometheus.Gatherer(m.registry)
	mfs, err := gatherer.Gather()
	if err != nil {
		log.Errorf("(metrics.DeleteVmNetCfgStatus) error while gathering metrics for vm [%s]: %s",
			vmName, err.Error())

		return
	}
	for _, mf := range mfs {
		if mf.GetName() == "kubevirtiphelper_vmnetcfg_status" {
			for _, m := range mf.GetMetric() {
				labelFound = false
				pLabel := make(map[string]string)
				for _, l := range m.GetLabel() {
					pLabel[l.GetName()] = l.GetValue()
					if l.GetName() == LabelVMName && l.GetValue() == vmName {
						labelFound = true
					}
				}
				if labelFound {
					vmnetCfgMetrics = append(vmnetCfgMetrics, pLabel)
				}
			}
		}
	}

	// delete the metrics which contain the vm name
	for _, pl := range vmnetCfgMetrics {
		m.kubevirtiphelperVmNetCfgStatus.Delete(pl)
	}
}

func (m *MetricsAllocator) Run() {
	log.Infof("(metrics.Run) starting the Metrics service")

	var metricsPort int

	metricsPort, err := strconv.Atoi(os.Getenv("METRICS_PORT"))
	if err != nil {
		metricsPort = 8080
	}
	listenAddress := fmt.Sprintf(":%d", metricsPort)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry}))
	mux.HandleFunc("/healthz", m.healthzHandler)
	mux.HandleFunc("/ready", m.readyHandler)

	m.httpServer = http.Server{
		Addr:    listenAddress,
		Handler: mux,
	}

	log.Infof("(metrics.Run) %s", m.httpServer.ListenAndServe())
}

// SetHealthCheck registers a named check evaluated by the /healthz and
// /ready endpoints: a check error turns the endpoint into a 503, so the
// liveness probe of the pod restarts it. checks are registered by the
// application before the server starts.
func (m *MetricsAllocator) SetHealthCheck(name string, check func() error) {
	m.healthMutex.Lock()
	defer m.healthMutex.Unlock()

	m.healthChecks = append(m.healthChecks, healthCheck{name: name, check: check})
}

// SetReadinessCheck registers a check evaluated by the /ready endpoint
// only: a pod which has not started serving (or never acquired the
// leadership) stays not-ready without failing its liveness probe.
func (m *MetricsAllocator) SetReadinessCheck(name string, check func() error) {
	m.healthMutex.Lock()
	defer m.healthMutex.Unlock()

	m.healthChecks = append(m.healthChecks, healthCheck{name: name, check: check, readiness: true})
}

// checksFor returns the checks of an endpoint: /healthz runs the liveness
// checks, /ready runs liveness plus readiness.
func (m *MetricsAllocator) checksFor(includeReadiness bool) []healthCheck {
	m.healthMutex.Lock()
	defer m.healthMutex.Unlock()

	var checks []healthCheck
	for _, hc := range m.healthChecks {
		if includeReadiness || !hc.readiness {
			checks = append(checks, hc)
		}
	}

	return checks
}

// healthzHandler reports 200 only when every registered liveness check
// passes.
func (m *MetricsAllocator) healthzHandler(w http.ResponseWriter, r *http.Request) {
	m.evalChecks(w, m.checksFor(false))
}

// readyHandler reports 200 only when every liveness and readiness check
// passes.
func (m *MetricsAllocator) readyHandler(w http.ResponseWriter, r *http.Request) {
	m.evalChecks(w, m.checksFor(true))
}

func (m *MetricsAllocator) evalChecks(w http.ResponseWriter, checks []healthCheck) {
	if len(checks) == 0 {
		http.Error(w, "no health checks registered", http.StatusServiceUnavailable)

		return
	}

	for _, hc := range checks {
		if err := hc.check(); err != nil {
			http.Error(w, fmt.Sprintf("%s: %s", hc.name, err.Error()), http.StatusServiceUnavailable)

			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (m *MetricsAllocator) Stop() {
	log.Infof("(metrics.Stop) stopping the Metrics service")
	if err := m.httpServer.Shutdown(context.Background()); err != nil {
		log.Errorf("(metrics.Stop) error while stopping the Metrics service: %s", err.Error())
	}
}

func New() *MetricsAllocator {
	return NewMetricsAllocator()
}
