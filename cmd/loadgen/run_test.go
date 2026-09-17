package main

import (
	"net/http"
	"net/http/httptest"
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

	got := run(backend.URL, "", 4, phase, phase, time.Second)

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
	got := run("http://127.0.0.1:1/", "", 2, 50*time.Millisecond, 150*time.Millisecond, 200*time.Millisecond)

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

	got := run(backend.URL, "", 2, 50*time.Millisecond, 150*time.Millisecond, time.Second)

	if len(got.Counters) != 0 {
		t.Errorf("counters = %v, want none when no endpoint is given", got.Counters)
	}
	if got.Requests == 0 {
		t.Error("the run recorded no requests")
	}
}
