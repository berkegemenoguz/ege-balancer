package main

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// sessionHeader names the client a request comes from, for the backend's
// cache; the answer says in X-Cache whether that client was remembered.
const sessionHeader = "X-Session"

// dripPieces is how many pieces a dripped answer is sent in.
const dripPieces = 10

// server serves requests the way its profile describes. A request waits for a
// worker, holds it while it is "served" for a sampled time, and then succeeds
// or fails. Latency is only spent while holding a worker, so a backend with
// little capacity slows down under load as requests queue, the way a real one
// does. On top of that sit what it remembers about its clients, how time
// slows it — a cold start, pauses, freezes — and the faults in force.
type server struct {
	profile profile
	sampler *sampler
	faults  *faults
	clock   *clock
	cache   *cache // nil when the backend remembers nothing

	// slots holds one token per request being served; it is nil when capacity
	// is unlimited.
	slots   chan struct{}
	waiting atomic.Int64

	// stopping is closed when the process shuts down. A graceful shutdown waits
	// for requests in flight, and a hanging request would otherwise make it
	// wait for ever.
	stopping chan struct{}
	stopOnce sync.Once

	body []byte
}

// newServer returns a server for p, drawing from a generator seeded with seed.
func newServer(p profile, seed uint64) *server {
	s := &server{
		profile:  p,
		sampler:  newSampler(p, seed),
		faults:   newFaults(p, time.Now),
		clock:    newClock(p, time.Now),
		stopping: make(chan struct{}),
		body:     bodyFor(p.name, p.bodySize),
	}
	if p.capacity > 0 {
		s.slots = make(chan struct{}, p.capacity)
	}
	if p.cacheSize > 0 {
		s.cache = newCache(p.cacheSize)
	}
	if p.pauseEvery > 0 {
		s.clock.phase = time.Duration(s.sampler.draw() * float64(p.pauseEvery))
	}
	return s
}

// stop releases the requests that are hanging, so that a shutdown can finish.
func (s *server) stop() {
	s.stopOnce.Do(func() { close(s.stopping) })
}

// bodyFor is the answer body: the backend's name on the first line, padded to
// size. The name comes first so that a person reading the answer, or a script
// taking its first line, sees who served it.
func bodyFor(name string, size int) []byte {
	head := name + "\n"
	if size <= len(head) {
		return []byte(head)
	}

	const pattern = "0123456789abcdefghijklmnopqrstuvwxyz"
	body := make([]byte, size)
	copy(body, head)
	for i := len(head); i < size; i++ {
		body[i] = pattern[(i-len(head))%len(pattern)]
	}
	// A final newline keeps a terminal prompt off the end of the padding.
	body[size-1] = '\n'
	return body
}

// handler routes the health check and everything else.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/", s.serve)
	return mux
}

// health reports the backend unhealthy while more than half of its queue is
// full. It skips the queue and the latency: a probe that waited behind real
// traffic would time out and report the backend dead when it is only busy. It
// does not skip a pause or a freeze, which stop the whole process.
func (s *server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Backend", s.profile.name)
	if !s.clock.wait(r.Context()) {
		return
	}
	if s.profile.queue > 0 && 2*s.waiting.Load() > int64(s.profile.queue) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, s.profile.name+"\n")
}

// serve answers one request according to the profile and the faults in force.
func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Backend", s.profile.name)
	ctx := r.Context()

	// A stopped process does not take the request in.
	if !s.clock.wait(ctx) {
		return
	}

	release, admitted := s.admit(ctx)
	if !admitted {
		if ctx.Err() != nil {
			return
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
		return
	}
	defer release()

	// The request is read in full before anything else, as a server that
	// parses it would. It matters most for a request that is about to hang:
	// until its body has been read, the server cannot notice the client going
	// away, and the hang would hold its worker for good.
	_, _ = io.Copy(io.Discard, r.Body)

	chosen, ft := s.choose()
	if chosen == modeHang {
		s.hang(ctx)
		return
	}

	if !sleep(ctx, s.serviceTime(w, r)) {
		return
	}
	// A pause that began while the request was served delays its answer too.
	if !s.clock.wait(ctx) {
		return
	}

	switch chosen {
	case modeError:
		http.Error(w, s.profile.name+" failed", http.StatusInternalServerError)
	case modeReset:
		s.dropMidway(w)
	case modeDrip:
		s.drip(ctx, w, ft.over)
	default:
		s.write(w)
	}
}

// choose decides the fate of one request. A backend with no fault in force
// spends no random draw on it, so its latencies are the same sequence they
// were before faults existed.
func (s *server) choose() (mode, fault) {
	if !s.faults.possible() {
		return "", fault{}
	}
	return s.faults.choose(s.sampler.draw())
}

// serviceTime is how long this request takes to serve: a sampled latency, made
// longer by a cold start and by the slow fault, and by a cache miss when the
// client names its session.
func (s *server) serviceTime(w http.ResponseWriter, r *http.Request) time.Duration {
	took := float64(s.sampler.latency()) * s.clock.factor() * s.faults.slowdown()

	if key := r.Header.Get(sessionHeader); key != "" && s.cache != nil {
		if s.cache.seen(key) {
			w.Header().Set("X-Cache", "hit")
		} else {
			w.Header().Set("X-Cache", "miss")
			took += float64(s.profile.missPenalty)
		}
	}
	return time.Duration(took)
}

// hang holds the request, and its worker, until the client gives up, as a
// thread stuck on a lock would. If the process shuts down first the connection
// is dropped, since a hung request has no answer to finish.
func (s *server) hang(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-s.stopping:
		panic(http.ErrAbortHandler)
	}
}

// write sends the whole answer.
func (s *server) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
	_, _ = w.Write(s.body)
}

// dropMidway announces the whole answer, sends half of it and drops the
// connection, as a process that crashes while answering does. The client sees
// the connection close before the promised length has arrived.
func (s *server) dropMidway(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
	_, _ = w.Write(s.body[:len(s.body)/2])
	_ = http.NewResponseController(w).Flush()
	panic(http.ErrAbortHandler)
}

// drip sends the answer in pieces spread over the given time, as a backend
// streaming from a slow disk or over a congested link would.
func (s *server) drip(ctx context.Context, w http.ResponseWriter, over time.Duration) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
	flusher := http.NewResponseController(w)

	piece := max(1, (len(s.body)+dripPieces-1)/dripPieces)
	step := over / dripPieces
	for sent := 0; sent < len(s.body); sent += piece {
		end := min(sent+piece, len(s.body))
		if _, err := w.Write(s.body[sent:end]); err != nil {
			return
		}
		_ = flusher.Flush()
		if end < len(s.body) && !sleep(ctx, step) {
			return
		}
	}
}

// sleep waits for d and reports whether ctx was still alive at the end.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// admit waits for a worker and returns the function that gives it back. It
// reports false when the queue is already full or the client gave up waiting.
func (s *server) admit(ctx context.Context) (func(), bool) {
	if s.slots == nil {
		return func() {}, true
	}
	release := func() { <-s.slots }

	select {
	case s.slots <- struct{}{}:
		return release, true
	default:
	}

	if s.waiting.Add(1) > int64(s.profile.queue) {
		s.waiting.Add(-1)
		return nil, false
	}
	defer s.waiting.Add(-1)

	select {
	case s.slots <- struct{}{}:
		return release, true
	case <-ctx.Done():
		return nil, false
	}
}
