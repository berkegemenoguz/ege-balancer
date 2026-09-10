package proxy

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// idleBucketTTL is how long an untouched bucket is kept before the sweep drops
// it, so that a long run of one-off clients cannot grow the map without bound.
const idleBucketTTL = 10 * time.Minute

// sweepThreshold is the number of tracked clients that triggers a sweep.
const sweepThreshold = 10000

// rateLimiter caps how many requests a single client address may send per
// second, using one token bucket per address. A limit of zero disables it.
type rateLimiter struct {
	metrics *observability.Metrics

	mu sync.Mutex
	// perSecond is both the refill rate and the burst size, so a client may
	// spend a full second of requests at once and then refills steadily. It is
	// guarded because a reload can change it while requests are in flight.
	perSecond float64
	buckets   map[string]*bucket
}

// bucket is the token bucket of one client address.
type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// newRateLimiter returns a limiter for the configured per-IP request rate.
func newRateLimiter(perSecond int, metrics *observability.Metrics) *rateLimiter {
	return &rateLimiter{
		perSecond: float64(perSecond),
		metrics:   metrics,
		buckets:   make(map[string]*bucket),
	}
}

// setRate applies a new limit, keeping the buckets of the clients already being
// tracked. A limit of zero disables limiting.
func (l *rateLimiter) setRate(perSecond int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perSecond = float64(perSecond)
}

// wrap rejects requests from clients over their rate before they reach next.
func (l *rateLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if client := clientAddr(r); !l.allow(client, time.Now()) {
			slog.Info("client over the rate limit", "client", client, "path", r.URL.Path)
			l.metrics.ObserveRejection("rate_limited")
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow takes a token from the client's bucket, refilling it for the time that
// has passed since its last request.
func (l *rateLimiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.perSecond <= 0 {
		return true
	}

	tracked, known := l.buckets[addr]
	if !known {
		if len(l.buckets) >= sweepThreshold {
			l.sweep(now)
		}
		tracked = &bucket{tokens: l.perSecond}
		l.buckets[addr] = tracked
	} else {
		tracked.tokens += now.Sub(tracked.lastSeen).Seconds() * l.perSecond
		if tracked.tokens > l.perSecond {
			tracked.tokens = l.perSecond
		}
	}
	tracked.lastSeen = now

	if tracked.tokens < 1 {
		return false
	}
	tracked.tokens--
	return true
}

// sweep drops the buckets of clients that have been quiet long enough to have
// refilled anyway. The caller holds the lock.
func (l *rateLimiter) sweep(now time.Time) {
	for addr, tracked := range l.buckets {
		if now.Sub(tracked.lastSeen) > idleBucketTTL {
			delete(l.buckets, addr)
		}
	}
}

// clientAddr is the address the request came from. The client cannot influence
// it: forwarding headers are rewritten by the proxy, never trusted.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
