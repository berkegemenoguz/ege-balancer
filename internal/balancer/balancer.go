// Package balancer defines the LBStrategy interface and its implementations:
// round robin, least connections and weighted round robin. The active strategy
// is selected through configuration.
package balancer

import (
	"errors"
	"fmt"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// ErrNoBackends is returned by a strategy that was asked to choose from an
// empty set of backends. The caller answers such a request with 503.
var ErrNoBackends = errors.New("no backend available")

// Backend is one upstream server the load balancer can forward to.
type Backend struct {
	Addr   string
	Weight int
}

// LBStrategy picks the backend that serves the next request. Implementations
// are safe for concurrent use: one instance serves every connection goroutine.
type LBStrategy interface {
	// Select returns the backend to forward to, or ErrNoBackends when the pool
	// holds nothing usable.
	Select(backends []*Backend) (*Backend, error)
	// Name is the identifier the strategy is configured under.
	Name() string
}

// New builds the strategy named by the configured algorithm.
func New(algorithm config.Algorithm) (LBStrategy, error) {
	switch algorithm {
	case config.RoundRobin:
		return NewRoundRobin(), nil
	case config.LeastConnections, config.WeightedRoundRobin:
		return nil, fmt.Errorf("algorithm %s is not implemented yet", algorithm)
	default:
		return nil, fmt.Errorf("unknown algorithm %s", algorithm)
	}
}

// BackendsFromConfig converts the configured backends into the pool the
// strategies operate on.
func BackendsFromConfig(configured []config.Backend) []*Backend {
	backends := make([]*Backend, 0, len(configured))
	for _, backend := range configured {
		backends = append(backends, &Backend{Addr: backend.Addr, Weight: backend.Weight})
	}
	return backends
}
