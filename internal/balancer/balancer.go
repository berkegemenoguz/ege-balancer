// Package balancer defines the LBStrategy interface and its implementations:
// round robin, least connections and weighted round robin. The active strategy
// is selected through configuration.
package balancer

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// ErrNoBackends is returned by a strategy that was asked to choose from an
// empty set of backends. The caller answers such a request with 503.
var ErrNoBackends = errors.New("no backend available")

// Backend is one upstream server the load balancer can forward to. A Backend is
// shared by every connection goroutine, so it is always held by pointer and its
// mutable state is accessed atomically.
type Backend struct {
	Addr string

	// weight is changed by a reload while requests are being balanced on it.
	weight atomic.Int64
	// active counts the requests currently being served by this backend.
	active atomic.Int64
}

// NewBackend returns a backend at addr with the given weight.
func NewBackend(addr string, weight int) *Backend {
	backend := &Backend{Addr: addr}
	backend.SetWeight(weight)
	return backend
}

// Weight is the backend's share under weighted round robin.
func (b *Backend) Weight() int {
	return int(b.weight.Load())
}

// SetWeight changes the backend's share, safely while it is being balanced on.
func (b *Backend) SetWeight(weight int) {
	b.weight.Store(int64(weight))
}

// Acquire records that a request has been handed to the backend.
func (b *Backend) Acquire() {
	b.active.Add(1)
}

// Release records that a request the backend was serving has finished.
func (b *Backend) Release() {
	b.active.Add(-1)
}

// ActiveConnections is the number of requests the backend is serving now.
func (b *Backend) ActiveConnections() int64 {
	return b.active.Load()
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
	case config.LeastConnections:
		return NewLeastConnections(), nil
	case config.WeightedRoundRobin:
		return NewWeightedRoundRobin(), nil
	default:
		return nil, fmt.Errorf("unknown algorithm %s", algorithm)
	}
}

// BackendsFromConfig converts the configured backends into the pool the
// strategies operate on.
func BackendsFromConfig(configured []config.Backend) []*Backend {
	return MergeBackends(nil, configured)
}

// MergeBackends builds the pool for a new configuration, reusing the existing
// Backend for any address that survives the change.
//
// Reuse matters on a reload: the connection counters live on these objects, so
// replacing a backend that is still serving would lose track of the requests in
// flight and mislead least connections until they finished.
func MergeBackends(existing []*Backend, configured []config.Backend) []*Backend {
	known := make(map[string]*Backend, len(existing))
	for _, backend := range existing {
		known[backend.Addr] = backend
	}

	backends := make([]*Backend, 0, len(configured))
	for _, wanted := range configured {
		if backend, kept := known[wanted.Addr]; kept {
			backend.SetWeight(wanted.Weight)
			backends = append(backends, backend)
			continue
		}
		backends = append(backends, NewBackend(wanted.Addr, wanted.Weight))
	}
	return backends
}
