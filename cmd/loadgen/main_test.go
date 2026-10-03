package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// timeoutError is an error that reports itself as a timeout, as the net package
// does for a request that ran out of time.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestFailureKindSeparatesTheCauses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"timed out", timeoutError{}, "timeout"},
		{"nothing listening", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), "connection refused"},
		{"peer reset", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, "connection reset"},
		{"closed early", errors.New("unexpected EOF"), "connection closed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := failureKind(test.err); got != test.want {
				t.Errorf("failureKind(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}

// TestTheClientsOwnTimeoutIsATimeout uses a real client and a real timeout:
// the error Go returns for it also matches context.DeadlineExceeded, which a
// synthetic error would not show.
func TestTheClientsOwnTimeoutIsATimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer slow.Close()

	_, err := (&http.Client{Timeout: 20 * time.Millisecond}).Get(slow.URL)
	if err == nil {
		t.Fatal("the request did not time out")
	}
	if got := failureKind(err); got != "timeout" {
		t.Errorf("failureKind(%v) = %q, want timeout", err, got)
	}
}

func TestFailureKindKeepsAnUnknownError(t *testing.T) {
	if got := failureKind(errors.New("something else entirely")); !strings.HasPrefix(got, "other: ") {
		t.Errorf("failureKind of an unknown error = %q, want it reported as it is", got)
	}
}

func TestBackendsAreOrderedByTheirNumber(t *testing.T) {
	got := shares(map[string]int{
		"backend-10": 1,
		"backend-9":  2,
		"backend-1":  7,
	}, 10)

	want := "backend-1 7 (70.0%)   backend-9 2 (20.0%)   backend-10 1 (10.0%)"
	if got != want {
		t.Errorf("shares =\n%q\nwant\n%q", got, want)
	}
}

func TestTextReportsTheRun(t *testing.T) {
	all := newSamples()
	all.record(10*time.Millisecond, 200, "backend-1", 2048)
	all.recordFailure("timeout")

	summary := summarise(all, 8, time.Second)
	summary.Algorithm = "least_connections"

	rendered := summary.text()
	for _, want := range []string{
		"8 connections for 1s · least_connections",
		"requests   1 (1/s",
		"latency    p50 10ms",
		"statuses   200: 1",
		"failures   1 — timeout: 1",
		"backends   backend-1 1 (100.0%)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the report does not contain %q:\n%s", want, rendered)
		}
	}
}
