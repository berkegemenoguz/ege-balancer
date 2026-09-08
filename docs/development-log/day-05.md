# Day 5 — LB engine: least connections and weighted round robin

**Planned deliverable:** all three algorithms selectable from configuration.

## What was built

- An active connection counter on `Backend` (`Acquire`, `Release`, `ActiveConnections`) over an
  `atomic.Int64`, maintained by the proxy for the whole lifetime of a request.
- `LeastConnections`, holding no state of its own.
- `WeightedRoundRobin`, using the smooth weighted round robin algorithm.
- Coverage stayed at 100% for the package.

## Decisions

**Deterministic smooth weighted round robin instead of probabilistic selection.** Section 5.3
describes weighted selection as probabilistic. Smooth WRR was implemented instead: it hits the
configured ratio exactly rather than approximately, needs no random source, and spreads the
heavy backend's turns across the cycle instead of bunching them. It also makes the tests exact
rather than statistical. With weights 5, 1, 1 the sequence is `a a b a c a a`, which is the
documented nginx behaviour.

*This is a deviation from the design document and the document should be updated to match.*

**Weighted credits are keyed by backend address, not by slice index.** Day 6 was going to pass
the strategy a filtered pool with unhealthy backends removed, at which point indices shift and
index-keyed state would silently attach to the wrong backend. A test written on day 5 already
covers selection over a shrinking slice.

**Ties in least connections go to the earliest backend in the pool,** which keeps selection
deterministic and therefore testable.

**The connection counter lives on `Backend`, not inside the strategy.** The proxy has to
decrement it when the request finishes, and the strategy is not involved at that point. It is
also the natural place for the metrics work on day 8 to read from.

## What went wrong

**`go vet` caught the tests copying a lock.** Adding `atomic.Int64` to `Backend` made
`t.Errorf("%+v", *backend)` illegal, since dereferencing copies the atomic. The fix was to print
the fields that matter instead of the whole struct. A useful reminder that vet catches this
class of bug in test code too.

## Verification

Against live backends: weights 1, 2 and 3 produced exactly 2, 4 and 6 out of 12 requests; least
connections under 30 concurrent requests spread them 15, 10 and 5.

The uneven split for least connections is expected: `http-echo` answers in microseconds, so
counters are usually back at zero when the next selection happens and the tie goes to the first
backend. The algorithm only earns its keep when backends differ in how long they take.
