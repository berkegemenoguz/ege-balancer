package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the counters and histograms the proxy reports into. Values that
// are already tracked elsewhere, such as active connections and backend health,
// are read at scrape time instead of being mirrored here; see poolCollector.
type Metrics struct {
	registry *prometheus.Registry

	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	failures   *prometheus.CounterVec
	rejections *prometheus.CounterVec
	reloads    *prometheus.CounterVec
}

// NewMetrics registers the load balancer's own collectors on a private
// registry, so that the exposed metrics are only the ones defined here.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lb_requests_total",
			Help: "Requests answered to clients, by backend and response status.",
		}, []string{"backend", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "lb_request_duration_seconds",
			Help: "Time to serve a request, measured at the load balancer.",
			// Covers the sub-millisecond answers of a local backend up to the
			// multi-second range where timeouts start to matter.
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"backend"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lb_backend_failures_total",
			Help: "Attempts a backend could not serve, by backend.",
		}, []string{"backend"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lb_rejected_requests_total",
			Help: "Requests refused by the load balancer itself, by reason.",
		}, []string{"reason"}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lb_config_reloads_total",
			Help: "Configuration reloads, by whether they were applied or rejected.",
		}, []string{"result"}),
	}

	m.registry.MustRegister(m.requests, m.duration, m.failures, m.rejections, m.reloads)
	return m
}

// ObserveRequest records a request that a backend answered.
func (m *Metrics) ObserveRequest(backend, status string, took time.Duration) {
	m.requests.WithLabelValues(backend, status).Inc()
	m.duration.WithLabelValues(backend).Observe(took.Seconds())
}

// ObserveBackendFailure records an attempt that a backend could not serve.
func (m *Metrics) ObserveBackendFailure(backend string) {
	m.failures.WithLabelValues(backend).Inc()
}

// ObserveRejection records a request the load balancer refused itself, for
// instance one over the rate limit or with an unusable body.
func (m *Metrics) ObserveRejection(reason string) {
	m.rejections.WithLabelValues(reason).Inc()
}

// ObserveReload records the outcome of a configuration reload, so that an
// operator who sent a signal can see whether it took effect.
func (m *Metrics) ObserveReload(result string) {
	m.reloads.WithLabelValues(result).Inc()
}

// Register adds a collector that reports live values at scrape time.
func (m *Metrics) Register(collector prometheus.Collector) {
	m.registry.MustRegister(collector)
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
