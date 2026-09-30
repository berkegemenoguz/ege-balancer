// Package config reads and validates the YAML configuration file: backend list,
// weights, algorithm selection, health check settings, timeouts and limits.
// Every other module depends on the types this package exposes.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Algorithm selects the load balancing strategy.
type Algorithm string

const (
	RoundRobin         Algorithm = "round_robin"
	LeastConnections   Algorithm = "least_connections"
	WeightedRoundRobin Algorithm = "weighted_round_robin"
	// ConsistentHashing sends every request with the same key to the same
	// backend; consistent_hash says what the key is.
	ConsistentHashing Algorithm = "consistent_hash"
)

// FailurePolicy selects how the proxy reacts when a backend fails to serve a
// request.
type FailurePolicy string

const (
	// RetryNextBackend forwards the request to the next healthy backend.
	RetryNextBackend FailurePolicy = "retry_next_backend"
	// FailFast returns the error to the client without retrying.
	FailFast FailurePolicy = "fail_fast"
	// CircuitBreakerPolicy takes a repeatedly failing backend out of the pool
	// for a fixed period.
	CircuitBreakerPolicy FailurePolicy = "circuit_breaker"
)

// StickyMode selects how a client is kept on the backend that first served it.
type StickyMode string

const (
	// StickyNone places every request by the algorithm alone.
	StickyNone StickyMode = "none"
	// StickyCookie keeps a client on its backend by a cookie the balancer sets.
	StickyCookie StickyMode = "cookie"
)

// DefaultStickyCookie is the name of the balancer's cookie when none is given.
const DefaultStickyCookie = "lb_backend"

// LogLevel is the minimum severity that is written to the log.
type LogLevel string

const (
	LevelDebug LogLevel = "debug"
	LevelInfo  LogLevel = "info"
	LevelWarn  LogLevel = "warn"
	LevelError LogLevel = "error"
)

// LogFormat is the encoding of a log record.
type LogFormat string

const (
	FormatJSON LogFormat = "json"
	FormatText LogFormat = "text"
)

// Config is the fully parsed and validated configuration of the load balancer.
type Config struct {
	ListenAddr string `yaml:"listen_addr"`
	// MetricsAddr serves /metrics and /status on a separate socket, so that
	// those paths stay proxyable on the traffic port and are not exposed to
	// clients by accident.
	MetricsAddr string `yaml:"metrics_addr"`
	// EnablePprof adds the Go profiling endpoints to the metrics server. It is
	// off by default: those endpoints expose heap and goroutine state, which is
	// for an operator to reach deliberately, not something to serve always.
	EnablePprof    bool           `yaml:"enable_pprof"`
	Algorithm      Algorithm      `yaml:"algorithm"`
	ConsistentHash ConsistentHash `yaml:"consistent_hash"`
	Sticky         Sticky         `yaml:"sticky"`
	FailurePolicy  FailurePolicy  `yaml:"failure_policy"`
	RetryOn5xx     bool           `yaml:"retry_on_5xx"`
	Retry          Retry          `yaml:"retry"`
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
	Backends       []Backend      `yaml:"backends"`
	HealthCheck    HealthCheck    `yaml:"health_check"`
	Timeouts       Timeouts       `yaml:"timeouts"`
	Limits         Limits         `yaml:"limits"`
	Logging        Logging        `yaml:"logging"`
}

// ConsistentHash configures the consistent_hash algorithm. Like the circuit
// breaker's settings, it may be present whatever the algorithm, and is only
// read under its own.
type ConsistentHash struct {
	// Key is what a request is hashed on: "header:<Name>" for the value of a
	// request header, or "client_ip" for the address the request came from.
	// A request without one is balanced by least connections instead.
	Key string `yaml:"key"`
	// BalanceFactor bounds the load on any one backend, in per cent of its
	// share of the requests in flight: a backend already serving that much
	// passes the next request to the one after it for that key. 0 leaves the
	// load unbounded.
	BalanceFactor int `yaml:"balance_factor"`
}

// KeyClientIP hashes a request on the address it came from.
const KeyClientIP = "client_ip"

// Header is the request header the key is read from, when the key names one.
func (h ConsistentHash) Header() (string, bool) {
	name, found := strings.CutPrefix(h.Key, "header:")
	if !found {
		return "", false
	}
	return name, true
}

