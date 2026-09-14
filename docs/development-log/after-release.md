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

## Power of two choices

**Why.** Measuring for the design paper showed least connections sending every one of 100
sequential requests to `backend-1`. The scan broke ties by pool order, and with backends answering
in about a millisecond almost every selection is a tie: nothing is in flight. It was also O(n), and
requests arriving together all chose the same backend.

### What was built

- Least connections now draws two different backends at random and takes the less busy. With two
  backends that is the whole pool, so the choice is exact; with one it is that backend.
- The random source is a field of the strategy: `math/rand/v2` in the proxy, a seeded generator in
  the unit tests.
- Tests for each acceptance criterion the design paper set before the change: idle backends shared
  evenly, the busiest of a pair never chosen, a burst spread, seeded runs reproducible, sequential
  traffic spread over real sockets, and a slow backend avoided under sustained load.
- The selection benchmarks now include a pool of a thousand backends.

### Decisions

**Criteria first.** The paper stated five acceptance criteria before any code was written, so the
change was judged against a bar it could not move.

**Keep the name.** The strategy still answers to `least_connections`. It still sends requests to the
less busy backends, configurations need no change, and Envoy made the same choice.

**Distinct draws without retrying.** The second index is drawn from one fewer and steps over the
first, so the two always differ and a two-backend pool is always compared in full.

**Seeded tests rather than statistical ones.** Deterministic tests were a principle since
weighted round robin. A seeded source keeps the unit tests exact; only the tests over real sockets,
where timing already varies, assert bounds.

### What went wrong

**One criterion did not hold as first measured.** The existing test sends 60 requests at once to one
slow and two fast backends, and the slow backend served 16 to 20 of them — up to a third, not
"clearly fewer". Measuring the old scan beside it explained why: in a single burst almost every
selection sees all counts at zero, so neither strategy can do better than an even spread, and the
scan's 16 came from its bias toward the first backend, which in that test was the slow one. Under
sustained traffic both kept the slow backend to 6–11 of 200 requests. The criterion was restated
for sustained load, a test for that case was added, and the burst case is recorded as a limit rather
than hidden.

**CI failed a test this change did not touch.** The push failed in `TestFailFastSurfacesTheFailure`,
which uses round robin: of 30 requests, the ten meant for a dead backend should have been 503, and
none were. The test killed its backend by closing the server, which frees the port — and a freed port
can be handed to the next listener that asks, the balancer's own sockets bound a moment later
included. Whatever took it answered for the dead backend. The failure did not reproduce in 200 runs
on macOS, so the cause is the likely one rather than a proven one; but the fault in the test is
certain either way. A dead backend now keeps its port and drops every connection without answering,
as a crashed process would, and the four tests that closed a server use it.

**Small pools got slower.** Two random draws cost more than ten atomic loads: 14 ns against 3.9 ns
at ten backends. It is about 0.03% of forwarding a request, and the break-even is around thirty
backends; the paper states it.

### Verification

| Criterion | Scan | Power of two choices |
| --- | --- | --- |
| Cost at 10, 100, 1,000 backends | 3.9, 47, 484 ns | 14, 14, 14 ns |
| 100 sequential requests, most on one backend | 100 | at most 18 in 20 runs |
| Burst of 50, most on one backend | 50 | 8 |
| Slow backend, sustained load | 7 of 200 | 6–11 of 200 |
| Slow backend, one burst of 60 | 16 of 60 | 16–20 of 60 |

The integration tests ran 20 times on two CPUs under `-race`. Coverage of `balancer` stayed at 100%.

## Mock backends

**Why.** The ten backends of the demo environment were `hashicorp/http-echo`, which answers every
request in microseconds with a few bytes. That had cost three things already: least connections
could not be shown in the demo, because nothing was ever in flight; the load test forwarded ten-byte
bodies; and every backend was equal, so differences in capacity were never exercised.

### What was built

- `cmd/mockbackend`, one program run as all ten backends, each with a profile: a log-normal latency
  given by its median and 99th percentile, a capacity with a bounded queue beyond which it answers
  503, an answer size, an error rate and a seed.
- `/healthz` answering 503 while more than half of the queue is waiting, and an `X-Backend` header
  naming the backend on every answer.
- An image of its own, `deploy/mockbackend.Dockerfile`, with an ignore file that sends only
  `go.mod` and the mock's package as build context; CI builds it beside the balancer's image.
- A compose file giving six backends a fast profile, two a slower one with less capacity, one a
  struggling one and one large answers.
- The demo console counting the distribution from the `X-Backend` header.

### Decisions

