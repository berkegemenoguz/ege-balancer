package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// countingHandler records how many requests reached it.
func countingHandler(served *int) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { *served++ })
}

// requestFrom builds a request that appears to come from addr.
func requestFrom(addr string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = addr + ":44444"
	return request
}

func TestRateLimitRejectsAboveTheConfiguredRate(t *testing.T) {
	const limit = 3

	var served int
	handler := newRateLimiter(limit).wrap(countingHandler(&served))

	for i := range limit {
		if response := send(handler, requestFrom("203.0.113.7")); response.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d", i+1, response.Code, http.StatusOK)
		}
	}

	response := send(handler, requestFrom("203.0.113.7"))
	if response.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d once the burst is spent",
			response.Code, http.StatusTooManyRequests)
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
	if served != limit {
		t.Errorf("%d requests reached the backend, want %d", served, limit)
	}
}

func TestRateLimitIsPerClient(t *testing.T) {
	var served int
	handler := newRateLimiter(1).wrap(countingHandler(&served))

	for _, client := range []string{"203.0.113.7", "198.51.100.4", "192.0.2.9"} {
		if response := send(handler, requestFrom(client)); response.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want %d", client, response.Code, http.StatusOK)
		}
	}
	if served != 3 {
		t.Errorf("%d requests reached the backend, want one per client", served)
	}
}

// spend takes n tokens and fails the test if any of them is refused.
func spend(t *testing.T, limiter *rateLimiter, at time.Time, n int) {
	t.Helper()

	for i := range n {
		if !limiter.allow("203.0.113.7", at) {
			t.Fatalf("token %d of %d was rejected", i+1, n)
		}
	}
}

func TestRateLimitRefillsOverTime(t *testing.T) {
	limiter := newRateLimiter(2)
	start := time.Now()

	spend(t, limiter, start, 2)
	if limiter.allow("203.0.113.7", start) {
		t.Fatal("a third request was allowed within the same instant")
	}

	// Half a second refills one of the two tokens per second.
	if !limiter.allow("203.0.113.7", start.Add(500*time.Millisecond)) {
		t.Error("the bucket did not refill over time")
	}
}

func TestRateLimitDoesNotAccumulateBeyondTheBurst(t *testing.T) {
	limiter := newRateLimiter(2)
	start := time.Now()

	// A minute of silence must not buy more than one full burst.
	later := start.Add(time.Minute)
	spend(t, limiter, start, 1)
	spend(t, limiter, later, 2)
	if limiter.allow("203.0.113.7", later) {
		t.Error("the bucket grew past its burst size while idle")
	}
}

func TestRateLimitDisabledWhenZero(t *testing.T) {
	var served int
	handler := newRateLimiter(0).wrap(countingHandler(&served))

	for range 50 {
		if response := send(handler, requestFrom("203.0.113.7")); response.Code != http.StatusOK {
			t.Fatalf("status = %d, want the limiter disabled", response.Code)
		}
	}
	if served != 50 {
		t.Errorf("%d requests reached the backend, want all 50", served)
	}
}

func TestSweepDropsIdleClients(t *testing.T) {
	limiter := newRateLimiter(1)
	start := time.Now()

	limiter.allow("203.0.113.7", start)
	limiter.sweep(start.Add(idleBucketTTL + time.Second))

	if len(limiter.buckets) != 0 {
		t.Errorf("%d buckets left after the sweep, want none", len(limiter.buckets))
	}
}
