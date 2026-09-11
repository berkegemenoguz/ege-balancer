package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
)

// forwarder builds the proxy in front of a real backend answering a short body.
// The upstream leg is genuine HTTP over a socket, so the transport, the
// connection pool and the response copy are all part of what is measured.
func forwarder(tb testing.TB) http.Handler {
	tb.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "backend-1")
	}))
	tb.Cleanup(backend.Close)

	backends := []*balancer.Backend{{Addr: strings.TrimPrefix(backend.URL, "http://")}}
	return New(testConfig(), balancer.NewRoundRobin(), backends, allHealthy, testMetrics())
}

// BenchmarkForward is one request through the proxy and back. Its bytes per
// operation are where a lost copy buffer pool shows up: 32 KB more, every time.
func BenchmarkForward(b *testing.B) {
	handler := forwarder(b)
	b.ReportAllocs()

	for b.Loop() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusOK {
			b.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
	}
}

// BenchmarkForwardParallel sends requests from many goroutines at once, which is
// the load under which an undersized upstream pool opens a connection per
// request instead of reusing one.
func BenchmarkForwardParallel(b *testing.B) {
	handler := forwarder(b)
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusOK {
				b.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
				return
			}
		}
	})
}
