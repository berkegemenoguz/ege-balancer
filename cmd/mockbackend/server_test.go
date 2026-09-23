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

// serveOverHTTP starts s on a real socket. The tests of hanging, dropped and
// dripped answers need one: a recorder cannot show a connection closing.
func serveOverHTTP(t *testing.T, s *server) string {
	t.Helper()
	listening := httptest.NewServer(s.handler())
	t.Cleanup(func() {
		s.stop()
		listening.Close()
	})
	return listening.URL
}

// sendAs sends one request naming session and returns what the backend said
// about its cache, and how long the answer took.
func sendAs(s *server, session string) (string, time.Duration) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(sessionHeader, session)
	recorder := httptest.NewRecorder()

	started := time.Now()
	s.handler().ServeHTTP(recorder, request)
	return recorder.Header().Get("X-Cache"), time.Since(started)
}

func TestARememberedSessionIsCheaper(t *testing.T) {
	s := newServer(profile{name: "backend-1", cacheSize: 10, missPenalty: 60 * time.Millisecond}, 1)

	if cached, took := sendAs(s, "user-1"); cached != "miss" || took < 60*time.Millisecond {
		t.Errorf("first request: X-Cache %q in %s, want a miss costing the 60ms penalty", cached, took)
	}
	if cached, took := sendAs(s, "user-1"); cached != "hit" || took >= 30*time.Millisecond {
		t.Errorf("second request: X-Cache %q in %s, want a hit without the penalty", cached, took)
	}
}

func TestWithoutASessionNothingIsRemembered(t *testing.T) {
	s := newServer(profile{name: "backend-1", cacheSize: 10, missPenalty: time.Millisecond}, 1)

	recorder := httptest.NewRecorder()
	s.handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := recorder.Header().Get("X-Cache"); got != "" {
		t.Errorf("X-Cache = %q for a request with no session, want none", got)
	}
	if got := s.cache.size(); got != 0 {
		t.Errorf("cache holds %d sessions, want none", got)
	}
}

func TestAHangingRequestHoldsItsWorkerUntilTheClientGivesUp(t *testing.T) {
	s := newServer(profile{name: "backend-3", capacity: 1}, 1)
	s.faults.inject(modeHang, fault{rate: 1}, time.Minute)
	url := serveOverHTTP(t, s)

	gaveUp := make(chan error, 1)
	go func() {
		_, err := (&http.Client{Timeout: 300 * time.Millisecond}).Get(url)
		gaveUp <- err
	}()
	eventually(t, func() bool { return len(s.slots) == 1 }, "the hanging request holds the worker")

	// With the only worker held and no queue, the next request is refused.
	if response, err := http.Get(url); err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second request = %v %v, want 503 while the worker is held", response, err)
	} else {
		_ = response.Body.Close()
	}

	if err := <-gaveUp; err == nil {
		t.Error("the hanging request was answered, want the client to give up")
	}
	eventually(t, func() bool { return len(s.slots) == 0 }, "the worker is released once the client gives up")

	s.faults.clear()
	if response, err := http.Get(url); err != nil || response.StatusCode != http.StatusOK {
		t.Errorf("after the fault = %v %v, want 200", response, err)
	} else {
		_ = response.Body.Close()
	}
}

func TestAHangingRequestDoesNotHoldUpTheHealthCheck(t *testing.T) {
	s := newServer(profile{name: "backend-3", capacity: 1}, 1)
	s.faults.inject(modeHang, fault{rate: 1}, time.Minute)
	url := serveOverHTTP(t, s)

	go func() { _, _ = (&http.Client{Timeout: 500 * time.Millisecond}).Get(url) }()
	eventually(t, func() bool { return len(s.slots) == 1 }, "the hanging request holds the worker")

	started := time.Now()
	if status, _, _ := get(t, s, "/healthz"); status != http.StatusOK {
		t.Errorf("health = %d while a request hangs, want 200", status)
	}
	if took := time.Since(started); took > 100*time.Millisecond {
		t.Errorf("health took %s, want it unaffected by the hanging request", took)
	}
}

