package main

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"
)

// profile describes how one mock backend behaves: how long it takes to answer,
// how much work it can do at once, how large its answers are and how often it
// fails. A profile with only a name answers at once with that name, as
// hashicorp/http-echo did before it.
type profile struct {
	name string

	// latency is the median time to serve a request once a worker has picked
	// it up, and p99 the time one request in a hundred exceeds. Together they
	// fix a log-normal distribution, which has the long tail real services
	// show. Without p99 every request takes exactly latency.
	latency time.Duration
	p99     time.Duration

	// capacity is how many requests are served at once; zero means no limit.
	capacity int
	// queue is how many requests may wait for a worker. Beyond it the backend
	// answers 503 at once, as an overloaded server would.
	queue int

	bodySize  int
	errorRate float64
}

// validate reports every problem with the profile at once.
func (p profile) validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if p.name == "" {
		add("name is required")
	}
	if p.latency < 0 {
		add("latency must not be negative")
	}
	if p.p99 < 0 {
		add("latency-p99 must not be negative")
	}
	if p.p99 > 0 && p.latency == 0 {
		add("latency-p99 needs a median latency")
	}
	if p.p99 > 0 && p.p99 < p.latency {
		add("latency-p99 (%s) must not be below latency (%s)", p.p99, p.latency)
	}
	if p.capacity < 0 {
		add("capacity must not be negative")
	}
	if p.queue < 0 {
		add("queue must not be negative")
	}
	if p.queue > 0 && p.capacity == 0 {
		add("queue needs a capacity to wait for")
	}
	if p.bodySize < 0 {
		add("body-size must not be negative")
	}
	if p.errorRate < 0 || p.errorRate > 1 {
		add("error-rate must be between 0 and 1")
	}
	return errors.Join(problems...)
}

// z99 is the 99th percentile of the standard normal distribution.
const z99 = 2.3263478740408408

// sampler draws service times and failures for a profile. Its generator is
// seeded, so that a load test can be repeated with the same sequence.
type sampler struct {
	// median is in nanoseconds; sigma is the spread of the logarithm.
	median float64
	sigma  float64

	mu  sync.Mutex
	rng *rand.Rand
}

// newSampler returns a sampler for the latency distribution of p.
func newSampler(p profile, seed uint64) *sampler {
	s := &sampler{
		median: float64(p.latency),
		rng:    rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
	}
	if p.p99 > p.latency && p.latency > 0 {
		// For a log-normal distribution the median is e^mu and the 99th
		// percentile e^(mu + z99·sigma), so their ratio fixes sigma.
		s.sigma = math.Log(float64(p.p99)/float64(p.latency)) / z99
	}
	return s
}

// latency draws the time the next request takes to serve.
func (s *sampler) latency() time.Duration {
	if s.sigma == 0 {
		return time.Duration(s.median)
	}
	s.mu.Lock()
	z := s.rng.NormFloat64()
	s.mu.Unlock()
	return time.Duration(s.median * math.Exp(s.sigma*z))
}

// fails reports whether the next request should fail, at the given rate.
func (s *sampler) fails(rate float64) bool {
	if rate <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64() < rate
}

// parseSize reads a byte count written as a plain number or with a B, KiB or
// MiB suffix.
func parseSize(value string) (int, error) {
	units := []struct {
		suffix string
		scale  int
	}{{"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}

	number, scale := strings.TrimSpace(value), 1
	for _, unit := range units {
		if trimmed, found := strings.CutSuffix(number, unit.suffix); found {
			number, scale = strings.TrimSpace(trimmed), unit.scale
			break
		}
	}

	count, err := strconv.Atoi(number)
	if err != nil || count < 0 {
		return 0, fmt.Errorf("size %q is not a byte count such as 512, 4KiB or 1MiB", value)
	}
	return count * scale, nil
}
