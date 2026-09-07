package balancer

import (
	"errors"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

func TestNewBuildsEveryConfiguredStrategy(t *testing.T) {
	for _, algorithm := range []config.Algorithm{
		config.RoundRobin, config.LeastConnections, config.WeightedRoundRobin,
	} {
		t.Run(string(algorithm), func(t *testing.T) {
			strategy, err := New(algorithm)
			if err != nil {
				t.Fatalf("New returned an unexpected error: %v", err)
			}
			if got := strategy.Name(); got != string(algorithm) {
				t.Errorf("Name() = %q, want %q", got, algorithm)
			}
		})
	}
}

func TestNewRejectsUnknownAlgorithm(t *testing.T) {
	strategy, err := New(config.Algorithm("random"))
	if err == nil {
		t.Fatalf("New returned %s, want an error", strategy.Name())
	}
	if !strings.Contains(err.Error(), "unknown algorithm") {
		t.Errorf("error = %v, want it to mention the unknown algorithm", err)
	}
}

func TestBackendsFromConfig(t *testing.T) {
	backends := BackendsFromConfig([]config.Backend{
		{Addr: "backend-1:5678", Weight: 1},
		{Addr: "backend-2:5678", Weight: 3},
	})

	if got, want := len(backends), 2; got != want {
		t.Fatalf("len = %d, want %d", got, want)
	}
	if backends[1].Addr != "backend-2:5678" || backends[1].Weight != 3 {
		t.Errorf("backends[1] = %s weight %d, want backend-2:5678 weight 3",
			backends[1].Addr, backends[1].Weight)
	}
}

func TestSelectOnEmptyPool(t *testing.T) {
	backend, err := NewRoundRobin().Select(nil)
	if !errors.Is(err, ErrNoBackends) {
		t.Errorf("error = %v, want ErrNoBackends", err)
	}
	if backend != nil {
		t.Errorf("backend = %s, want nil", backend.Addr)
	}
}
