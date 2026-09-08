package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// validate reports every problem found in the configuration, so that a single
// run surfaces all of them instead of one per attempt.
func (c *Config) validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if c.ListenAddr == "" {
		add("listen_addr is required")
	} else if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		add("listen_addr %q is not a host:port address", c.ListenAddr)
	}

	if _, _, err := net.SplitHostPort(c.MetricsAddr); err != nil {
		add("metrics_addr %q is not a host:port address", c.MetricsAddr)
	}

	switch c.Algorithm {
	case RoundRobin, LeastConnections, WeightedRoundRobin:
	case "":
		add("algorithm is required")
	default:
		add("algorithm %q is unknown, expected %s, %s or %s",
			c.Algorithm, RoundRobin, LeastConnections, WeightedRoundRobin)
	}

	switch c.FailurePolicy {
	case RetryNextBackend, FailFast, CircuitBreakerPolicy:
	case "":
		add("failure_policy is required")
	default:
		add("failure_policy %q is unknown, expected %s, %s or %s",
			c.FailurePolicy, RetryNextBackend, FailFast, CircuitBreakerPolicy)
	}

	if c.Retry.MaxRetries < 0 {
		add("retry.max_retries must not be negative")
	}
	if c.FailurePolicy == RetryNextBackend && c.Retry.MaxRetries == 0 {
		add("retry.max_retries must be at least 1 when failure_policy is %s", RetryNextBackend)
	}

	if c.FailurePolicy == CircuitBreakerPolicy {
		if c.CircuitBreaker.FailureThreshold < 1 {
			add("circuit_breaker.failure_threshold must be at least 1")
		}
		if c.CircuitBreaker.OpenDuration <= 0 {
			add("circuit_breaker.open_duration must be greater than zero")
		}
	}

	problems = append(problems, c.validateBackends()...)
	problems = append(problems, c.validateHealthCheck()...)
	problems = append(problems, c.validateTimeouts()...)
	problems = append(problems, c.validateLimits()...)
	problems = append(problems, c.validateLogging()...)

	return errors.Join(problems...)
}

func (c *Config) validateBackends() []error {
	if len(c.Backends) == 0 {
		return []error{errors.New("backends must contain at least one entry")}
	}

	var problems []error
	seen := make(map[string]int, len(c.Backends))
	for i, backend := range c.Backends {
		if backend.Addr == "" {
			problems = append(problems, fmt.Errorf("backends[%d].addr is required", i))
		} else {
			if _, _, err := net.SplitHostPort(backend.Addr); err != nil {
				problems = append(problems, fmt.Errorf(
					"backends[%d].addr %q is not a host:port address", i, backend.Addr))
			}
			if first, duplicate := seen[backend.Addr]; duplicate {
				problems = append(problems, fmt.Errorf(
					"backends[%d].addr %q duplicates backends[%d]", i, backend.Addr, first))
			} else {
				seen[backend.Addr] = i
			}
		}
		if backend.Weight < 0 {
			problems = append(problems, fmt.Errorf("backends[%d].weight must not be negative", i))
		}
	}
	return problems
}

func (c *Config) validateHealthCheck() []error {
	var problems []error
	hc := c.HealthCheck

	if !strings.HasPrefix(hc.Path, "/") {
		problems = append(problems, fmt.Errorf(
			"health_check.path %q must start with a slash", hc.Path))
	}
	if hc.Interval <= 0 {
		problems = append(problems, errors.New("health_check.interval must be greater than zero"))
	}
	if hc.Timeout <= 0 {
		problems = append(problems, errors.New("health_check.timeout must be greater than zero"))
	}
	if hc.Interval > 0 && hc.Timeout > 0 && hc.Timeout >= hc.Interval {
		problems = append(problems, fmt.Errorf(
			"health_check.timeout (%s) must be shorter than health_check.interval (%s)",
			hc.Timeout, hc.Interval))
	}
	if hc.HealthyThreshold < 1 {
		problems = append(problems, errors.New("health_check.healthy_threshold must be at least 1"))
	}
	if hc.UnhealthyThreshold < 1 {
		problems = append(problems, errors.New("health_check.unhealthy_threshold must be at least 1"))
	}
	return problems
}

func (c *Config) validateTimeouts() []error {
	var problems []error
	for _, field := range []struct {
		name  string
		value Duration
	}{
		{"connect_timeout", c.Timeouts.ConnectTimeout},
		{"read_timeout", c.Timeouts.ReadTimeout},
		{"write_timeout", c.Timeouts.WriteTimeout},
		{"idle_timeout", c.Timeouts.IdleTimeout},
	} {
		if field.value <= 0 {
			problems = append(problems, fmt.Errorf(
				"timeouts.%s must be greater than zero", field.name))
		}
	}
	return problems
}

func (c *Config) validateLimits() []error {
	var problems []error
	if c.Limits.MaxConnections < 1 {
		problems = append(problems, errors.New("limits.max_connections must be at least 1"))
	}
	if c.Limits.MaxRequestBodyBytes < 1 {
		problems = append(problems, errors.New("limits.max_request_body_bytes must be at least 1"))
	}
	if c.Limits.RateLimitPerIP < 0 {
		problems = append(problems, errors.New("limits.rate_limit_per_ip must not be negative"))
	}
	return problems
}

func (c *Config) validateLogging() []error {
	var problems []error
	switch c.Logging.Level {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
	default:
		problems = append(problems, fmt.Errorf(
			"logging.level %q is unknown, expected %s, %s, %s or %s",
			c.Logging.Level, LevelDebug, LevelInfo, LevelWarn, LevelError))
	}
	switch c.Logging.Format {
	case FormatJSON, FormatText:
	default:
		problems = append(problems, fmt.Errorf(
			"logging.format %q is unknown, expected %s or %s",
			c.Logging.Format, FormatJSON, FormatText))
	}
	return problems
}
