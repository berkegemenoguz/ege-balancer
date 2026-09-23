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
// how much work it can do at once, how large its answers are, what it
// remembers about its clients, and how it fails. A profile with only a name
// answers at once with that name, as hashicorp/http-echo did before it.
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

	// cacheSize is how many client sessions the backend remembers, keyed by the
	// X-Session header; zero remembers none. A request from a session it has
	// never seen, or has forgotten, costs missPenalty on top of its service
	// time, which is what makes sending a client back to the same backend
	// worth something.
	cacheSize   int
	missPenalty time.Duration

	// The shares of requests that fail other than with a 500: held without
	// ever being answered, dropped half way through the answer, or answered
	// in pieces spread over dripOver.
	hangRate  float64
	resetRate float64
	dripRate  float64
	dripOver  time.Duration

	// coldStart is how long after starting the backend is slower than its
	// profile, and coldFactor how much slower at the very start; the factor
	// falls linearly to 1 over coldStart. A factor of 0 or 1 means none.
	coldStart  time.Duration
	coldFactor float64

	// pause stops the whole process at the end of every pauseEvery, health
	// check included, as a stop-the-world garbage collection does.
	pauseEvery time.Duration
	pause      time.Duration
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
	for _, rate := range []struct {
		flag  string
		value float64
	}{{"error-rate", p.errorRate}, {"hang-rate", p.hangRate}, {"reset-rate", p.resetRate}, {"drip-rate", p.dripRate}} {
		if rate.value < 0 || rate.value > 1 {
			add("%s must be between 0 and 1", rate.flag)
		}
	}
	if sum := p.errorRate + p.hangRate + p.resetRate + p.dripRate; sum > 1 {
		add("error, hang, reset and drip rates add up to %g, more than every request", sum)
	}

	if p.cacheSize < 0 {
		add("cache-size must not be negative")
	}
	if p.missPenalty < 0 {
		add("miss-penalty must not be negative")
	}
	if p.missPenalty > 0 && p.cacheSize == 0 {
		add("miss-penalty needs a cache-size to miss")
	}
	if p.dripOver < 0 {
		add("drip-over must not be negative")
	}
	if p.dripRate > 0 && p.dripOver == 0 {
		add("drip-rate needs a positive drip-over")
	}
	if p.coldStart < 0 {
		add("cold-start must not be negative")
	}
	if p.coldFactor != 0 && p.coldFactor < 1 {
		add("cold-factor must be at least 1")
	}
	if p.coldFactor > 1 && p.coldStart == 0 {
		add("cold-factor needs a cold-start period to fall over")
	}
	if p.pauseEvery < 0 || p.pause < 0 {
		add("pause-every and pause must not be negative")
	}
	if p.pause > 0 && p.pauseEvery <= p.pause {
		add("pause-every (%s) must be longer than pause (%s)", p.pauseEvery, p.pause)
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

// draw returns a uniform number in [0, 1), from which a request's fault is
// chosen.
func (s *sampler) draw() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64()
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
