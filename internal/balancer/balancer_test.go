package balancer

import (
	"errors"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

func TestNewBuildsConfiguredStrategy(t *testing.T) {
	strategy, err := New(config.RoundRobin)
	if err != nil {
		t.Fatalf("New returned an unexpected error: %v", err)
	}
	if got := strategy.Name(); got != string(config.RoundRobin) {
		t.Errorf("Name() = %q, want %q", got, config.RoundRobin)
	}
}

func TestNewRejectsUnavailableStrategies(t *testing.T) {
	tests := []struct {
		name      string
		algorithm config.Algorithm
		wantErr   string
	}{
		{"least connections", config.LeastConnections, "not implemented yet"},
		{"weighted round robin", config.WeightedRoundRobin, "not implemented yet"},
		{"unknown", config.Algorithm("random"), "unknown algorithm"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			strategy, err := New(test.algorithm)
			if err == nil {
				t.Fatalf("New returned %v, want an error", strategy)
			}
			if got := err.Error(); !strings.Contains(got, test.wantErr) {
				t.Errorf("error = %q, want it to mention %q", got, test.wantErr)
			}
		})
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
		t.Errorf("backends[1] = %+v, want the second configured backend", *backends[1])
	}
}

func TestSelectOnEmptyPool(t *testing.T) {
	backend, err := NewRoundRobin().Select(nil)
	if !errors.Is(err, ErrNoBackends) {
		t.Errorf("error = %v, want ErrNoBackends", err)
	}
	if backend != nil {
		t.Errorf("backend = %+v, want nil", *backend)
	}
}
