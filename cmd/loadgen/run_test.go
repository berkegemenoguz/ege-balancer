package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunMeasuresOnlyAfterTheWarmup(t *testing.T) {
	var served, warm atomic.Int64
	measuring := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-measuring:
			served.Add(1)
		default:
			warm.Add(1)
		}
		w.Header().Set(backendHeader, "backend-1")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// The warmup and the measurement are the same length, so the backend can
	// count the requests of each phase apart.
	const phase = 300 * time.Millisecond
	time.AfterFunc(phase, func() { close(measuring) })

	got := run(load{url: backend.URL, method: http.MethodGet}, "", 4, phase, phase, time.Second)

	if got.Requests == 0 {
		t.Fatal("the run recorded no requests")
	}
	if warm.Load() == 0 {
		t.Error("the backend saw no warmup traffic, want the warmup driven like the measurement")
	}
	if int64(got.Requests) > served.Load() {
		t.Errorf("recorded %d requests but only %d were sent after the warmup", got.Requests, served.Load())
	}
	if got.Statuses[http.StatusOK] != got.Requests {
		t.Errorf("statuses = %v, want every request answered 200", got.Statuses)
	}
	if got.Backends["backend-1"] != got.Requests {
		t.Errorf("backends = %v, want every request credited to backend-1", got.Backends)
	}
	if got.Failures != 0 {
		t.Errorf("failures = %d %v, want none", got.Failures, got.FailureKind)
	}
	if got.Connections != 4 {
		t.Errorf("connections = %d, want 4", got.Connections)
	}
}

func TestRunReportsAnUnreachableTarget(t *testing.T) {
	got := run(load{url: "http://127.0.0.1:1/", method: http.MethodGet}, "", 2, 50*time.Millisecond, 150*time.Millisecond, 200*time.Millisecond)

	if got.Requests != 0 {
		t.Errorf("recorded %d requests against a closed port, want none", got.Requests)
	}
	if got.Failures == 0 {
		t.Fatal("no failure was recorded against a closed port")
	}
	if got.FailureKind["connection refused"] == 0 {
		t.Errorf("failures = %v, want them reported as refused connections", got.FailureKind)
	}
}

func TestRunWithoutAMetricsEndpointStillReports(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	got := run(load{url: backend.URL, method: http.MethodGet}, "", 2, 50*time.Millisecond, 150*time.Millisecond, time.Second)

	if len(got.Counters) != 0 {
		t.Errorf("counters = %v, want none when no endpoint is given", got.Counters)
	}
	if got.Requests == 0 {
		t.Error("the run recorded no requests")
	}
}

func TestRunSendsTheMethodTheBodyAndTheSessions(t *testing.T) {
	const keys = 5

	var mu sync.Mutex
	methods := map[string]int{}
	sizes := map[int]int{}
	sessions := map[string]bool{}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		session := r.Header.Get(sessionHeader)

		mu.Lock()
		methods[r.Method]++
		sizes[len(body)]++
		seen := sessions[session]
		sessions[session] = true
		mu.Unlock()

		if seen {
			w.Header().Set(cacheHeader, "hit")
		} else {
			w.Header().Set(cacheHeader, "miss")
		}
	}))
	defer backend.Close()

	shape := load{url: backend.URL, method: http.MethodPost, body: []byte("12345678"), keys: keys}
	got := run(shape, "", 2, 50*time.Millisecond, 150*time.Millisecond, time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[http.MethodPost] == 0 {
		t.Errorf("methods = %v, want only POST", methods)
	}
	if len(sizes) != 1 || sizes[8] == 0 {
		t.Errorf("body sizes = %v, want every body 8 bytes", sizes)
	}
	if len(sessions) > keys {
		t.Errorf("%d sessions were named, want at most %d", len(sessions), keys)
	}
	if got.Cache == nil || got.Cache.Hits+got.Cache.Misses != got.Requests {
		t.Fatalf("cache = %+v over %d requests, want every answer counted", got.Cache, got.Requests)
	}
	if got.Cache.Hits == 0 {
		t.Error("no hits were counted, want repeat sessions remembered")
	}
}

func TestAnAnswerCutOffIsAFailureNotASuccess(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "8192")
		_, _ = w.Write(make([]byte, 4096))
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer backend.Close()

	got := run(load{url: backend.URL, method: http.MethodPost, body: []byte("x")}, "", 1,
		20*time.Millisecond, 100*time.Millisecond, time.Second)

	if got.Requests != 0 {
		t.Errorf("recorded %d answers as served, want none: every one was cut off", got.Requests)
	}
	if got.FailureKind[cutOff] == 0 {
		t.Errorf("failures = %v, want the cut-off answers counted", got.FailureKind)
	}
}

func TestRequestsTheClientSendsAgainAreCounted(t *testing.T) {
	// The backend drops every other connection before answering, which Go's
	// client answers by sending a GET again on a new connection.
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1)%2 == 0 {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	got := run(load{url: backend.URL, method: http.MethodGet}, "", 1,
		20*time.Millisecond, 150*time.Millisecond, time.Second)

	if got.ClientRetries == 0 {
		t.Errorf("no request was counted as sent again, want the client's own retries seen")
	}
}
