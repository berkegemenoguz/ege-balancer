package health

import (
	"context"
	"strconv"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
)

// registered returns a checker that knows n backends and runs no probes, so a
// benchmark measures the bookkeeping and nothing else.
func registered(b *testing.B, n int) (*HTTPChecker, []string) {
	b.Helper()

	backends := make([]*balancer.Backend, 0, n)
	addrs := make([]string, 0, n)
	for i := range n {
		addr := "backend-" + strconv.Itoa(i) + ":5678"
		backends = append(backends, &balancer.Backend{Addr: addr})
		addrs = append(addrs, addr)
	}

	ctx, cancel := context.WithCancel(b.Context())
	cancel() // registers the backends without starting their probes

	checker := New(testConfig())
	checker.Start(ctx, backends)
	return checker, addrs
}

// BenchmarkIsHealthy is the read every request makes, once per backend in the
// pool, while the proxy filters out the unhealthy ones.
func BenchmarkIsHealthy(b *testing.B) {
	checker, addrs := registered(b, 10)
	b.ReportAllocs()

	i := 0
	for b.Loop() {
		checker.IsHealthy(addrs[i%len(addrs)])
		i++
	}
}

// BenchmarkIsHealthyWhileReporting mixes in the writes the proxy makes after
// each request, one for every sixteen reads, which is where the read-write lock
// has to earn its keep.
func BenchmarkIsHealthyWhileReporting(b *testing.B) {
	checker, addrs := registered(b, 10)
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			addr := addrs[i%len(addrs)]
			if i%16 == 0 {
				checker.ReportSuccess(addr)
			} else {
				checker.IsHealthy(addr)
			}
			i++
		}
	})
}
