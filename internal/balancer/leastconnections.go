package balancer

import (
	"math/rand/v2"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// LeastConnections sends each request to the less busy of two backends drawn
// at random: the power of two choices. It distributes more fairly than round
// robin when the backends need noticeably different amounts of time per
// request.
//
// Comparing two random backends instead of scanning for the least busy one
// costs the same whatever the pool size, and removes two faults of the scan:
// when every backend is idle, a scan always picks the first one, and requests
// arriving together all read the same counters and pile onto the same backend.
// A second sample keeps almost all of the balance of a full scan.
//
// The strategy holds no state of its own: the counters live on the backends,
// where the proxy maintains them for the lifetime of every request.
type LeastConnections struct {
	// intn draws an index below n. It is math/rand/v2's, which is safe for
	// concurrent use; tests replace it with a seeded source to reproduce a run.
	intn func(n int) int
}

// NewLeastConnections returns a least connections strategy.
func NewLeastConnections() *LeastConnections {
	return &LeastConnections{intn: rand.IntN}
}

// Select draws two different backends and returns the one serving fewer
// requests. On a tie it returns the first drawn, which is itself random.
func (l *LeastConnections) Select(backends []*Backend) (*Backend, error) {
	switch len(backends) {
	case 0:
		return nil, ErrNoBackends
	case 1:
		return backends[0], nil
	}

	// Draw the second index from one fewer and step over the first, so that
	// the two are always different without retrying.
	first := l.intn(len(backends))
	second := l.intn(len(backends) - 1)
	if second >= first {
		second++
	}

	if backends[second].ActiveConnections() < backends[first].ActiveConnections() {
		return backends[second], nil
	}
	return backends[first], nil
}

// Name identifies the strategy in configuration and logs.
func (l *LeastConnections) Name() string {
	return string(config.LeastConnections)
}
