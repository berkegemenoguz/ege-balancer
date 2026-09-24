// Command loadgen drives closed-loop HTTP load through the load balancer and
// reports what came back: throughput, latency percentiles, the status codes the
// client saw, and the share of the traffic each backend served.
//
// It is a development tool, kept in the repository so that the measurements in
// docs/performance-report.md can be repeated rather than taken on trust.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// backendHeader is the header the mock backends answer with, which is how
	// the generator reports the distribution without reading the balancer's
	// metrics.
	backendHeader = "X-Backend"
	// sessionHeader names the client a request comes from, and cacheHeader is
	// the mock backend's answer to whether it remembered that client.
	sessionHeader = "X-Session"
	cacheHeader   = "X-Cache"
)

// load is what every request of a run looks like.
type load struct {
	url    string
	method string
	// body is sent with every request; nil sends none.
	body []byte
	// keys is how many client sessions the requests are spread over, each
	// request naming one at random; zero names none.
	keys int
}

func main() {
	target := flag.String("addr", "http://127.0.0.1:8080", "address of the balancer to drive")
	path := flag.String("path", "/", "path to request")
	connections := flag.Int("connections", 100, "connections held open for the whole run, one goroutine each")
	duration := flag.Duration("duration", 20*time.Second, "how long to measure")
	warmup := flag.Duration("warmup", 5*time.Second, "load applied before measuring starts, and discarded")
	timeout := flag.Duration("timeout", 10*time.Second, "timeout of a single request")
	metrics := flag.String("metrics", "http://127.0.0.1:8081/metrics", "balancer metrics endpoint, read at both ends of the measurement window; empty to skip")
	method := flag.String("method", http.MethodGet, "method of every request")
	bodySize := flag.Int("body-size", 0, "bytes of body sent with every request")
	keys := flag.Int("keys", 0, "client sessions to spread the requests over, named in X-Session; 0 names none")
	label := flag.String("label", "", "label recorded with the result, such as the algorithm in force")
	shape := flag.Bool("histogram", false, "also print the latency distribution")
	out := flag.String("json", "", "also write the result as JSON to this file")
	flag.Parse()

	if *connections < 1 {
		log.Fatal("connections must be at least 1")
	}
	if *bodySize < 0 || *keys < 0 {
		log.Fatal("body-size and keys must not be negative")
	}

	traffic := load{url: *target + *path, method: strings.ToUpper(*method), keys: *keys}
	if *bodySize > 0 {
		traffic.body = bytes.Repeat([]byte("x"), *bodySize)
	}

	summary := run(traffic, *metrics, *connections, *warmup, *duration, *timeout)
	summary.Algorithm = *label

	fmt.Print(summary.text())
	if *shape {
		fmt.Print(summary.histogramText())
	}

	if *out != "" {
		if err := writeJSON(*out, summary); err != nil {
			log.Fatalf("writing %s: %v", *out, err)
		}
	}
}

// run applies the load and returns the measured result. The warmup period is
// driven exactly like the measurement, and thrown away: connections are being
// opened then, and the balancer's own pools are still filling.
//
// Workers are asked to stop rather than cut off, so the run ends with no
// request in flight and every request sent is also measured. Balancers before
// v1.3.2 also counted a cancelled request as a failed attempt at every backend
// it was offered to, and as a refusal the load never caused.
func run(shape load, metricsURL string, connections int, warmup, duration, timeout time.Duration) result {
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport(connections),
	}

	measureFrom := time.Now().Add(warmup)

	var done atomic.Bool
	stop := time.AfterFunc(warmup+duration, func() { done.Store(true) })
	defer stop.Stop()

	measured := make([]*samples, connections)

	var wg sync.WaitGroup
	for i := range connections {
		measured[i] = newSamples()
		wg.Go(func() {
			drive(client, shape, measureFrom, &done, measured[i])
		})
	}
	// The counters are read once the warmup is over and again at the end, so
	// that what they report belongs to the measured window.
	before := readCounters(context.Background(), metricsURL, time.Until(measureFrom))

	wg.Wait()
	// Throughput is over the measured window itself. Requests that started in it
	// count even when they finish after it, but the time spent waiting for them
	// does not: a request hanging for ten seconds would otherwise stretch the
	// window and understate every figure divided by it.
	took := duration
	moved := before.since(readCounters(context.Background(), metricsURL, 0))

	all := newSamples()
	for _, worker := range measured {
		all.merge(worker)
	}
	summary := summarise(all, connections, took)
	summary.Counters = moved
	return summary
}

