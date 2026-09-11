package balancer

import (
	"strconv"
	"testing"
)

// A strategy runs once per request on every connection goroutine, so what it
// costs per selection and how it behaves under contention both matter.

// strategies builds a fresh instance of every algorithm, so that one benchmark
// never inherits another's state.
var strategies = []struct {
	name  string
	build func() LBStrategy
}{
	{"round_robin", func() LBStrategy { return NewRoundRobin() }},
	{"least_connections", func() LBStrategy { return NewLeastConnections() }},
	{"weighted_round_robin", func() LBStrategy { return NewWeightedRoundRobin() }},
}

// poolSizes covers the mock environment and pools ten and a hundred times its
// size, where a strategy that walks the whole pool shows it and one that does
// not stays flat.
var poolSizes = []int{10, 100, 1000}

func BenchmarkSelect(b *testing.B) {
	for _, strategy := range strategies {
		for _, size := range poolSizes {
			b.Run(strategy.name+"/"+strconv.Itoa(size), func(b *testing.B) {
				backends, selector := pool(size), strategy.build()
				b.ReportAllocs()
				for b.Loop() {
					if _, err := selector.Select(backends); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkSelectParallel measures the strategies the way the proxy uses them:
// many goroutines selecting from one shared instance at once.
func BenchmarkSelectParallel(b *testing.B) {
	for _, strategy := range strategies {
		b.Run(strategy.name, func(b *testing.B) {
			backends, selector := pool(10), strategy.build()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := selector.Select(backends); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
