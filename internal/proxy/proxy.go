// Package proxy is the proxy core: it forwards requests to the backend chosen
// by the balancer, and applies the retry, circuit breaker and failure policy
// logic on top of it.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// backendKey addresses the backend chosen for a request inside its context.
type backendKey struct{}

// errRetryable5xx turns a 5xx answer into a failed attempt, so that the retry
// loop moves on to the next backend. It never reaches the client.
var errRetryable5xx = errors.New("backend answered 5xx")

// Core forwards requests to the backends, applying the configured failure
// policy on the way.
type Core struct {
	strategy balancer.LBStrategy
	backends []*balancer.Backend
	checker  health.Checker
	breaker  *breaker
	metrics  *observability.Metrics

	forward *httputil.ReverseProxy
	// attempts is how many backends a single request may be offered to.
	attempts   int
	retryOn5xx bool
	maxBody    int64
}

// New returns the handler that serves proxied traffic, with rate limiting and
// request validation applied in front of it.
func New(
	cfg *config.Config,
	strategy balancer.LBStrategy,
	backends []*balancer.Backend,
	checker health.Checker,
	metrics *observability.Metrics,
) http.Handler {
	core := &Core{
		strategy:   strategy,
		backends:   backends,
		checker:    checker,
		breaker:    newBreaker(cfg.FailurePolicy, cfg.CircuitBreaker),
		metrics:    metrics,
		attempts:   attemptsFor(cfg),
		retryOn5xx: cfg.RetryOn5xx,
		maxBody:    cfg.Limits.MaxRequestBodyBytes,
	}
	core.forward = core.newReverseProxy(cfg.Timeouts)

	return newRateLimiter(cfg.Limits.RateLimitPerIP, metrics).wrap(validateRequest(metrics, core))
}

// attemptsFor translates the failure policy into the number of backends a
// request may be offered to.
func attemptsFor(cfg *config.Config) int {
	if cfg.FailurePolicy == config.RetryNextBackend {
		// The first try plus the configured retries.
		return 1 + cfg.Retry.MaxRetries
	}
	// fail_fast answers on the first failure; circuit_breaker does the same and
	// instead keeps the failing backend out of later selections.
	return 1
}

// newReverseProxy builds the forwarder shared by every attempt. The backend to
// use is carried on the request context, so one instance serves all of them.
func (c *Core) newReverseProxy(timeouts config.Timeouts) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: backendFrom(r.In.Context()).Addr})
			// Replaces any X-Forwarded-For sent by the client, so that a client
			// cannot forge the address the backend sees.
			r.SetXForwarded()
		},
		Transport: transport(timeouts),
		ModifyResponse: func(response *http.Response) error {
			if c.retryOn5xx && response.StatusCode >= http.StatusInternalServerError {
				return errRetryable5xx
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Nothing is written here: the attempt reports the failure and the
			// retry loop decides whether the client sees an error at all.
			if current, ok := w.(*attempt); ok {
				current.err = err
			}
		},
	}
}

// ServeHTTP offers the request to healthy backends until one serves it or the
// configured number of attempts runs out.
func (c *Core) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := c.replayableBody(r)
	if err != nil {
		c.metrics.ObserveRejection("body_too_large")
		http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
		return
	}

	tried := make(map[string]bool, c.attempts)
	for range c.attempts {
		backend, err := c.strategy.Select(c.available(tried))
		if err != nil {
			break
		}
		tried[backend.Addr] = true

		if c.serve(w, r, backend, body) {
			return
		}
	}

	slog.Warn("no backend could serve the request", "method", r.Method, "path", r.URL.Path)
	c.metrics.ObserveRejection("no_backend_available")
	unavailable(w)
}

// serve makes one attempt and reports whether the client was answered.
func (c *Core) serve(w http.ResponseWriter, r *http.Request, backend *balancer.Backend, body []byte) bool {
	// The counter is what least connections balances on, so it must cover the
	// whole request, not just the choice.
	backend.Acquire()
	defer backend.Release()

	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	current := &attempt{ResponseWriter: w, status: http.StatusOK}
	started := time.Now()
	c.forward.ServeHTTP(current, r.WithContext(context.WithValue(r.Context(), backendKey{}, backend)))
	took := time.Since(started)

	if current.err == nil {
		c.checker.ReportSuccess(backend.Addr)
		c.breaker.recordSuccess(backend.Addr)
		c.metrics.ObserveRequest(backend.Addr, strconv.Itoa(current.status), took)
		slog.Debug("request served", "method", r.Method, "path", r.URL.Path,
			"backend", backend.Addr, "status", current.status, "duration", took)
		return true
	}

	c.checker.ReportFailure(backend.Addr)
	c.breaker.recordFailure(backend.Addr)
	c.metrics.ObserveBackendFailure(backend.Addr)
	slog.Warn("backend attempt failed", "method", r.Method, "path", r.URL.Path,
		"backend", backend.Addr, "error", current.err)
	return false
}

// available returns the backends that may serve the request now: healthy, not
// tripped by the circuit breaker, and not already tried for this request.
func (c *Core) available(tried map[string]bool) []*balancer.Backend {
	fit := make([]*balancer.Backend, 0, len(c.backends))
	for _, backend := range c.backends {
		if !tried[backend.Addr] && c.checker.IsHealthy(backend.Addr) && c.breaker.allow(backend.Addr) {
			fit = append(fit, backend)
		}
	}
	return fit
}

// replayableBody buffers the request body when a retry may need to send it
// again. It returns nil when there is no body to replay.
func (c *Core) replayableBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	if r.ContentLength > c.maxBody {
		return nil, errors.New("request body exceeds the configured limit")
	}
	if c.attempts == 1 {
		// Without retries the body is streamed straight through.
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, c.maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxBody {
		return nil, errors.New("request body exceeds the configured limit")
	}
	return body, nil
}

// attempt lets the error handler mark a try as failed without writing anything
// to the client, which keeps the response free for the next backend. It also
// records the status the backend answered with, for the metrics.
type attempt struct {
	http.ResponseWriter
	err    error
	status int
}

// WriteHeader records the status on its way to the client.
func (a *attempt) WriteHeader(status int) {
	a.status = status
	a.ResponseWriter.WriteHeader(status)
}

// Unwrap exposes the underlying writer, so that http.ResponseController can
// still reach the flushing and deadline support of the real connection.
func (a *attempt) Unwrap() http.ResponseWriter {
	return a.ResponseWriter
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