// Sticky configures session affinity. Under cookie, the first request of a
// client is placed by the algorithm, and a cookie the balancer sets keeps the
// client's later requests on the backend that answered it, for as long as that
// backend can take them.
type Sticky struct {
	Mode StickyMode `yaml:"mode"`
	// Cookie is the name of the balancer's cookie.
	Cookie string `yaml:"cookie"`
	// MaxAge is how long the browser keeps the cookie; 0 keeps it until the
	// browser closes.
	MaxAge Duration `yaml:"max_age"`
	// Secure restricts the cookie to HTTPS. The balancer speaks plain HTTP,
	// so this is for a balancer behind something that terminates TLS.
	Secure bool `yaml:"secure"`
}

// Retry bounds how often a single request may be forwarded again, and how many
// retries the whole balancer may have in flight at once.
type Retry struct {
	MaxRetries int `yaml:"max_retries"`
	// BudgetPercent caps the retries in flight at this share of the requests in
	// flight, so that retries cannot multiply the load on a failing pool.
	BudgetPercent float64 `yaml:"budget_percent"`
	// MinRetryConcurrency is the number of retries always allowed in flight,
	// whatever the load, so that light traffic can still be retried.
	MinRetryConcurrency int `yaml:"min_retry_concurrency"`
}

// CircuitBreaker describes when a backend is tripped out of the pool and for
// how long it stays out before a probe request is allowed through.
type CircuitBreaker struct {
	FailureThreshold int      `yaml:"failure_threshold"`
	OpenDuration     Duration `yaml:"open_duration"`
}

// Backend is a single upstream server. A weight of zero means the default
// weight of one; it is consulted by weighted round robin and consistent
// hashing.
type Backend struct {
	Addr   string `yaml:"addr"`
	Weight int    `yaml:"weight"`
}

// HealthCheck configures the active probe and the thresholds that move a
// backend between the healthy and unhealthy states.
type HealthCheck struct {
	Path               string   `yaml:"path"`
	Interval           Duration `yaml:"interval"`
	Timeout            Duration `yaml:"timeout"`
	HealthyThreshold   int      `yaml:"healthy_threshold"`
	UnhealthyThreshold int      `yaml:"unhealthy_threshold"`
}

// Timeouts bound every phase of proxying a request. Connect and response bound
// the conversation with a backend; read, write and idle bound the one with the
// client.
type Timeouts struct {
	ConnectTimeout Duration `yaml:"connect_timeout"`
	// ResponseTimeout is how long a backend may take to start answering before
	// the attempt is abandoned. Without it a slow backend holds a request until
	// the client-side write timeout kills it, with no chance to try another
	// backend. Defaults to the read timeout when omitted.
	ResponseTimeout Duration `yaml:"response_timeout"`
	ReadTimeout     Duration `yaml:"read_timeout"`
	WriteTimeout    Duration `yaml:"write_timeout"`
	IdleTimeout     Duration `yaml:"idle_timeout"`
}

// Limits protect the load balancer from exhausting its own resources.
// A rate limit of zero disables per-IP rate limiting.
type Limits struct {
	MaxConnections      int   `yaml:"max_connections"`
	MaxRequestBodyBytes int64 `yaml:"max_request_body_bytes"`
	RateLimitPerIP      int   `yaml:"rate_limit_per_ip"`
}

// Logging configures the structured logger.
type Logging struct {
	Level  LogLevel  `yaml:"level"`
	Format LogFormat `yaml:"format"`
}

// Load reads the configuration file at path, applies the defaults for the
// optional fields and validates the result. The returned error reports every
// problem that was found, not just the first one.
func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only file

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// applyDefaults fills in the fields that may be omitted from the file.
func (c *Config) applyDefaults() {
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":8081"
	}
	// The retry budget defaults are Envoy's: retries may add a fifth to the
	// load, and three may always be in flight.
	if c.Retry.BudgetPercent == 0 {
		c.Retry.BudgetPercent = 20
	}
	if c.Retry.MinRetryConcurrency == 0 {
		c.Retry.MinRetryConcurrency = 3
	}
	if c.Timeouts.ResponseTimeout == 0 {
		c.Timeouts.ResponseTimeout = c.Timeouts.ReadTimeout
	}
	if c.Sticky.Mode == "" {
		c.Sticky.Mode = StickyNone
	}
	if c.Sticky.Cookie == "" {
		c.Sticky.Cookie = DefaultStickyCookie
	}
	if c.Logging.Level == "" {
		c.Logging.Level = LevelInfo
	}
	if c.Logging.Format == "" {
		c.Logging.Format = FormatJSON
	}
	for i := range c.Backends {
		if c.Backends[i].Weight == 0 {
			c.Backends[i].Weight = 1
		}
	}
}
