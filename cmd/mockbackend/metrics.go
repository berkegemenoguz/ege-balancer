package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// What the backend did with a request, as its metrics count it. The faults
// share their names with the modes that cause them.
const (
	outcomeAnswered   = "answered"   // answered in full
	outcomeOverloaded = "overloaded" // refused with 503, the queue being full
	outcomeAbandoned  = "abandoned"  // the client left before it was answered
)

// outcomes are every outcome the metrics report, in the order they list them.
var outcomes = []string{
	outcomeAnswered, string(modeHang), string(modeReset), string(modeDrip), string(modeError),
	outcomeOverloaded, outcomeAbandoned,
}

// modes are every fault the admin port can inject, in the order the metrics
// list them.
var modes = []mode{modeHang, modeReset, modeDrip, modeError, modeSlow, modeFreeze}

// counts are the events the backend has seen since it started. Every counter
// exists from the start, at zero, so that the first event is a change
// Prometheus can see rather than the first sample of a new series.
type counts struct {
	outcomes map[string]*atomic.Int64
	cache    map[string]*atomic.Int64
	injected map[mode]*atomic.Int64
}

// newCounts returns counters for every outcome, cache result and fault.
func newCounts() *counts {
	c := &counts{
		outcomes: map[string]*atomic.Int64{},
		cache:    map[string]*atomic.Int64{"hit": {}, "miss": {}},
		injected: map[mode]*atomic.Int64{},
	}
	for _, o := range outcomes {
		c.outcomes[o] = &atomic.Int64{}
	}
	for _, m := range modes {
		c.injected[m] = &atomic.Int64{}
	}
	return c
}

// metrics serves the backend's counters and its state in the Prometheus text
// format. They are written here rather than with the Prometheus client
// library: there are a few dozen counters and gauges and no histograms, and
// the mock stays a program of the standard library alone. Latency is not
// among them; the balancer measures it where the client feels it.
func (s *server) metrics(w http.ResponseWriter, _ *http.Request) {
	var out exposition

	out.family("mock_requests_total", "counter", "Requests, by what the backend did with them.")
	for _, o := range outcomes {
		out.sample("outcome", o, float64(s.counts.outcomes[o].Load()))
	}

	out.family("mock_cache_requests_total", "counter", "Requests naming a session, by whether the backend remembered it.")
	for _, result := range []string{"hit", "miss"} {
		out.sample("result", result, float64(s.counts.cache[result].Load()))
	}

	out.family("mock_faults_injected_total", "counter", "Faults injected through the admin port, by mode.")
	for _, m := range modes {
		out.sample("mode", string(m), float64(s.counts.injected[m].Load()))
	}

	inForce := map[mode]bool{}
	for _, active := range s.faults.active() {
		inForce[active.Mode] = true
	}
	out.family("mock_fault_in_force", "gauge", "1 while a fault injected through the admin port is in force, by mode.")
	for _, m := range modes {
		out.sample("mode", string(m), boolean(inForce[m]))
	}

	cacheKeys := 0
	if s.cache != nil {
		cacheKeys = s.cache.size()
	}
	out.family("mock_cache_keys", "gauge", "Sessions the backend remembers.")
	out.sample("", "", float64(cacheKeys))

	out.family("mock_busy_workers", "gauge", "Requests being served.")
	out.sample("", "", float64(s.busy.Load()))

	out.family("mock_capacity", "gauge", "Requests the backend serves at once; 0 for no limit.")
	out.sample("", "", float64(s.profile.capacity))

	out.family("mock_waiting", "gauge", "Requests queued for a worker.")
	out.sample("", "", float64(s.waiting.Load()))

	out.family("mock_cold_factor", "gauge", "How many times slower than normal a cold start makes the backend; 1 once warm.")
	out.sample("", "", s.clock.factor())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(out.String()))
}

// exposition builds metrics in the Prometheus text format. The names, help
// texts and label values are all constants of this file, so none of them
// needs escaping.
type exposition struct {
	strings.Builder
	name string
}

// family starts a metric family: its help text, its type, and the name the
// samples that follow belong to.
func (e *exposition) family(name, kind, help string) {
	e.name = name
	_, _ = fmt.Fprintf(e, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// sample adds one sample to the current family, with one label or none.
func (e *exposition) sample(label, value string, v float64) {
	formatted := strconv.FormatFloat(v, 'g', -1, 64)
	if label == "" {
		_, _ = fmt.Fprintf(e, "%s %s\n", e.name, formatted)
		return
	}
	_, _ = fmt.Fprintf(e, "%s{%s=%q} %s\n", e.name, label, value, formatted)
}

// boolean is 1 for true and 0 for false.
func boolean(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
