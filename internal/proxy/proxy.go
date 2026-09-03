// Package proxy is the proxy core: it forwards requests to the backend chosen
// by the balancer, and applies the retry, circuit breaker and failure policy
// logic on top of it.
package proxy

import (
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// New returns a handler that forwards every request to target.
//
// Backend selection is not part of this handler yet: until the load balancing
// engine exists, all traffic goes to the single backend it is built with.
func New(target config.Backend, timeouts config.Timeouts) http.Handler {
	upstream := &url.URL{Scheme: "http", Host: target.Addr}

	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			// Replaces any X-Forwarded-For sent by the client, so that a client
			// cannot forge the address the backend sees.
			r.SetXForwarded()
		},
		Transport: transport(timeouts),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy: %s %s to %s failed: %v", r.Method, r.URL.Path, upstream.Host, err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		},
	}
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
