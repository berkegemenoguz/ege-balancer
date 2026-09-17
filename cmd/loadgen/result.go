package main

import (
	"slices"
	"sort"
	"time"
)

// samples is what one worker observed. Each worker owns its own, so nothing is
// shared while the load is running and the measurement does not pay for locks.
type samples struct {
	latencies []time.Duration
	statuses  map[int]int
	backends  map[string]int
	// perBackend keeps the latencies of each backend apart, which is what tells
	// a slow hump in the distribution from a slow backend.
	perBackend map[string][]time.Duration
	failures   map[string]int
	bytes      int64
}

// newSamples returns an empty set, with room for a run's worth of latencies.
func newSamples() *samples {
	return &samples{
		latencies:  make([]time.Duration, 0, 4096),
		statuses:   make(map[int]int),
		backends:   make(map[string]int),
		perBackend: make(map[string][]time.Duration),
		failures:   make(map[string]int),
	}
}

// record adds one answered request.
func (s *samples) record(took time.Duration, status int, backend string, bytes int64) {
	s.latencies = append(s.latencies, took)
	s.statuses[status]++
	if backend != "" {
		s.backends[backend]++
		s.perBackend[backend] = append(s.perBackend[backend], took)
	}
	s.bytes += bytes
}

// recordFailure adds one request that never produced an answer.
func (s *samples) recordFailure(kind string) {
	s.failures[kind]++
}

// merge folds other into s.
func (s *samples) merge(other *samples) {
	s.latencies = append(s.latencies, other.latencies...)
	s.bytes += other.bytes
	for status, n := range other.statuses {
		s.statuses[status] += n
	}
	for backend, n := range other.backends {
		s.backends[backend] += n
	}
	for backend, latencies := range other.perBackend {
		s.perBackend[backend] = append(s.perBackend[backend], latencies...)
	}
	for kind, n := range other.failures {
		s.failures[kind] += n
	}
}

// cancelled is the failure kind of a request the run itself cut off at the
// deadline. It is reported apart from the failures the balancer caused.
const cancelled = "cancelled"

// result is one finished run, in the shape the report quotes.
type result struct {
	Algorithm   string         `json:"algorithm,omitempty"`
	Connections int            `json:"connections"`
	Duration    string         `json:"duration"`
	Requests    int            `json:"requests"`
	Failures    int            `json:"failures"`
	CutOff      int            `json:"cut_off_at_the_deadline"`
	Throughput  float64        `json:"throughput_per_second"`
	MegabytesPS float64        `json:"megabytes_per_second"`
	P50         string         `json:"p50"`
	P95         string         `json:"p95"`
	P99         string         `json:"p99"`
	Max         string         `json:"max"`
	Counters    map[string]int `json:"balancer_counters,omitempty"`
	Histogram   []bucket       `json:"histogram,omitempty"`
	PerBackend  []timing       `json:"per_backend,omitempty"`
	Statuses    map[int]int    `json:"statuses"`
	Backends    map[string]int `json:"backends"`
	FailureKind map[string]int `json:"failure_kinds,omitempty"`
}

// summarise turns the merged samples of a run into a result.
func summarise(all *samples, connections int, took time.Duration) result {
	slices.Sort(all.latencies)

	failures := 0
	for kind, n := range all.failures {
		if kind == cancelled {
			continue
		}
		failures += n
	}

	seconds := took.Seconds()
	return result{
		Connections: connections,
		Duration:    took.Round(time.Millisecond).String(),
		Requests:    len(all.latencies),
		Failures:    failures,
		CutOff:      all.failures[cancelled],
		Throughput:  float64(len(all.latencies)) / seconds,
		MegabytesPS: float64(all.bytes) / seconds / (1 << 20),
		P50:         percentile(all.latencies, 0.50).Round(100 * time.Microsecond).String(),
		P95:         percentile(all.latencies, 0.95).Round(100 * time.Microsecond).String(),
		P99:         percentile(all.latencies, 0.99).Round(100 * time.Microsecond).String(),
		Max:         percentile(all.latencies, 1).Round(100 * time.Microsecond).String(),
		Histogram:   distribution(all.latencies),
		PerBackend:  timings(all.perBackend),
		Statuses:    all.statuses,
		Backends:    all.backends,
		FailureKind: all.failures,
	}
}

// timing is how one backend answered: how much of the traffic it took and how
// long it took over it.
type timing struct {
	Backend  string `json:"backend"`
	Requests int    `json:"requests"`
	P50      string `json:"p50"`
	P95      string `json:"p95"`
	Max      string `json:"max"`
}

// timings summarises each backend's own latencies, ordered by backend name.
func timings(perBackend map[string][]time.Duration) []timing {
	names := make([]string, 0, len(perBackend))
	for name := range perBackend {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return lessBackend(names[i], names[j]) })

	measured := make([]timing, 0, len(names))
	for _, name := range names {
		latencies := perBackend[name]
		slices.Sort(latencies)
		measured = append(measured, timing{
			Backend:  name,
			Requests: len(latencies),
			P50:      percentile(latencies, 0.50).Round(100 * time.Microsecond).String(),
			P95:      percentile(latencies, 0.95).Round(100 * time.Microsecond).String(),
			Max:      percentile(latencies, 1).Round(100 * time.Microsecond).String(),
		})
	}
	return measured
}

// bucket is one band of the latency histogram: how many requests were answered
// in at most Under, and no faster than the band below it.
type bucket struct {
	Under    string `json:"under"`
	Requests int    `json:"requests"`
}

// bands are the upper bounds of the histogram, spaced roughly by powers of ten
// and their halves, which is enough to tell a single hump from two.
var bands = []time.Duration{
	time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond,
	10 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond,
	100 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2 * time.Second, 5 * time.Second,
}

// distribution counts a sorted run into the bands, with a final open band for
// everything slower. Bands with no requests in them are left out.
func distribution(sorted []time.Duration) []bucket {
	if len(sorted) == 0 {
		return nil
	}

	counted := make([]bucket, 0, len(bands)+1)
	from := 0
	for _, upper := range bands {
		to := from
		for to < len(sorted) && sorted[to] <= upper {
			to++
		}
		if to > from {
			counted = append(counted, bucket{Under: upper.String(), Requests: to - from})
		}
		from = to
	}
	if from < len(sorted) {
		counted = append(counted, bucket{Under: "more", Requests: len(sorted) - from})
	}
	return counted
}

// percentile returns the q quantile of a sorted slice, by nearest rank. An
// empty slice has no percentile and reports zero.
func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}

	rank := int(q * float64(len(sorted)))
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
