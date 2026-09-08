# Day 4 — LB engine: round robin

**Planned deliverable:** a working LB engine with round robin selection.

## What was built

- `internal/balancer`: the `LBStrategy` interface from section 6.2, the `Backend` type, and
  `ErrNoBackends`.
- `RoundRobin`, selecting in O(1) by advancing an `atomic.Uint64`.
- `New(algorithm)`, turning the configured algorithm into a strategy.
- The proxy now asks the strategy for a backend on every request instead of holding one.
- 100% coverage in both packages.

## Decisions

**The chosen backend travels on the request context.** `httputil.ReverseProxy` calls `Rewrite`
per request, so one shared `ReverseProxy` instance can serve every backend: the handler selects,
puts the backend on the context, and `Rewrite` reads it back. The alternative, one
`ReverseProxy` per backend, would duplicate transports and connection pools.

**Algorithms that are not written yet fail at startup, loudly.** Configuration validation
accepts all three algorithm names from day 2, so selecting `least_connections` on day 4 had to
produce a clear error rather than a silent fall back to round robin. Starting with a different
algorithm than the operator asked for is worse than not starting.

**The counter is atomic, not mutex-guarded.** Section 5.4 asks for `sync/atomic` on simple
counters; round robin needs nothing more than an increment.

## What went wrong

Nothing broke. The interface from the design document was implementable as written, and the
concurrency test passed on the first run.

## Verification

A concurrency test runs 50 goroutines making 100 selections each under `-race`; the per-backend
totals must be exactly equal, so a lost increment fails the test even if the race detector stays
quiet.

Against ten live backends, 30 requests were distributed 3 to each.

## Note for later

Running the balancer on the host against the compose backends needs its own configuration file,
because `backend-1:5678` only resolves inside the compose network. `configs/lb.localhost.yaml`
was added for that, pointing at the published loopback ports.
