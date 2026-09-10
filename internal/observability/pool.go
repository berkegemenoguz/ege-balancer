package observability

import (
	"encoding/json"
	"net/http"
	"net/http/pprof"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
)

// Pool reports the live state of the backend pool, both as Prometheus metrics
// and as the JSON served by /status.
//
// The values are read from the balancer and the health checker when they are
// asked for, so there is no second copy of the state to keep in step.
type Pool struct {
	// mu guards what a reload replaces; Collect and the status handler read it.
	mu        sync.RWMutex
	algorithm string
	backends  []*balancer.Backend
	checker   health.Checker

	// reloads counts the configurations applied since start, so /status can
	// answer "did my reload take effect".
	reloads atomic.Int64

	activeDesc  *prometheus.Desc
	healthyDesc *prometheus.Desc
}

// NewPool returns a collector over the given backend pool.
func NewPool(algorithm string, backends []*balancer.Backend, checker health.Checker) *Pool {
	return &Pool{
		algorithm: algorithm,
		backends:  backends,
		checker:   checker,
		activeDesc: prometheus.NewDesc("lb_backend_active_connections",
			"Requests a backend is serving right now.", []string{"backend"}, nil),
		healthyDesc: prometheus.NewDesc("lb_backend_healthy",
			"Whether a backend is in the pool: 1 healthy, 0 unhealthy.", []string{"backend"}, nil),
	}
}

// Reload points the collector at the pool a reload produced.
func (p *Pool) Reload(algorithm string, backends []*balancer.Backend) {
	p.mu.Lock()
	p.algorithm, p.backends = algorithm, backends
	p.mu.Unlock()

	p.reloads.Add(1)
}

// snapshot returns what to report, without holding the lock while reporting.
func (p *Pool) snapshot() (string, []*balancer.Backend) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.algorithm, p.backends
}

// Describe sends the descriptors of the metrics this collector reports.
func (p *Pool) Describe(descs chan<- *prometheus.Desc) {
	descs <- p.activeDesc
	descs <- p.healthyDesc
}

// Collect reads the current state of every backend.
func (p *Pool) Collect(metrics chan<- prometheus.Metric) {
	_, backends := p.snapshot()
	for _, backend := range backends {
		metrics <- prometheus.MustNewConstMetric(p.activeDesc, prometheus.GaugeValue,
			float64(backend.ActiveConnections()), backend.Addr)

		healthy := 0.0
		if p.checker.IsHealthy(backend.Addr) {
			healthy = 1
		}
		metrics <- prometheus.MustNewConstMetric(p.healthyDesc, prometheus.GaugeValue,
			healthy, backend.Addr)
	}
}

// status is the document served by /status.
type status struct {
	Algorithm string          `json:"algorithm"`
	Reloads   int64           `json:"reloads"`
	Healthy   int             `json:"healthy_backends"`
	Total     int             `json:"total_backends"`
	Backends  []backendStatus `json:"backends"`
}

// backendStatus is one backend's entry in the status document.
type backendStatus struct {
	Addr              string `json:"addr"`
	Weight            int    `json:"weight"`
	Healthy           bool   `json:"healthy"`
	ActiveConnections int64  `json:"active_connections"`
}

// StatusHandler serves a JSON summary of the pool, for a person looking at the
// balancer rather than for a metrics scraper.
func (p *Pool) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		algorithm, backends := p.snapshot()
		current := status{
			Algorithm: algorithm,
			Reloads:   p.reloads.Load(),
			Total:     len(backends),
			Backends:  make([]backendStatus, 0, len(backends)),
		}

		for _, backend := range backends {
			healthy := p.checker.IsHealthy(backend.Addr)
			if healthy {
				current.Healthy++
			}
			current.Backends = append(current.Backends, backendStatus{
				Addr:              backend.Addr,
				Weight:            backend.Weight,
				Healthy:           healthy,
				ActiveConnections: backend.ActiveConnections(),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(current); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	})
}

// Endpoints returns the handler serving /metrics and /status, and the Go
// profiling endpoints under /debug/pprof when they are enabled.
func Endpoints(metrics *Metrics, pool *Pool, withPprof bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/status", pool.StatusHandler())

	if withPprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}
