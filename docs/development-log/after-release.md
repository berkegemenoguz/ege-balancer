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

## Retry budget

**Why.** Under `retry_next_backend` every request may be retried `max_retries` times. When the
pool starts failing, that multiplies the traffic reaching it by up to `1 + max_retries` — at the
moment it can least absorb it. `max_retries` bounds one request; nothing bounded all of them.

### What was built

- `retry.budget_percent` and `retry.min_retry_concurrency`: retries in flight may be at most that
  share of the requests in flight, and never fewer than the minimum. The defaults are 20% and 3.
- A retry the budget refuses is not sent: the client gets 503 with `Retry-After`, and the refusal
  is counted as `lb_rejected_requests_total{reason="retry_budget_exhausted"}`.
- `lb_retries_total`, the retries actually sent, and a Retries panel on the dashboard showing both.

### Decisions

**Envoy's model rather than a token bucket.** Finagle's budget deposits tokens per request and
spends one per retry, over a time window. Envoy compares retries in flight with requests in
flight. The second has no window or refill rate to tune, follows the load at every moment, and
takes two counters and no lock.

**On by default.** A configuration that says nothing about the budget gets one. With the minimum
of three, light traffic retries exactly as before; the budget only acts when many requests fail
at once, which is the case it exists for.

**The minimum is at least one.** Zero in the file means "use the default", as it does for a
backend's weight. A minimum of zero would also mean that at low traffic — where 20% of the load
rounds down to nothing — no request could ever be retried.

**A refusal is a rejection, not a second metric.** The client receives a 503 from the balancer
itself, which is what `lb_rejected_requests_total` counts. `lb_retries_total` counts only the
retries sent, so the two together give the refusal rate without counting any event twice.

**The budget belongs to the configuration snapshot.** A reload builds a new, empty one. Requests
in flight keep the budget they started with, so no counter is ever decremented on the wrong one.

### What went wrong

**Hand-built configurations skip the defaults.** The proxy's unit tests build `config.Config`
directly rather than loading a file, so the budget came out as zero and would have refused every
retry. Their shared fixture now carries the defaults. The integration tests were unaffected: they
write the configuration to a file and load it the way the binary does.

**CI found a data race from day 11.** The push carrying the budget failed in
`TestReloadKeepsUnchangeableSettings` with a race the change had not caused. On a reload,
`MergeBackends` wrote a surviving backend's weight as a plain field while `/status` — and, under
real traffic, weighted round robin — read it. The test polls `/status` while a reload is applied,
and on this run the two met. The weight is now atomic and reached only through `Weight` and
`SetWeight`, so an unsynchronised write no longer compiles; a new test changes weights while a
strategy selects, and fails under `-race` with the plain field restored. It is the second
race from the plan's days that only CI found, after the health probe racing a day 9 test.

### Verification

Four backends that all fail slowly, fifty concurrent requests, up to three retries each:

| Budget | Attempts reaching the backends |
| --- | --- |
| The whole load | exactly 200 — every request tried four times; `lb_retries_total` 150 |
| The default 20% | 62 in each of 10 runs, and in each of 20 runs on two CPUs |

The proxy test for a spent budget fails when the budget check is removed. The budget costs
3.5 ns alone and 144 ns with ten goroutines contending for it — about 1% of forwarding a request
in parallel — and forwarding itself is unchanged at 104 allocations and about 13 KB.