// readCounters waits for the given delay and then scrapes the balancer's own
// counters. A failure to read them is reported and left out of the result: the
// load measurement itself is still valid without them.
func readCounters(ctx context.Context, url string, after time.Duration) counters {
	if after > 0 {
		time.Sleep(after)
	}
	if url == "" {
		return nil
	}

	read, err := scrapeCounters(ctx, &http.Client{Timeout: 5 * time.Second}, url)
	if err != nil {
		log.Printf("reading %s: %v", url, err)
		return nil
	}
	return read
}

// transport keeps one idle connection per worker, so that a run holds the
// connections it opened instead of building new ones for every request.
func transport(connections int) http.RoundTripper {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        connections,
		MaxIdleConnsPerHost: connections,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
}

// drive sends requests one after another until the run is done, recording into
// hot once the warmup is over. Requests sent during the warmup are discarded.
func drive(client *http.Client, shape load, measureFrom time.Time, done *atomic.Bool, hot *samples) {
	discard := newSamples()

	for !done.Load() {
		into := hot
		if time.Now().Before(measureFrom) {
			into = discard
		}
		request(context.Background(), client, shape, into)
	}
}

// request sends one request and records its outcome.
func request(ctx context.Context, client *http.Client, shape load, into *samples) {
	var body io.Reader
	if shape.body != nil {
		body = bytes.NewReader(shape.body)
	}
	attempt, err := http.NewRequestWithContext(ctx, shape.method, shape.url, body)
	if err != nil {
		into.recordFailure("request")
		return
	}
	if shape.keys > 0 {
		attempt.Header.Set(sessionHeader, "session-"+strconv.Itoa(rand.IntN(shape.keys)))
	}

	// Go's client sends an idempotent request again, on a new connection, when
	// the one it reused closes before any answer arrives. The balancer counts
	// nothing for that, so the generator counts the connections a request took.
	connections := 0
	attempt = attempt.WithContext(httptrace.WithClientTrace(attempt.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connections++ },
	}))

	started := time.Now()
	response, err := client.Do(attempt)
	if connections > 1 {
		into.recordClientRetry()
	}
	if err != nil {
		into.recordFailure(failureKind(err))
		return
	}

	read, err := io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err != nil {
		// The answer began and did not finish: whatever its status said, the
		// client did not get it.
		into.recordFailure(cutOff)
		return
	}

	into.record(time.Since(started), response.StatusCode, response.Header.Get(backendHeader), read)
	into.recordCache(response.Header.Get(cacheHeader))
}

// failureKind groups the errors a run can produce, so that a timeout is not
// reported next to a refused connection as if they were the same problem.
func failureKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The run ended while this request was in flight.
		return "cancelled"
	case os.IsTimeout(err):
		return "timeout"
	case strings.Contains(err.Error(), "connection refused"):
		return "connection refused"
	case strings.Contains(err.Error(), "connection reset"):
		return "connection reset"
	case strings.Contains(err.Error(), "EOF"):
		return "connection closed"
	default:
		return "other: " + err.Error()
	}
}