**One program, many profiles.** Every backend runs the same image; they differ only in settings,
which the compose file shares through YAML anchors and environment variables. Changing a scenario is
a line in the compose file, not code.

**Latency is spent holding a worker.** A request waits for a worker first and is then "served".
That makes a backend with little capacity slow down as its queue grows, which is how a real server
behaves and what gives least connections something to balance.

**Log-normal latency.** Real service times have a long tail, and the median and the 99th percentile
are the two numbers people actually know about a service; together they fix a log-normal
distribution.

**The integration tests keep their own backends.** They need a backend they can slow down, fail or
kill in the middle of a test, and they run without Docker; the mock is for the environment.

**Ten backends, for now.** A pool of thirty was considered. It would help measure where the power of
two choices overtakes the scan, but would make the dashboard and the demo unreadable and break the
comparison with every earlier measurement. It is recorded as future work, with a generator for the
compose file and configurations rather than thirty hand-written services.

### What went wrong

**The distribution was counted from the body.** The demo console took the whole answer as the
backend's name, which a 4 KiB body breaks. It now reads the `X-Backend` header, and the body still
begins with the name so that a person using `curl` sees who answered.

### Verification

Unit tests cover the latency distribution — over 100,000 draws the median within 5% and the 99th
percentile within 10% of the profile — the queue and capacity, a client leaving the queue, health
under overload, the error rate and flag parsing; coverage of the package is 80%.

The environment was brought up with `docker compose up --build`:

| Check | Result |
| --- | --- |
| Build context of the mock image | 23.6 kB; the image is 15.1 MB |
| Pool as the balancer sees it | 10 of 10 healthy |
| Capacity 2 and queue 2, 20 requests at once, run outside Docker | 4 answered, 16 refused with 503 |
| Median time through the balancer, round robin | fast 14–21 ms, medium 34–41 ms, slow 100 ms (max 753 ms), large answers 15 ms |
| Answer size of the large profile | 262,144 bytes |
| Memory under load | 7–10 MiB per backend against a 32 MiB limit |
| Stopping a backend | exits 0.07 s after SIGTERM |

## Grafana dashboards

**Why.** Watching the demo with the new mock backends showed how much the single dashboard hid or
misled. The error panel said "No data" when there were simply no errors. The request rate axis
started at 1.98, turning a one-request wobble into a spike. The in-flight gauge, sampled every five
seconds, jumped between 0 and 1 and said nothing about which backend was busy. Backends shared
colours, so the slow one could not be picked out. And the latency histogram, which is recorded per
backend, was never shown per backend.

### What was built

- Three linked dashboards in an *Ege-Balancer* folder: *Overview* for the screen during a demo,
  *Backends* for comparing them, *Resilience* for what failures do.
- On every graph, markers for applied and rejected configuration reloads.
- A colour per backend following its profile, a backend selector and a window selector.
- `deploy/grafana/generate_dashboards.py`, which writes all three.

### Decisions

**Three dashboards, not one long page.** The overview has to fit a screen while someone talks over
it; the detail belongs a click away.

**Generated, not hand-written.** Three dashboards share colours, variables, annotations and panel
styling. Kept in step by hand, they would drift within a few edits. The script uses only the
Python standard library, and the JSON it writes is what Grafana provisions.

**Averages over raw gauges.** Requests in flight are shown as `avg_over_time` over the selected
window. The raw gauge is correct but, sampled every five seconds at light load, it is noise.

**Zero where there is nothing.** Totals that can be absent — refusals, 5xx, retries — fall back to
`vector(0)`, so a quiet system draws a line at zero instead of an empty panel.

### What went wrong

**The first measurement of least connections was read from a noisy graph.** On the old dashboard
the per-backend lines after switching algorithm looked like random divergence. Reading the same
data from Prometheus over the whole window showed the slow backend taking the smallest share and
the in-flight averages ranked by profile. Both views are now panels.

**The folder did not apply to a running Grafana.** Grafana created the *Ege-Balancer* folder
but left the dashboards it had already provisioned in *General*, and restarting it did not move
them: its database lives in the container's anonymous volume and survives restarts. A throwaway
Grafana started with the same provisioning and an empty database put all three in the folder, so
a new environment is right; an existing one needs its Grafana volume renewed once.

### Verification

| Check | Result |
| --- | --- |
| Every query of the three dashboards, run against Prometheus | 43 queries, no errors |
| Provisioning | all three loaded; no provisioning errors in Grafana's log |
| Requests stat against the demo's sustained traffic | 20.0 req/s |
| Backends table, sorted by p95 | the slow backend first |
| The generator, run again | identical JSON |
| A fresh Grafana with the same provisioning | all three dashboards in the *Ege-Balancer* folder |
