package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
)

// TestUpstreamConnectionsAreReused guards the largest defect this project has
// had. Go's default transport keeps two idle connections per host, so under
// concurrent load the proxy opened a new connection for nearly every request,
// ran out of ephemeral ports, and failed. No other test noticed; only the load
// test did. This one counts the connections the backend actually accepts.
func TestUpstreamConnectionsAreReused(t *testing.T) {
	const (
		rounds = 5
		// Below the per-backend idle pool, so every connection of one round
		// can be kept for the next.
		concurrency = 24
	)

	var opened atomic.Int64
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	backend.Start()
	defer backend.Close()

	backends := []*balancer.Backend{{Addr: strings.TrimPrefix(backend.URL, "http://")}}
	handler := New(testConfig(), balancer.NewRoundRobin(), backends, allHealthy, testMetrics())

	for range rounds {
		var wg sync.WaitGroup
		for range concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			}()
		}
		wg.Wait()
	}

	// The first round may open one connection per concurrent request; every
	// later round must find them waiting. A pool that keeps only a couple
	// would open close to a full round's worth again each time.
	if got, limit := opened.Load(), int64(2*concurrency); got > limit {
		t.Errorf("the backend accepted %d connections over %d rounds of %d requests, "+
			"want at most %d: connections are not being reused", got, rounds, concurrency, limit)
	}
}
