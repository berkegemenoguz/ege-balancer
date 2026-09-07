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
)

// backendKey addresses the backend chosen for a request inside its context.
type backendKey struct{}

// New returns a handler that asks strategy which backend serves each request
// and forwards it there.
func New(strategy balancer.LBStrategy, backends []*balancer.Backend, timeouts config.Timeouts) http.Handler {
	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			backend, _ := r.In.Context().Value(backendKey{}).(*balancer.Backend)
			r.SetURL(&url.URL{Scheme: "http", Host: backend.Addr})
			// Replaces any X-Forwarded-For sent by the client, so that a client
			// cannot forge the address the backend sees.
			r.SetXForwarded()
		},
		Transport:    transport(timeouts),
		ErrorHandler: handleBackendError,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend, err := strategy.Select(backends)
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

// handleBackendError answers a request the chosen backend could not serve.
func handleBackendError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("proxy: %s %s to %s failed: %v", r.Method, r.URL.Path, r.URL.Host, err)
	unavailable(w)
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
