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
	"sync/atomic"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// backendKey addresses the backend chosen for a request inside its context,
// and attemptKey the attempt being made with it.
type (
	backendKey struct{}
	attemptKey struct{}
)

// errRetryable5xx turns a 5xx answer into a failed attempt, so that the retry
// loop moves on to the next backend. It never reaches the client.
var errRetryable5xx = errors.New("backend answered 5xx")

// Core forwards requests to the backends, applying the configured failure
// policy on the way.
type Core struct {
	checker health.Checker
	metrics *observability.Metrics
	forward *httputil.ReverseProxy

	// current is replaced wholesale on a reload. A request reads it once and
	// works from that snapshot, so a reload can never leave one request using
	// the new backend pool with the old failure policy.
	current atomic.Pointer[settings]
}

// settings is everything about forwarding that a reload can change.
type settings struct {
	strategy balancer.LBStrategy
	backends []*balancer.Backend
	breaker  *breaker
	budget   *retryBudget
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
) *Handler {
	core := &Core{checker: checker, metrics: metrics}
	core.current.Store(settingsFor(cfg, strategy, backends))
	core.forward = core.newReverseProxy(cfg, len(backends))

	limiter := newRateLimiter(cfg.Limits.RateLimitPerIP, metrics)
	return &Handler{
		core:    core,
		limiter: limiter,
		// The identifier is assigned outermost, so that a request refused by the
		// rate limiter or the validator can be traced like any other.
		serve: withRequestID(limiter.wrap(validateRequest(metrics, core))),
	}
}

// Handler is the served chain: the request identifier, rate limiting, request
// validation, then the proxy core. It is the type a reload is applied to.
type Handler struct {
	core    *Core
	limiter *rateLimiter
	serve   http.Handler
}

// ServeHTTP runs a request through the chain.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serve.ServeHTTP(w, r)
}

// Reload applies a new configuration to the running handler. The backend pool
// is rebuilt from the backends the caller merged, so backends that survive the
// change keep their counters.
func (h *Handler) Reload(cfg *config.Config, strategy balancer.LBStrategy, backends []*balancer.Backend) {
	h.limiter.setRate(cfg.Limits.RateLimitPerIP)
	h.core.current.Store(settingsFor(cfg, strategy, backends))
}

// Strategy is the strategy currently in use, so that a reload can keep it when
// the algorithm has not changed.
func (h *Handler) Strategy() balancer.LBStrategy {
	return h.core.current.Load().strategy
}

