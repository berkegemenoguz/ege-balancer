package observability

import (
	"io"
	"net/http"
	"sync/atomic"
)

// Probes answers the two questions an orchestrator asks of the balancer itself:
// is the process alive, and should it be sent traffic.
//
// The answers are kept apart on purpose. A balancer whose backends are all down
// is still a working balancer: restarting it would not bring a backend back, so
// only readiness depends on the pool.
type Probes struct {
	pool     *Pool
	draining atomic.Bool
}

// NewProbes returns the probes of a balancer serving the given pool.
func NewProbes(pool *Pool) *Probes {
	return &Probes{pool: pool}
}

// Drain makes the balancer report itself not ready from now on, so that whatever
// routes traffic to it stops while the requests already in flight finish.
func (p *Probes) Drain() {
	p.draining.Store(true)
}

// LiveHandler serves /healthz, which answers 200 for as long as the process can
// answer at all.
func (p *Probes) LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, "ok")
	})
}

// ReadyHandler serves /readyz, which answers 200 while the balancer has a
// healthy backend to send a request to and is not shutting down, and 503
// otherwise.
func (p *Probes) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case p.draining.Load():
			answer(w, http.StatusServiceUnavailable, "shutting down")
		case !p.anyHealthy():
			answer(w, http.StatusServiceUnavailable, "no healthy backend")
		default:
			answer(w, http.StatusOK, "ok")
		}
	})
}

// anyHealthy reports whether at least one backend in the pool is healthy, by
// the same health checker /status reads.
func (p *Probes) anyHealthy() bool {
	_, backends := p.pool.snapshot()
	for _, backend := range backends {
		if p.pool.checker.IsHealthy(backend.Addr) {
			return true
		}
	}
	return false
}

// answer writes a short plain text response.
func answer(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, message+"\n")
}