// writeJSON saves the result for the report to quote.
func writeJSON(path string, summary result) error {
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

// text renders a result for a person watching the run.
func (r result) text() string {
	var out strings.Builder

	title := fmt.Sprintf("%d connections for %s", r.Connections, r.Duration)
	if r.Algorithm != "" {
		title += " · " + r.Algorithm
	}
	fmt.Fprintf(&out, "%s\n", title)
	fmt.Fprintf(&out, "  requests   %d (%.0f/s, %.1f MB/s)\n", r.Requests, r.Throughput, r.MegabytesPS)
	fmt.Fprintf(&out, "  latency    p50 %s   p95 %s   p99 %s   max %s\n", r.P50, r.P95, r.P99, r.Max)
	fmt.Fprintf(&out, "  statuses   %s\n", counts(r.Statuses))
	if r.Cache != nil {
		fmt.Fprintf(&out, "  cache      %d hits, %d misses (%.1f%% hit)\n", r.Cache.Hits, r.Cache.Misses, 100*r.Cache.HitRate)
	}

	if r.ClientRetries > 0 {
		fmt.Fprintf(&out, "  client     %d requests sent again by the client on a new connection\n", r.ClientRetries)
	}
	if r.Failures > 0 {
		fmt.Fprintf(&out, "  failures   %d — %s\n", r.Failures, named(withoutCancelled(r.FailureKind)))
	}
	if r.CutOff > 0 {
		fmt.Fprintf(&out, "  cut off    %d in flight when the run ended\n", r.CutOff)
	}
	if len(r.Counters) > 0 {
		fmt.Fprintf(&out, "  balancer   %s\n", named(r.Counters))
	}
	if len(r.Backends) > 0 {
		fmt.Fprintf(&out, "  backends   %s\n", shares(r.Backends, r.Requests))
	}
	return out.String()
}

// withoutCancelled drops the requests the run cut off, which are reported on
// their own line rather than among the failures.
func withoutCancelled(kinds map[string]int) map[string]int {
	rest := make(map[string]int, len(kinds))
	for kind, n := range kinds {
		if kind != cancelled {
			rest[kind] = n
		}
	}
	return rest
}

// histogramText renders the latency distribution as a bar per band, which is
// how a run with two humps is told apart from one with a long tail.
func (r result) histogramText() string {
	const width = 40

	widest := 0
	for _, band := range r.Histogram {
		if band.Requests > widest {
			widest = band.Requests
		}
	}
	if widest == 0 {
		return ""
	}

	var out strings.Builder
	fmt.Fprintf(&out, "  latency distribution of %d requests\n", r.Requests)
	for _, band := range r.Histogram {
		bar := band.Requests * width / widest
		fmt.Fprintf(&out, "  %8s  %-40s %6d  %5.1f%%\n", band.Under, strings.Repeat("#", bar),
			band.Requests, 100*float64(band.Requests)/float64(r.Requests))
	}

	if len(r.PerBackend) > 0 {
		fmt.Fprint(&out, "  per backend\n")
		for _, backend := range r.PerBackend {
			fmt.Fprintf(&out, "  %-12s %6d requests   p50 %-9s p95 %-9s max %s\n",
				backend.Backend, backend.Requests, backend.P50, backend.P95, backend.Max)
		}
	}
	return out.String()
}

// counts renders status code totals in code order.
func counts(statuses map[int]int) string {
	codes := make([]int, 0, len(statuses))
	for code := range statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)

	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, fmt.Sprintf("%d: %d", code, statuses[code]))
	}
	return strings.Join(parts, "   ")
}

// named renders string-keyed totals in name order.
func named(kinds map[string]int) string {
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s: %d", name, kinds[name]))
	}
	return strings.Join(parts, "   ")
}

// shares renders what each backend served, ordered the way the backends are
// named rather than alphabetically, so backend-10 comes last.
func shares(backends map[string]int, requests int) string {
	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return lessBackend(names[i], names[j]) })

	parts := make([]string, 0, len(names))
	for _, name := range names {
		share := 0.0
		if requests > 0 {
			share = 100 * float64(backends[name]) / float64(requests)
		}
		parts = append(parts, fmt.Sprintf("%s %d (%.1f%%)", name, backends[name], share))
	}
	return strings.Join(parts, "   ")
}

// lessBackend orders names by their trailing number when they have one, so that
// backend-9 sorts before backend-10.
func lessBackend(a, b string) bool {
	na, oka := trailingNumber(a)
	nb, okb := trailingNumber(b)
	if oka && okb && na != nb {
		return na < nb
	}
	return a < b
}

// trailingNumber returns the number after the last dash in name.
func trailingNumber(name string) (int, bool) {
	dash := strings.LastIndex(name, "-")
	if dash < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(name[dash+1:])
	return n, err == nil
}
