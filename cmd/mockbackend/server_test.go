package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// get sends one request to the server and returns its status, backend header
// and body.
func get(t *testing.T, s *server, path string) (int, string, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	s.handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(recorder.Body)
	return recorder.Code, recorder.Header().Get("X-Backend"), string(body)
}

// eventually waits up to a second for condition to hold.
func eventually(t *testing.T, condition func() bool, describe string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", describe)
}

func TestAProfileWithOnlyANameAnswersLikeHTTPEcho(t *testing.T) {
	status, backend, body := get(t, newServer(profile{name: "backend-1"}, 1), "/")

	if status != http.StatusOK || backend != "backend-1" || body != "backend-1\n" {
		t.Errorf("answered %d, X-Backend %q, body %q; want 200, backend-1 and the name alone",
			status, backend, body)
	}
}

func TestTheBodyIsPaddedWithTheNameFirst(t *testing.T) {
	_, _, body := get(t, newServer(profile{name: "backend-10", bodySize: 4096}, 1), "/")

	if len(body) != 4096 {
		t.Errorf("body is %d bytes, want 4096", len(body))
	}
	if first, _, _ := strings.Cut(body, "\n"); first != "backend-10" {
		t.Errorf("first line = %q, want the backend's name", first)
	}
}

func TestRequestsTakeTheProfilesLatency(t *testing.T) {
	s := newServer(profile{name: "backend-1", latency: 30 * time.Millisecond}, 1)

	started := time.Now()
	get(t, s, "/")
	if took := time.Since(started); took < 30*time.Millisecond {
		t.Errorf("the request took %s, want at least the 30ms latency", took)
	}
}

func TestAFullBackendQueuesAndThenRefuses(t *testing.T) {
	s := newServer(profile{name: "backend-9", capacity: 1, queue: 1}, 1)

	// Another request holds the only worker.
	s.slots <- struct{}{}

	queued := make(chan int, 1)
	go func() {
		status, _, _ := get(t, s, "/")
		queued <- status
	}()
	eventually(t, func() bool { return s.waiting.Load() == 1 }, "the second request is queued")

	if status, _, _ := get(t, s, "/"); status != http.StatusServiceUnavailable {
		t.Errorf("a request beyond the queue got %d, want 503 at once", status)
	}

	<-s.slots
	if status := <-queued; status != http.StatusOK {
		t.Errorf("the queued request got %d once a worker was free, want 200", status)
	}
}

func TestAClientThatGivesUpLeavesTheQueue(t *testing.T) {
	s := newServer(profile{name: "backend-9", capacity: 1, queue: 1}, 1)
	s.slots <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		s.handler().ServeHTTP(httptest.NewRecorder(), request)
	}()
	eventually(t, func() bool { return s.waiting.Load() == 1 }, "the request is queued")

	cancel()
	<-done
	if got := s.waiting.Load(); got != 0 {
		t.Errorf("%d requests still counted as waiting after the client left, want 0", got)
	}
}

func TestHealthReportsAnOverloadedBackend(t *testing.T) {
	s := newServer(profile{name: "backend-9", capacity: 1, queue: 2}, 1)

	if status, _, _ := get(t, s, "/healthz"); status != http.StatusOK {
		t.Fatalf("an idle backend reported %d, want 200", status)
	}

	// More than half of the queue is waiting.
	s.waiting.Store(2)
	if status, _, _ := get(t, s, "/healthz"); status != http.StatusServiceUnavailable {
		t.Errorf("an overloaded backend reported %d, want 503", status)
	}
}

func TestHealthIgnoresTheLatency(t *testing.T) {
	s := newServer(profile{name: "backend-9", latency: time.Second}, 1)

	started := time.Now()
	get(t, s, "/healthz")
	if took := time.Since(started); took > 100*time.Millisecond {
		t.Errorf("the health check took %s, want it answered without the latency", took)
	}
}

func TestTheErrorRateDecidesTheAnswer(t *testing.T) {
	if status, backend, _ := get(t, newServer(profile{name: "backend-9", errorRate: 1}, 1), "/"); status != http.StatusInternalServerError || backend != "backend-9" {
		t.Errorf("with an error rate of 1 the answer was %d from %q, want 500 from backend-9", status, backend)
	}
	if status, _, _ := get(t, newServer(profile{name: "backend-9"}, 1), "/"); status != http.StatusOK {
		t.Errorf("with no error rate the answer was %d, want 200", status)
	}
}
