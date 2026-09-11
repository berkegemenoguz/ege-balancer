# After the release

Work done once the twelve day plan was finished and v1.0.0 was tagged. The plan's days are over,
so this page is organised by addition rather than by day.

## Benchmarks and regression guards

**Why.** The load test on day 10 found three bottlenecks, and two of them came from a single line
each: the transport's default of two idle connections per host, and the missing buffer pool.
Nothing in the test suite would have noticed either being undone, and the load test that found
them needs the whole environment and a quiet machine.

### What was built

- `TestUpstreamConnectionsAreReused`: sends 5 rounds of 24 concurrent requests through the proxy
  and counts the connections the backend accepts.
- `TestForwardingAllocatesLessThanACopyBuffer`: forwards 500 requests and fails if each one
  allocates as much as the 32 KB copy buffer.
- Benchmarks for every strategy at 10 and 100 backends, sequential and parallel; for health
  lookups, alone and alongside reports; for the rate limiter with one client and many; and for
  forwarding a request.
- A benchmarks workflow that runs them on every push, runs them on the code before the push, and
  puts the benchstat comparison in the run's summary.

The numbers and what they show are in the
[performance report](../performance-report.md#keeping-the-fixes).

### Decisions

**Tests gate, benchmarks report.** A benchmark's timing on a shared runner varies by several per
cent between identical runs, so failing a build on it would fail builds at random. The two fixes
are protected by tests whose answer does not depend on the machine: a count of connections, and
a count of bytes. The benchmarks only inform.

**Bytes, not allocations.** A request makes 104 allocations with the buffer pool and 104 without
it: the pool changes the size of one allocation, not how many there are. A test counting
allocations would pass with the fix removed, so the test gates on bytes per request.

**Compare with the state before the push, not with the previous commit.** A push can carry
several commits, and comparing only the last one against its parent would miss everything before
it. The workflow checks out `github.event.before` — the tip of `main` before the push — and
measures it on the same runner in the same job, so both sides share the hardware.

**A missing baseline is not a failure.** The first push has none, a force push can leave one that
no longer exists, and old code may not build with the benchmarks of the new. In each case the
summary shows the new numbers alone.

### What went wrong

**The rate limiter benchmark measured string building.** The first version built each client's
address inside the timed loop, and reported 16 bytes allocated per call that the limiter did not
make. The addresses are now built before the timer starts, and the limiter allocates nothing.

**A comment had drifted.** In the transport setup, the comment explaining the pool sizing sat
above the response header timeout rather than the idle connection fields it describes. It was
moved back.

### Verification

Each guard was checked by reintroducing the bug it guards against:

| Guard | Healthy | Bug reintroduced |
| --- | --- | --- |
| Connection reuse | 24 connections | 112 connections, fails |
| Bytes per request | about 13 KB | about 46 KB, fails |

The workflow passed actionlint, and both of its summaries — with a baseline and without one —
were produced locally from real benchmark output before it was pushed.
