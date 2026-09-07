// Package proxy is the proxy core: it forwards requests to the backend chosen
// by the balancer, and applies the retry, circuit breaker and failure policy
// logic on top of it.
package proxy

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
)

// backendKey addresses the backend chosen for a request inside its context.
type backendKey struct{}

// New returns a handler that forwards each request to a healthy backend chosen
// by strategy, and reports the outcome back to checker so that a backend which
// fails real traffic is taken out without waiting for the next probe.
func New(
	strategy balancer.LBStrategy,
	backends []*balancer.Backend,
	checker health.Checker,
	timeouts config.Timeouts,
) http.Handler {
	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			backend := backendFrom(r.In.Context())
			r.SetURL(&url.URL{Scheme: "http", Host: backend.Addr})
			// Replaces any X-Forwarded-For sent by the client, so that a client
			// cannot forge the address the backend sees.
			r.SetXForwarded()
		},
		Transport: transport(timeouts),
		ModifyResponse: func(response *http.Response) error {
			checker.ReportSuccess(backendFrom(response.Request.Context()).Addr)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			checker.ReportFailure(backendFrom(r.Context()).Addr)
			log.Printf("proxy: %s %s to %s failed: %v", r.Method, r.URL.Path, r.URL.Host, err)
			unavailable(w)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend, err := strategy.Select(healthy(backends, checker))
		if err != nil {
			log.Printf("proxy: %s %s rejected: %v", r.Method, r.URL.Path, err)
			unavailable(w)
			return
		}

		// The counter is what least connections balances on, so it must cover
		// the whole request, not just the choice.
		backend.Acquire()
		defer backend.Release()

		reverseProxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), backendKey{}, backend)))
	})
}

// healthy returns the backends that are currently fit to serve traffic.
func healthy(backends []*balancer.Backend, checker health.Checker) []*balancer.Backend {
	fit := make([]*balancer.Backend, 0, len(backends))
	for _, backend := range backends {
		if checker.IsHealthy(backend.Addr) {
			fit = append(fit, backend)
		}
	}
	return fit
}

// backendFrom returns the backend the request was routed to.
func backendFrom(ctx context.Context) *balancer.Backend {
	backend, _ := ctx.Value(backendKey{}).(*balancer.Backend)
	return backend
}

// unavailable tells the client that no backend served the request and that
// retrying shortly is worthwhile.
func unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
}

// transport applies the configured connect timeout to every upstream dial.
func transport(timeouts config.Timeouts) http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = (&net.Dialer{
		Timeout:   time.Duration(timeouts.ConnectTimeout),
		KeepAlive: 30 * time.Second,
	}).DialContext
	return base
}
