package proxy

import (
	"net/http"
	"slices"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// keyFor returns what a request is hashed on under consistent hashing: the
// value of the configured header, or the address the request came from. It
// returns "" for a request without one, which is then placed by load.
func keyFor(hash config.ConsistentHash) func(*http.Request) string {
	if hash.Key == config.KeyClientIP {
		return clientAddr
	}
	if name, isHeader := hash.Header(); isHeader {
		return func(r *http.Request) string { return r.Header.Get(name) }
	}
	return func(*http.Request) string { return "" }
}

// choose picks the backend for one attempt from the candidates. Under consistent
// hashing, a request with a key goes where its key belongs, and a retry, with
// the backends already tried left out, to the next in the key's order. The
// first attempt also records where the request was placed.
func (c *Core) choose(active *settings, key string, candidates []*balancer.Backend, first bool) (*balancer.Backend, error) {
	keyed, isKeyed := active.strategy.(balancer.KeyedStrategy)
	if !isKeyed {
		return active.strategy.Select(candidates)
	}
	if key == "" {
		if first {
			c.metrics.ObservePlacement(observability.PlacementKeyless)
		}
		return keyed.Select(candidates)
	}

	backend, err := keyed.SelectFor(key, candidates)
	if err == nil && first {
		c.metrics.ObservePlacement(placement(keyed.Home(key, active.backends), backend, candidates))
	}
	return backend, err
}

// placement says why a keyed request went where it did: to its home, past a
// home that was in the pool but over its bound, or past one that was not in
// the pool.
func placement(home, chosen *balancer.Backend, candidates []*balancer.Backend) string {
	switch {
	case chosen == home:
		return observability.PlacementHome
	case slices.Contains(candidates, home):
		return observability.PlacementOverloaded
	default:
		return observability.PlacementUnavailable
	}
}