func TestStoppingDropsHangingRequests(t *testing.T) {
	s := newServer(profile{name: "backend-3"}, 1)
	s.faults.inject(modeHang, fault{rate: 1}, time.Minute)
	listening := httptest.NewServer(s.handler())
	defer listening.Close()

	answered := make(chan error, 1)
	go func() {
		response, err := http.Get(listening.URL)
		if err == nil {
			_ = response.Body.Close()
		}
		answered <- err
	}()
	time.Sleep(50 * time.Millisecond)
	s.stop()

	select {
	case err := <-answered:
		if err == nil {
			t.Error("the hanging request was answered on stop, want its connection dropped")
		}
	case <-time.After(time.Second):
		t.Fatal("the hanging request outlived stop")
	}
}

func TestADroppedAnswerEndsBeforeItsLength(t *testing.T) {
	s := newServer(profile{name: "backend-10", bodySize: 4096}, 1)
	s.faults.inject(modeReset, fault{rate: 1}, time.Minute)

	response, err := http.Get(serveOverHTTP(t, s))
	if err != nil {
		t.Fatalf("request failed before the answer began: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err == nil {
		t.Error("the answer was read to the end, want the connection dropped part way")
	}
	if len(body) != 2048 {
		t.Errorf("received %d bytes, want the first half of the 4096 promised", len(body))
	}
}

func TestADrippedAnswerArrivesWholeButSlowly(t *testing.T) {
	s := newServer(profile{name: "backend-1", bodySize: 1000}, 1)
	s.faults.inject(modeDrip, fault{rate: 1, over: 200 * time.Millisecond}, time.Minute)

	started := time.Now()
	response, err := http.Get(serveOverHTTP(t, s))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	took := time.Since(started)

	if err != nil || len(body) != 1000 || !strings.HasPrefix(string(body), "backend-1\n") {
		t.Errorf("received %d bytes (%v), want the whole 1000 byte answer", len(body), err)
	}
	// Ten pieces with a pause between each: nine pauses of 20ms.
	if took < 180*time.Millisecond {
		t.Errorf("the answer took %s, want it spread over about 200ms", took)
	}
}

func TestAnInjectedErrorAnswers500(t *testing.T) {
	s := newServer(profile{name: "backend-2"}, 1)
	s.faults.inject(modeError, fault{rate: 1}, time.Minute)

	if status, _, _ := get(t, s, "/"); status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
}

func TestSlowMultipliesTheServiceTime(t *testing.T) {
	s := newServer(profile{name: "backend-2", latency: 20 * time.Millisecond}, 1)
	s.faults.inject(modeSlow, fault{factor: 4}, time.Minute)

	started := time.Now()
	get(t, s, "/")
	if took := time.Since(started); took < 80*time.Millisecond {
		t.Errorf("the request took %s, want four times the 20ms latency", took)
	}
}

func TestAColdBackendIsSlower(t *testing.T) {
	s := newServer(profile{name: "backend-2", latency: 20 * time.Millisecond,
		coldStart: time.Minute, coldFactor: 4}, 1)

	started := time.Now()
	get(t, s, "/")
	if took := time.Since(started); took < 70*time.Millisecond {
		t.Errorf("the first request took %s, want close to four times the 20ms latency", took)
	}
}

func TestAFrozenBackendDoesNotAnswerEvenItsHealthCheck(t *testing.T) {
	s := newServer(profile{name: "backend-2"}, 1)
	s.clock.freeze(80 * time.Millisecond)

	started := time.Now()
	if status, _, _ := get(t, s, "/healthz"); status != http.StatusOK {
		t.Errorf("health = %d after the freeze, want 200", status)
	}
	if took := time.Since(started); took < 80*time.Millisecond {
		t.Errorf("health answered after %s, want it to wait out the 80ms freeze", took)
	}
}

func TestAHangingPostIsReleasedWhenTheClientGivesUp(t *testing.T) {
	s := newServer(profile{name: "backend-3", capacity: 1}, 1)
	s.faults.inject(modeHang, fault{rate: 1}, time.Minute)
	url := serveOverHTTP(t, s)

	client := &http.Client{Timeout: 200 * time.Millisecond}
	if _, err := client.Post(url, "text/plain", strings.NewReader(strings.Repeat("x", 256))); err == nil {
		t.Fatal("the hanging POST was answered, want the client to give up")
	}

	// Until the body was read the server could not see the client leave, and
	// the worker stayed held after the fault had ended.
	eventually(t, func() bool { return len(s.slots) == 0 }, "the worker is released once the client gives up")
}