// settingsFor snapshots the forwarding settings described by cfg.
func settingsFor(cfg *config.Config, strategy balancer.LBStrategy, backends []*balancer.Backend) *settings {
	return &settings{
		strategy:   strategy,
		backends:   backends,
		breaker:    newBreaker(cfg.FailurePolicy, cfg.CircuitBreaker),
		budget:     newRetryBudget(cfg.Retry),
		attempts:   attemptsFor(cfg),
		retryOn5xx: cfg.RetryOn5xx,
		maxBody:    cfg.Limits.MaxRequestBodyBytes,
	}
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
func (c *Core) newReverseProxy(cfg *config.Config, backendCount int) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: backendFrom(r.In.Context()).Addr})
			// Replaces any X-Forwarded-For sent by the client, so that a client
			// cannot forge the address the backend sees.
			r.SetXForwarded()
			// The backend logs the same identifier as the balancer, which is what
			// makes one request traceable across both.
			r.Out.Header.Set(requestIDHeader, requestIDFrom(r.In.Context()))
		},
		Transport:  transport(cfg, backendCount),
		BufferPool: newBufferPool(),
		ModifyResponse: func(response *http.Response) error {
			if c.current.Load().retryOn5xx && response.StatusCode >= http.StatusInternalServerError {
				return errRetryable5xx
			}
			if response.Request != nil {
				if current := attemptFrom(response.Request.Context()); current != nil {
					response.Body = &watchedBody{ReadCloser: response.Body, attempt: current}
				}
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

// ServeHTTP offers the request to healthy backends until one serves it, the
// configured number of attempts runs out, or the retry budget refuses a retry.
func (c *Core) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// One snapshot for the whole request: a reload part way through must not
	// change the rules this request is being judged by.
	active := c.current.Load()

	body, err := c.replayableBody(r, active)
	if err != nil {
		c.metrics.ObserveRejection("body_too_large")
		http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
		return
	}

	active.budget.requestStarted()
	defer active.budget.requestFinished()

	tried := make(map[string]bool, active.attempts)
	for n := range active.attempts {
		backend, err := active.strategy.Select(c.available(active, tried))
		if err != nil {
			break
		}
		tried[backend.Addr] = true

		retry := n > 0
		if retry {
			if !active.budget.tryRetry() {
				slog.Warn("retry budget exhausted, not retrying", "method", r.Method, "path", r.URL.Path,
					"request_id", requestIDFrom(r.Context()))
				c.metrics.ObserveRejection("retry_budget_exhausted")
				unavailable(w)
				return
			}
			c.metrics.ObserveRetry()
		}

		served, attemptErr := c.try(w, r, active, backend, body, retry)
		if served {
			return
		}

		// Another attempt would be made, but the request may already have been
		// carried out by the backend that just failed.
		if n+1 < active.attempts && !retryable(r.Method, attemptErr) {
			slog.Warn("not retrying a request the backend may have carried out",
				"method", r.Method, "path", r.URL.Path, "backend", backend.Addr,
				"request_id", requestIDFrom(r.Context()))
			c.metrics.ObserveRejection("not_retryable")
			unavailable(w)
			return
		}
	}

	slog.Warn("no backend could serve the request", "method", r.Method, "path", r.URL.Path,
		"request_id", requestIDFrom(r.Context()))
	c.metrics.ObserveRejection("no_backend_available")
	unavailable(w)
}

// try makes one attempt and gives back the retry budget it held. The release is
// deferred because an answer that fails part way through makes the forwarder
// abort by panicking, and a release written after the call would be skipped:
// each such retry would then hold its share of the budget for good, until
// enough had leaked to refuse every retry until the next reload.
func (c *Core) try(w http.ResponseWriter, r *http.Request, active *settings, backend *balancer.Backend, body []byte, retry bool) (bool, error) {
	if retry {
		defer active.budget.retryFinished()
	}
	return c.serve(w, r, active, backend, body)
}

// serve makes one attempt and reports whether the client was answered, and the
// error that failed the attempt if it was not.
func (c *Core) serve(w http.ResponseWriter, r *http.Request, active *settings, backend *balancer.Backend, body []byte) (bool, error) {
	// The counter is what least connections balances on, so it must cover the
	// whole request, not just the choice.
	backend.Acquire()
	defer backend.Release()

	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	current := &attempt{ResponseWriter: w, status: http.StatusOK}
	ctx := context.WithValue(r.Context(), backendKey{}, backend)
	ctx = context.WithValue(ctx, attemptKey{}, current)

	// An answer the backend breaks off after it has started cannot become an
	// error for the retry loop: the forwarder aborts the client's connection
	// instead, by panicking, and the panic has to carry on so the client is not
	// left holding half an answer. It is still the backend's failure, and is
	// counted as one on the way through. A client that hangs up is not: its own
	// context is done, and the backend is not blamed for it.
	defer func() {
		if recovered := recover(); recovered != nil {
			if current.aborted != nil && r.Context().Err() == nil {
				c.failed(r, active, backend, current.aborted)
			}
			panic(recovered)
		}
	}()

	started := time.Now()
	c.forward.ServeHTTP(current, r.WithContext(ctx))
	took := time.Since(started)

	if current.err == nil {
		c.succeeded(r, active, backend, current.status, took)
		return true, nil
	}
	c.failed(r, active, backend, current.err)
	return false, current.err
}

// succeeded records an attempt the backend served.
func (c *Core) succeeded(r *http.Request, active *settings, backend *balancer.Backend, status int, took time.Duration) {
	c.checker.ReportSuccess(backend.Addr)
	active.breaker.recordSuccess(backend.Addr)
	c.metrics.ObserveRequest(backend.Addr, strconv.Itoa(status), took)
	slog.Debug("request served", "method", r.Method, "path", r.URL.Path,
		"backend", backend.Addr, "status", status, "duration", took,
		"request_id", requestIDFrom(r.Context()))
}

// failed records an attempt the backend did not serve, against the backend.
func (c *Core) failed(r *http.Request, active *settings, backend *balancer.Backend, err error) {
	c.checker.ReportFailure(backend.Addr)
	active.breaker.recordFailure(backend.Addr)
	c.metrics.ObserveBackendFailure(backend.Addr)
	slog.Warn("backend attempt failed", "method", r.Method, "path", r.URL.Path,
		"backend", backend.Addr, "error", err,
		"request_id", requestIDFrom(r.Context()))
}

// available returns the backends that may serve the request now: healthy, not
// tripped by the circuit breaker, and not already tried for this request.
//
// If health checking has emptied the pool completely, the untried backends are
// returned anyway. Under overload the probes are the first thing to time out,
// and every backend can be marked unhealthy at once; refusing all traffic then
// turns a slow system into a broken one, while trying a backend that may still
// answer costs one attempt.
func (c *Core) available(active *settings, tried map[string]bool) []*balancer.Backend {
	fit := make([]*balancer.Backend, 0, len(active.backends))
	untried := make([]*balancer.Backend, 0, len(active.backends))

	for _, backend := range active.backends {
		if tried[backend.Addr] {
			continue
		}
		untried = append(untried, backend)
		if c.checker.IsHealthy(backend.Addr) && active.breaker.allow(backend.Addr) {
			fit = append(fit, backend)
		}
	}

	if len(fit) == 0 && len(untried) > 0 {
		c.metrics.ObserveRejection("no_healthy_backend")
		return untried
	}
	return fit
}

// replayableBody buffers the request body when a retry may need to send it
// again. It returns nil when there is no body to replay.
func (c *Core) replayableBody(r *http.Request, active *settings) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	if r.ContentLength > active.maxBody {
		return nil, errors.New("request body exceeds the configured limit")
	}
	if active.attempts == 1 {
		// Without retries the body is streamed straight through.
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, active.maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > active.maxBody {
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
	// aborted is the error of a backend that broke off an answer already on
	// its way to the client.
	aborted error
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

// attemptFrom returns the attempt a request is being forwarded in, if any.
func attemptFrom(ctx context.Context) *attempt {
	current, _ := ctx.Value(attemptKey{}).(*attempt)
	return current
}

// watchedBody notices a backend failing part way through its answer. The read
// that fails happens inside the forwarder, which then aborts rather than
// returning, so the attempt is marked here where the failure is seen.
type watchedBody struct {
	io.ReadCloser
	attempt *attempt
}

// Read passes the backend's answer through and marks the attempt aborted on
// any error other than the end of the answer.
func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.attempt.aborted = err
	}
	return n, err
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

// transport carries requests to the backends. Its connection pool is sized to
// the traffic the balancer accepts: Go's default keeps only two idle
// connections per host, which under load makes the proxy open a fresh TCP
// connection for nearly every request, exhaust the ephemeral port range and
// start failing with "can't assign requested address".
func transport(cfg *config.Config, backendCount int) http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = (&net.Dialer{
		Timeout:   time.Duration(cfg.Timeouts.ConnectTimeout),
		KeepAlive: 30 * time.Second,
	}).DialContext

	// Abandons a backend that accepts the connection but does not start
	// answering, so the failure policy can move the request elsewhere.
	base.ResponseHeaderTimeout = time.Duration(cfg.Timeouts.ResponseTimeout)

	// Inbound connections are capped, so at most that many requests can be in
	// flight upstream; sharing that budget across the pool bounds the idle
	// connections without throttling reuse.
	base.MaxIdleConns = cfg.Limits.MaxConnections
	base.MaxIdleConnsPerHost = idleConnsPerBackend(cfg.Limits.MaxConnections, backendCount)
	base.IdleConnTimeout = time.Duration(cfg.Timeouts.IdleTimeout)

	return base
}

// idleConnsPerBackend divides the connection budget over the pool, keeping
// enough per backend to be useful when the budget is small or the pool large.
func idleConnsPerBackend(maxConnections, backendCount int) int {
	const minimum = 32

	if backendCount < 1 {
		backendCount = 1
	}
	if share := maxConnections / backendCount; share > minimum {
		return share
	}
	return minimum
}
