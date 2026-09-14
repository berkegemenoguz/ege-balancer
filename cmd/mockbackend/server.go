package main

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// server serves requests the way its profile describes. A request waits for a
// worker, holds it while it is "served" for a sampled time, and then succeeds
// or fails. Latency is only spent while holding a worker, so a backend with
// little capacity slows down under load as requests queue, the way a real one
// does.
type server struct {
	profile profile
	sampler *sampler

	// slots holds one token per request being served; it is nil when capacity
	// is unlimited.
	slots   chan struct{}
	waiting atomic.Int64

	body []byte
}

// newServer returns a server for p, drawing from a generator seeded with seed.
func newServer(p profile, seed uint64) *server {
	s := &server{
		profile: p,
		sampler: newSampler(p, seed),
		body:    bodyFor(p.name, p.bodySize),
	}
	if p.capacity > 0 {
		s.slots = make(chan struct{}, p.capacity)
	}
	return s
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
// traffic would time out and report the backend dead when it is only busy.
func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Backend", s.profile.name)
	if s.profile.queue > 0 && 2*s.waiting.Load() > int64(s.profile.queue) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, s.profile.name+"\n")
}

// serve answers one request according to the profile.
func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Backend", s.profile.name)

	release, admitted := s.admit(r.Context())
	if !admitted {
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
		return
	}
	defer release()

	work := time.NewTimer(s.sampler.latency())
	defer work.Stop()
	select {
	case <-work.C:
	case <-r.Context().Done():
		return
	}

	if s.sampler.fails(s.profile.errorRate) {
		http.Error(w, s.profile.name+" failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
	_, _ = w.Write(s.body)
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
