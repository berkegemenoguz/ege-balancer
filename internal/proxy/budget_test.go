package proxy

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// TestForwardingAllocatesLessThanACopyBuffer guards the second fix from the load
// test. Without pooled copy buffers the reverse proxy allocates a fresh 32 KB
// buffer for every response. Nothing leaks and no functional test notices, but
// the garbage collector does: it was 80% of all allocation under load.
//
// The count of allocations does not move when that happens — it is still one
// slice, only a large one — so the guard is on bytes. A request that allocates
// a whole copy buffer's worth of memory means that buffer is back.
func TestForwardingAllocatesLessThanACopyBuffer(t *testing.T) {
	const requests = 500

	handler := forwarder(t)

	// The first requests open the upstream connections, a one-off cost this
	// test is not about.
	for range 20 {
		forward(t, handler)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range requests {
		forward(t, handler)
	}
	runtime.ReadMemStats(&after)

	perRequest := (after.TotalAlloc - before.TotalAlloc) / requests
	t.Logf("%d bytes allocated per forwarded request", perRequest)

	if perRequest >= copyBufferSize {
		t.Errorf("forwarding allocates %d bytes per request, at least a whole copy buffer "+
			"of %d: the response copy buffers are no longer being reused", perRequest, copyBufferSize)
	}
}

// forward sends one request through handler and fails unless it succeeds.
func forward(tb testing.TB, handler http.Handler) {
	tb.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		tb.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
}
