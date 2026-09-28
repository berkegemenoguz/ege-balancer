package balancer

import (
	"cmp"
	"math"
	"slices"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// KeyedStrategy is a strategy that can place a request by a key, so that every
// request with the same key reaches the same backend. A request without a key
// is placed by Select, as under any other strategy.
type KeyedStrategy interface {
	LBStrategy
	// SelectFor returns the backend that serves key among backends.
	SelectFor(key string, backends []*Backend) (*Backend, error)
	// Home returns the backend key belongs to among backends, whatever their
	// load, or nil when there are none. Comparing it with what SelectFor chose
	// tells whether a request stayed at home.
	Home(key string, backends []*Backend) *Backend
}

// ConsistentHash places each request by its key with weighted rendezvous
// hashing, also called highest random weight: every backend scores the key,
// and the highest score serves it. The score is a hash of the key and the
// backend's address turned into a draw from an exponential distribution scaled
// by the backend's weight, so that each backend wins a share of the keys in
// proportion to its weight.
//
// Nothing is stored, so there is nothing to rebuild when the pool changes, and
// a change moves as few keys as it can: a backend that leaves the pool takes
// only its own keys with it, and a backend that joins takes only the keys it
// now scores highest on. A backend that is unhealthy, or was already tried for
// this request, is simply not among the candidates, and the key goes to its
// next highest score — so a retry, and a failover, land on the same second
// choice every time.
//
// With a balance factor the load is bounded, as in consistent hashing with
// bounded loads (Mirrokni, Thorup and Zadimoghaddam, 2016): a backend already
// serving its share of the requests in flight times the factor passes the key
// on to the next in its order. A popular key then spreads over a few backends
// instead of overloading one.
//
// Scoring every backend costs O(n) per request, against the O(1) of a ring
// lookup; for the tens of backends this balancer is built for, that is a few
// hundred nanoseconds, and it buys exact weights, minimal disruption and no
// state.
type ConsistentHash struct {
	// balanceFactor is the bound in per cent; 0 leaves the load unbounded.
	balanceFactor int
	// keyless places requests that carry no key.
	keyless *LeastConnections
}

// NewConsistentHash returns a consistent hashing strategy whose load is
// bounded at balanceFactor per cent of each backend's share, or unbounded
// when balanceFactor is 0.
func NewConsistentHash(balanceFactor int) *ConsistentHash {
	return &ConsistentHash{balanceFactor: balanceFactor, keyless: NewLeastConnections()}
}

// Select places a request that has no key, by least connections: with nothing
// to keep it anywhere in particular, the less busy of two backends serves it.
func (c *ConsistentHash) Select(backends []*Backend) (*Backend, error) {
	return c.keyless.Select(backends)
}

// SelectFor returns the backend with the highest score for key that still has
// room under the bound, or simply the highest when the load is unbounded.
func (c *ConsistentHash) SelectFor(key string, backends []*Backend) (*Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoBackends
	}
	keyHash := hashKey(key)

	home := c.Home(key, backends)
	if c.balanceFactor == 0 {
		return home, nil
	}

	room := c.room(backends)
	if room(home) {
		return home, nil
	}

	// The home backend is full, which is the uncommon case: rank the rest and
	// take the first with room. Some backend always has room, because the
	// bounds add up to more than the requests in flight; the home backend is
	// the answer only if that arithmetic is somehow defeated.
	type scored struct {
		backend *Backend
		score   float64
	}
	ranked := make([]scored, len(backends))
	for i, backend := range backends {
		ranked[i] = scored{backend, score(keyHash, backend)}
	}
	slices.SortFunc(ranked, func(a, b scored) int { return cmp.Compare(b.score, a.score) })
	for _, candidate := range ranked {
		if room(candidate.backend) {
			return candidate.backend, nil
		}
	}
	return home, nil
}

// Home returns the backend with the highest score for key, whatever its load.
func (c *ConsistentHash) Home(key string, backends []*Backend) *Backend {
	keyHash := hashKey(key)

	var best *Backend
	bestScore := math.Inf(-1)
	for _, backend := range backends {
		if s := score(keyHash, backend); s > bestScore {
			best, bestScore = backend, s
		}
	}
	return best
}

// room returns a test of whether a backend may take one more request under the
// bound. Each backend's bound is its weight's share of the requests in flight,
// this one included, times the balance factor, rounded up so that there is
// always room for at least one.
func (c *ConsistentHash) room(backends []*Backend) func(*Backend) bool {
	var inFlight, totalWeight int64
	for _, backend := range backends {
		inFlight += backend.ActiveConnections()
		totalWeight += int64(weightOf(backend))
	}
	factor := float64(c.balanceFactor) / 100
	return func(backend *Backend) bool {
		share := float64(inFlight+1) * float64(weightOf(backend)) / float64(totalWeight)
		return float64(backend.ActiveConnections()) < math.Ceil(factor*share)
	}
}

// Name identifies the strategy in configuration and logs.
func (c *ConsistentHash) Name() string {
	return string(config.ConsistentHashing)
}

// score is how strongly backend claims the key: a hash of the two turned into
// a uniform draw u in (0, 1), and then into w / -ln(u). For a backend of weight
// w that is an exponential variable of rate 1/w, and the largest of several
// such variables falls on each backend with probability proportional to its
// weight.
func score(keyHash uint64, backend *Backend) float64 {
	h := mix(keyHash ^ addrHash(backend))
	u := (float64(h>>11) + 0.5) / (1 << 53)
	return float64(weightOf(backend)) / -math.Log(u)
}

// addrHash is the hash of a backend's address. It is kept on the backend, since
// every keyed request scores every backend; a race between two first uses only
// computes the same value twice.
func addrHash(backend *Backend) uint64 {
	if h := backend.hash.Load(); h != 0 {
		return h
	}
	h := hashKey(backend.Addr)
	backend.hash.Store(h)
	return h
}

// weightOf is a backend's weight, never less than one, so that a backend added
// without the configuration's defaults still takes a share.
func weightOf(backend *Backend) int {
	return max(backend.Weight(), 1)
}

// hashKey is the 64-bit FNV-1a hash of s, passed through mix. It is written out
// rather than taken from hash/fnv, which would allocate on every request, and
// it is fixed rather than seeded, unlike hash/maphash: a key must reach the
// same backend after a restart and from every balancer in front of the pool.
func hashKey(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= prime
	}
	return mix(h)
}

// mix is the finalizer of SplitMix64. FNV-1a alone changes few bits between
// keys that differ in their last character, such as session-1 and session-2;
// after mix, every bit of the input affects every bit of the output.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
