package balancer

import "github.com/berkegemenoguz/ege-balancer/internal/config"

// LeastConnections sends each request to the backend that is serving the fewest
// requests at that moment. It distributes more fairly than round robin when the
// backends need noticeably different amounts of time per request.
//
// The strategy holds no state of its own: the counters live on the backends,
// where the proxy maintains them for the lifetime of every request.
type LeastConnections struct{}

// NewLeastConnections returns a least connections strategy.
func NewLeastConnections() *LeastConnections {
	return &LeastConnections{}
}

// Select returns the least busy backend. Ties go to the earliest backend in the
// pool, which keeps the choice deterministic.
func (l *LeastConnections) Select(backends []*Backend) (*Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoBackends
	}

	least := backends[0]
	fewest := least.ActiveConnections()
	for _, backend := range backends[1:] {
		if active := backend.ActiveConnections(); active < fewest {
			least, fewest = backend, active
		}
	}
	return least, nil
}

// Name identifies the strategy in configuration and logs.
func (l *LeastConnections) Name() string {
	return string(config.LeastConnections)
}
