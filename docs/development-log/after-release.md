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

## Health and readiness endpoints

**Why.** The balancer checked the health of its backends but could not be asked about its own.
Its metrics port served `/metrics` and `/status`, neither of which answers the yes-or-no question a
container runtime or an orchestrator asks, and the Compose file said outright that no container
health check could be defined: the distroless image has no shell and no `curl` to run one with.

### What was built

- `/healthz` on the metrics port: liveness, 200 for as long as the process answers.
- `/readyz` on the metrics port: readiness, 200 while at least one backend is healthy, 503 when
  none is and from the moment a shutdown begins.
- A shutdown order: the balancer reports itself not ready, drains the traffic port, and only then
  stops the metrics port.
- `lb -probe <url>`, which exits 0 on a 200 and 1 otherwise, and a health check in the Compose file
  that runs it against `/healthz`.

### Decisions

**Two endpoints, not one.** A balancer whose backends are all down is still a working balancer.
If the question "should this be restarted" depended on the pool, a backend outage would restart
the balancer too, which brings no backend back and drops the connections it held. Only readiness
depends on the pool; the Compose health check uses liveness.

**On the metrics port.** Every path on the traffic port belongs to the backends; a `/healthz` there
would shadow theirs, and the mock backends have exactly that path.

**The same source as `/status`.** Readiness asks the health checker `/status` reads, so the two
cannot disagree about whether a backend is healthy.

**The binary probes itself.** Adding `curl` or a shell to the image would undo the reason for
distroless. A flag on the binary that is already there costs twenty lines and nothing in the image.

### What went wrong

**Readiness during shutdown needed the metrics port to outlive the traffic port.** Both servers
shut down on the same signal, so the moment `/readyz` first had to say "shutting down" was the
moment its port closed. Reporting not ready alone would never have been seen. The metrics server
now runs on its own context, cancelled only once the traffic server has drained.

### Verification

| Check | Result |
| --- | --- |
| `/readyz` with every backend healthy, one healthy, none, and shutting down (unit) | 200, 200, 503 `no healthy backend`, 503 `shutting down` |
| `/healthz` with no healthy backend while shutting down (unit) | 200 |
| Two backends killed, then one revived (integration) | `/readyz` 200 → 503 → 200; `/healthz` 200 throughout |
| Shutdown with a request in flight (integration) | `/readyz` 503 `shutting down` while the request drains; the request answers 200 |
| The readiness integration tests, 30 runs on 2 CPUs with the race detector | all passed |
| The shutdown test with the drain removed | fails: `/readyz` answers 200 until its port closes |
| `lb -probe` against a local balancer | `/healthz` exit 0; `/readyz` exit 1 once the unreachable backends left the pool; a closed port exit 1 |

## Requests that must not be retried

**Why.** `retry_next_backend` retried any failed attempt on another backend, whatever the method
was. A backend can take a request, carry it out, and then fail before its answer reaches the
balancer — the connection breaks, or with `retry_on_5xx` set it answers 5xx after doing the work.
The balancer cannot tell that apart from a request that never arrived, so a retried POST could
place a second order or take a second payment.

### What was built

- `internal/proxy/idempotency.go`: `idempotent` for the methods RFC 9110 defines as such, and
  `retryable`, which allows a retry when the method is idempotent or the dial failed.
- The retry loop stops before a further attempt when the request is not retryable, answers 503 and
  counts `lb_rejected_requests_total{reason="not_retryable"}`.

### Decisions

**Nothing in the configuration.** A switch for retrying POST would be a switch for the unsafe
behaviour. nginx has one (`non_idempotent`) and keeps it off; this project has consistently
declined to add such switches, so the rule lives in the code.

**A failed dial is the exception.** It is the one failure that proves no backend received the
request, so a POST is still retried past a backend that is not listening. This is also what keeps
the existing resilience behaviour: a backend that is down is skipped for every method.

**The method decides, not the body.** Bodies are still buffered for replay when retries are
possible, because an idempotent request with a body — a PUT — is retried like any other.

**5xx counts as received.** Under `retry_on_5xx` a 5xx answer is a failed attempt, but the backend
answered, so it had the request. A POST is not sent on.

### What went wrong

**The first version of the test raced.** The backend that takes a request and closes the
connection counted its requests in a plain int. With no answer to synchronise with, the handler's
write and the test's read are unordered, and the race detector said so. The counter is now atomic.

### Verification

| Check | Result |
| --- | --- |
| `retryable` over methods and failures (unit) | GET retried after any failure; POST only after a failed dial |
| POST against a backend that takes it and closes the connection (unit) | 503; the second backend is never asked |
| The same failure with GET (unit) | retried, answered 200 by the second backend |
| POST against a backend that is not listening (unit) | retried, answered 200 |
| POST with `retry_on_5xx` and a 5xx answer (unit) | 503; the second backend is never asked |
| Six POSTs over two backends, one taking requests and dropping them (integration) | the ones it took are refused, the rest answered; nothing retried onto the live backend |
| Six GETs in the same setup (integration) | all six answered 200 |

## Request identifiers

**Why.** A request that fails appears in several logs: the balancer's, and one for each backend it
was offered to. Nothing tied those lines together, and a client reporting a problem had nothing to
quote. With ten backends and retries, lining up the logs by timestamp is guesswork.

### What was built

- `internal/proxy/requestid.go`: every request is given an identifier, returned to the client and
  sent on to the backend as `X-Request-Id`.
- `request_id` on every log line about a request, in the proxy core, the rate limiter and the
  request validator.

### Decisions

**The client's own identifier is kept.** A trace that started in front of the balancer should
survive it. It is accepted only as printable ASCII of at most 64 characters, because the value
reaches the logs of the balancer and of every backend: an unbounded or control-character header
would be a way to make those logs unreadable or forged. Anything else is replaced.

**Generated with `crypto/rand.Text`.** 26 base32 characters from the standard library, with no
dependency and no error to handle. A counter would restart at zero on every restart and collide
across processes.

**Assigned outermost.** The identifier is set before rate limiting and validation, so a request
the balancer refuses itself carries one too — which is exactly the request someone asks about.

**Carried on the context.** The forwarder reads it there when it writes the outbound header, so
one `ReverseProxy` still serves every attempt and the value cannot be taken from a header the
client controls.

**No configuration.** The header name is the conventional one and nothing here needs tuning.

### Verification

| Check | Result |
| --- | --- |
| A request without the header (unit) | an identifier is returned to the client and the backend is sent the same one |
| A request with `X-Request-Id: trace-42` (unit) | kept, both to the backend and back to the client |
| An empty, over-long or control-character header (unit) | replaced by one of the balancer's own |
| A request refused by the rate limiter (unit) | the 429 still carries an identifier |
| Two requests through the assembled balancer (integration) | both answered with identifiers, and they differ |
| A client's identifier through the assembled balancer (integration) | comes back unchanged |
| The balancer and one mock backend, run for real | the answer carries the generated identifier; the debug line `request served` carries it as `request_id`; a client's `order-7` appears in both; the failure lines of an unreachable backend carry it too |

## A distribution test that depended on health checking

**Why.** CI failed in `TestRoundRobinSpreadsTrafficEvenly`: over 100 requests and ten backends, two
backends served 9 and two served 11. Every request was answered 200, so nothing had failed for the
client; round robin had simply not been given ten equal turns. The same assumption sat in seven
tests, which assert an exact or bounded share per backend.

### What the cause turned out to be

The tests' configuration probes every 10ms with a 5ms timeout and an unhealthy threshold of 2. On a
loaded runner two probes in a row time out, the backend leaves the pool for a few milliseconds, and
its turns go to whichever backend the strategy picks instead. Client traffic never notices: the
backend is answering, only slowly.

The mechanism was reproduced deliberately rather than guessed at. One backend was made to answer in
20ms — slow enough to time out every 5ms probe, fast enough to serve every request — while traffic
kept flowing. `/status` reported nine healthy backends in 37 of 40 samples, the distribution came
out 11, 11, 11 and 7, and all 100 requests were answered 200: the shape CI reported, exaggerated.

### The fix

`pinHealth` in the test harness puts the unhealthy threshold out of reach, so no backend can leave
the pool mid-run. It is applied to the tests that measure distribution — the five in
`balancing_test.go`, the algorithm switch in `reload_test.go` and the metrics test in
`proxying_test.go` — and nowhere else: the resilience tests are about health checking and must keep
their own thresholds.

An earlier attempt raised the probe timeout to two seconds instead. The configuration rejected it:
the timeout must be shorter than the interval. The threshold alone is enough, and
`proxying_test.go` already used it that way for a different reason.

This also makes two least connections tests honest. They slow a backend by 40ms on purpose and
assert it stays in rotation while taking less traffic; with the old timings that backend's probes
failed too, so health checking could have been what moved the load.

### Verification

| Check | Result |
| --- | --- |
| The experiment above, with the threshold pinned | ten healthy backends in 40 of 40 samples, exactly 10 requests each |
| `go test -race ./...` | green |
| The seven distribution tests, 30 runs on 2 CPUs with the race detector | green |
| `golangci-lint run ./...` | no issues |

## Measuring against the profiled backends

**Why.** The performance report measured the balancer against ten trivial backends that answered
ten bytes in microseconds. That found the balancer's own bottlenecks, which is what day 10 was for,
but it could say nothing about the algorithms: nothing was ever in flight, so least connections had
no signal, and every backend was identical, so weights and capacity differences were never
exercised. The mock backends added after v1.1 have profiles; the measurements had never been
repeated against them. The report also quoted a driver that had been thrown away, so nobody could
reproduce it.

### What was built

- `cmd/loadgen`: one goroutine per connection, keep-alive, closed loop. It reports percentiles over
  every recorded latency, the status codes, the share each backend served from its `X-Backend`
  header, the balancer's own retry and refusal counters read at both ends of the measured window,
  the latency distribution as a histogram, and each backend's own latencies.
- `configs/lb.measure.yaml` and `deploy/docker-compose.measure.yml`: the measurement configuration,
  mounted over the example one so a run cannot be spoiled by what the demo console last left there.
- `scripts/measure.sh`, which drives three algorithms across six load levels, `REPEATS` times each,
  interleaved and with `docker stats` sampled through every window; `scripts/measure-failure.sh`,
  which takes a backend away mid-run, by SIGTERM or SIGKILL; and `scripts/summarise.py`, which turns
  the results into the report's tables.
- The second campaign in [the performance report](../performance-report.md): 54 matrix runs, 18
  failure runs, and a dedicated run for the shape of the latency at 2,000 connections.

### Decisions

**The generator is committed this time.** The day 10 driver was a throwaway, and the report's
numbers became unverifiable the moment it was deleted. Twenty seconds of load is worth little if
nobody can run it again.

**Closed loop, not a fixed rate.** A generator that sends a chosen number of requests per second
measures the generator's choice. One that sends the next request when the last is answered measures
what the pool delivers.

**Three runs per cell, reported as a median with its range.** A single run cannot separate two
figures a few per cent apart, and the mocks draw their latencies at random. The rule the report
follows is that a gap narrower than the range is not a result — which is what keeps it from claiming
that least connections beats weighted round robin at the knee, where they are 5% apart.

**The algorithms are interleaved and their order rotated.** See below: measured in blocks, the
campaign drifted.

**Workers stop rather than being cut off.** The first version ended the run by cancelling the
context, which cancelled requests in flight. Those failed every attempt inside the balancer and
were counted there as refusals — 12 of them in an early run where the client saw none. Now each
worker finishes its current request.

**The warmup is discarded, and the counters are read inside the window.** Counters read before the
warmup include what happened while connections were still being opened; an early run credited the
measured window with 503s from its own warmup.

**Weights proportional to capacity.** A weighted round robin run with weights of 1 would have
measured round robin twice. The weights are `capacity / 16`, so 4, 2 and 1.

### What went wrong

**The first run measured the rate limiter.** The example configuration limits a client to 100
requests per second, and the load comes from one address: 86,969 of 87,466 requests were answered
429. The measurement configuration turns the limiter off, as day 10 did.

**The failure experiment first measured a graceful drain.** `docker compose stop` sends SIGTERM,
which the mock handles by finishing the requests it holds, so the balancer barely noticed. The
script now takes `MODE=kill` as well, and both are reported. With three runs each, the two turn out
to cost the same in retries, which the single run could not have shown.

**The campaign drifted, and the first explanation was wrong.** Repeating the cells exposed it: a
block-ordered campaign, four seconds between runs, lost throughput steadily through the session —
six runs at 300 connections went from 5,405 to 3,395 answered per second. The first suspect was the
new `docker stats` sampler perturbing the measurement, so it was tested: with the sampler the runs
were *faster* (5,405, 5,221, 5,025) than without it (4,705, 3,713, 3,395), because the sampled group
ran first. The decline was the session, not the sampler. Restarting every container did not recover
it, the host had no sockets in `TIME_WAIT` and 56% of its memory free, and `pmset` had recorded no
thermal event; host CPU frequency under sustained load is the remaining suspect, and confirming it
needs root. The campaign answers it by design instead: the algorithms are interleaved at each level,
their order rotates each repeat, and the gap between runs is eight seconds. Under those conditions
the median cell held at 99.8% and 99.2% of its first run — no measurable drift. The block-ordered
runs were kept under `measurements/block-order/` and are not what the report quotes.

**The second wrong hypothesis was about the slow hump.** The client's p95 at 2,000 connections is
2.9 s against a p50 of 54 ms, and the guess was that the backend answering 256 KiB was dragging the
tail. Per-backend latencies said the opposite: that backend is the *only* one without a tail, at
405 ms, while all nine answering 4 KiB sit between 2.91 and 3.19 s. The balancer's own histogram
then located the wait — p95 of 241 ms inside the handler — so the seconds are spent in the
connection backlog before the request is picked up, which is also why every 4 KiB backend shows the
same figure.

### Verification

The results themselves are in [the performance report](../performance-report.md#measurements-against-the-profiled-backends);
what is checked here is the method that produced them.

| Check | Result |
| --- | --- |
| Every cell run three times, the algorithms interleaved and their order rotated | 54 matrix runs and 18 failure runs |
| Drift across the campaign | the median cell at 99.8% and 99.2% of its first run |
| The balancer's own counters across the matrix | unmoved in all 54 runs: every 503 came from a backend |
| Counters and CPU samples taken inside the measured window | the generator scrapes `/metrics` at both ends; `docker stats` starts after the warmup |
| `cmd/loadgen` unit tests | percentiles, aggregation, failure classification, metric parsing, the histogram, per-backend timings, the warmup boundary and an unreachable target |

## Realistic mock backends

**Why.** Consistent hashing and sticky sessions come next, and both are about sending a client back
to the backend that knows it. Against backends that remember nothing, that can show only its cost.
The backends also failed only at the edges — a refused connection, an immediate 500 — never in the
middle of an exchange, where most of what a balancer has to handle happens.

### What was built

- In `cmd/mockbackend`: a bounded cache of client sessions named in `X-Session`, with a penalty on
  a miss and `X-Cache: hit` or `miss` on the answer; faults — hang, drop the connection half way
  through the answer, drip the answer out, 500, run slower, freeze; a cold start and pauses; and an
  admin port through which a fault is started on one backend for a limited time. All of it is off
  by default.
- `deploy/docker-compose.realistic.yml`, which turns the steady versions on for every backend, and
  admin ports published on the loopback interface only, at 5781 to 5790.
- In the demo console, action `8` to make a backend misbehave, the faults in force on the prompt,
  and `r` and `q` clearing them.
- In `cmd/loadgen`, sessions to spread requests over, the hit rate, a method and a body.

### Decisions

**A separate admin port.** The balancer forwards every path on the traffic port, so an endpoint
there could be reached by any of its clients. And nothing on the admin port waits on the fault
layer, so it answers while the backend is hanging or frozen, which is exactly when someone wants to
stop it.

**Every injected fault ends on its own**, after at most ten minutes, so one left behind by a
demonstration cannot spoil the next measurement for long. An injected fault replaces the profile's
rate for its mode rather than adding to it.

**Hang holds its worker, freeze stops the health check.** A hung request is a thread stuck on a
lock: it occupies capacity, but the process still answers its probe. A freeze is the whole process
stopped, probe included. Balancers treat the two very differently, which is the point of having
both.

**Pauses at a phase of their own.** Ten backends started together with "a pause every fifteen
seconds" would all pause at the same moment, and the whole pool would stall at once. Each backend
draws its phase from its seed; the collections of separate processes have nothing to do with each
other.

**A cache sized against the key pool.** Affinity only pays when the sessions outnumber what one
backend can remember but fit in the pool as a whole: 5,000 per backend against a pool of 30,000.

### What went wrong

**A hanging POST never let go of its worker.** Go's server notices a client leaving only once the
request body has been read, and the mock never read it. Every POST that hung held its worker for
good, and after one run backend-3 answered nothing at all, the fault long cleared. The mock now
reads the request before serving it, as a server that parses it would.

**The first explanation of a live result was wrong.** A run with backend-3 cutting its answers off
showed the balancer retrying, which an answer already on its way cannot be. The balancer's log
settled it: every failure on backend-3 was a header timeout — it was still stuck from the POST
problem above, not cutting anything off.

**The generator could not see two things.** It counted an answer cut off half way as a success,
because it ignored the error from reading the body; and Go's client sends a GET again, on a new
connection, when the one it reused dies before any answer, so a cut-off GET came back as a 200. Both
are now counted.

**The generator's window stretched.** Workers finish the request they are on when the time is up,
and a request hanging for seconds kept the run going long after its window; throughput was divided
by the longer time. It is now divided by the window. The same stretch, 2 to 5 per cent at the
median, understated the throughput of the second measurement campaign, which the performance report
now says.

**Measuring these faults exposed three defects in the balancer itself**, recorded in the next
section.

### Verification

| Check | Result |
| --- | --- |
| `/faults` through the balancer | an ordinary answer from a backend; the admin endpoints are unreachable there |
| Where the admin ports are bound | `127.0.0.1` only |
| 30,000 sessions over the pool for 15 s | 3.1% hits: each backend had seen about a twentieth of the keys, as a short run should |
| A backend restarted with a cold start | factor 3.9 just after, 2.45 at 15 s, 1 at 30 s |
| A backend hanging every POST, then cleared | answers again at once, with no worker held |
| The console's fault action, driven through its prompts | the fault injected, named on the prompt, listed by `7`, cleared on quitting |
| `cmd/mockbackend` unit tests | 85.5% of statements; each timing test stable over 15 runs on two CPUs |

## Three defects the realistic backends exposed

**Why.** Faulting the new mock backends under load gave numbers that did not add up: 33 failed
attempts on one backend but one retry counted; 89 refusals for lack of retry budget with a single 503
at the client; POSTs counted as `not_retryable` while every client saw a 200. Two scratch tests and a
look at the balancer's own log traced it to three defects, all of them only visible when something
fails in the middle of an exchange.

### What was wrong

- **The retry budget leaked.** Go's reverse proxy aborts an answer that fails part way through by
  panicking, and the retry loop gave a retry's share back only after the call returned. A scratch
  test left one retry in flight with no request in flight. With a hundred requests in flight the
  budget allows twenty retries, and after twenty such leaks `lb_retries_total` stopped at 20 while
  every later retry was refused — until the next reload. In v1.1.0 since the budget was added.
- **Answers broken off part way were invisible.** The same abort skipped the bookkeeping: the
  scratch test's health checker heard neither a success nor a failure. A backend that kept cutting
  its answers short was never taken out of the pool. In the code since v1.0.0.
- **The shipped configurations defeated the response timeout.** Both timeouts were ten seconds; a
  backend abandoned after ten seconds left none to answer the client from another, and the client
  got an empty reply. The deployment checklist already said the response timeout must be the shorter.

### What was done

- The retry's share of the budget is given back in a deferred call, which runs through a panic.
- The backend's answer is read through a wrapper that marks the attempt when a read fails. As the
  abort passes through, the failure is recorded against the backend — health checker, circuit
  breaker, `lb_backend_failures_total` — and the panic carries on, so the client's connection is
  still closed rather than left with half an answer. A client that hangs up is not the backend's
  failure and is not counted.
- `response_timeout` is 3 s in the three configurations, against a write timeout of 10 s, and a
  configuration whose response timeout is not the shorter is logged as a warning at start and on
  every reload. A warning rather than an error, because a patch release should not refuse a
  configuration that loaded before.

### Verification

| Check | Result |
| --- | --- |
| A retry whose answer is broken off | the budget is back to zero afterwards; with the fix reverted, one retry stays held |
| A backend breaking off its answer | one failure reported to the health checker and in `lb_backend_failures_total`; with the fix reverted, none |
| A client hanging up part way through an answer | no failure counted; with the client check removed, the backend is blamed |
| Live, every run after the fix | retries allowed in each — from 20 to 235 per run — and not one refused for lack of budget |
| Live, a backend hanging every POST | 503 as `not_retryable` after 3 s, where before the client got an empty reply after 10 s |
| Live, a backend cutting off every answer | 33 failures recorded against it as `unexpected EOF`; the cut-off GETs sent again by the client, 27 of them, the POSTs failing at the client, 24 |
| The whole suite with the race detector | green; `internal/proxy` at 95.2% of statements |

## Clients that give up

**Why.** Breaking the balancer's failed attempts down by reason, for a dashboard of faults, meant
listing every error that reaches a backend's record. One of them was `context canceled`: the
client's own request, cancelled when the client gave up. A scratch test with a client that waited
200 ms for three slow backends found all three blamed, two retries sent and one request counted as
refused, for a client that had simply left.

### What was wrong

- **The backend was blamed.** When the client went away before the answer began, the forwarder
  ended the attempt with the cancelled request's error, and the attempt was recorded as the
  backend's failure — health checker, circuit breaker, `lb_backend_failures_total`. v1.3.1 had
  stopped this for a client leaving part way through an answer, but not for one leaving before it.
- **The request was retried.** The retry loop went on to the next backend with the same cancelled
  request, which failed at once and was counted against that backend too, and so on until the
  attempts ran out, ending with a refusal counted as `no_backend_available`.
- **Together with round robin it made an outage.** Each attempt moved round robin on by one, so
  with three backends and three attempts every request began where the last had: at the backend
  that made clients give up. In the code since v1.0.0.

### What was done

- An attempt that ends because the client has gone is not recorded against the backend.
- The retry loop stops as soon as the client has gone, with nothing written and nothing counted.
- The load generator's clean stop, added during the campaign against the profiled backends for this
  very behaviour, stays: it keeps every request it sent measured. Its comment and README no longer
  present the balancer's miscounting as the reason.

### Verification

| Check | Result |
| --- | --- |
| A client giving up on three slow backends, `retry_next_backend` | one backend offered the request, no failure reported, nothing refused |
| The same with the retry loop's check removed | a refusal counted as `no_backend_available` |
| The same with the attempt's check removed | the first backend blamed, in the health checker and the metrics |
| Live, three mocks, one hanging every request, 15 clients each giving up after 1 s — before | 14 of 15 unanswered; 14 failures counted against each backend, 28 retries, all three marked unhealthy |
| The same, after | 5 of 15 unanswered, the ones sent to the hanging backend; no failure counted, no retry, all three healthy |
| After, 6 clients waiting up to 10 s | all answered 200; the three sent to the hanging backend after the 3 s response timeout, counted against it as timeouts and retried elsewhere |
| The whole suite with the race detector | green; `internal/proxy` at 95.3% of statements |

## The Faults dashboard

**Why.** The mock backends can fail in six ways, but the balancer counted every failure the same
way, and the dashboards showed only the balancer's side. A hang, a dropped connection and a stopped
container all raised one counter, and whether the backend had done what it seemed to have done
could only be guessed.

### What was built

- **The mocks' own metrics.** `GET /metrics` on the admin port: what the backend did with each
  request — answered, hang, reset, drip, error, overloaded, or abandoned when the client left first —
  cache hits and misses, faults injected and in force, busy workers, capacity, queue and cold-start
  factor. 26 series per backend.
- **A reason on every failed attempt.** `lb_backend_failures_total` gains `reason`: `connect`,
  `timeout`, `reset`, `cut_off`, `5xx` or `other`.
- **Prometheus scrapes the mocks.** A `mocks` job reads the ten admin ports and labels each with
  the address the balancer forwards to, `backend="backend-3:5678"`, so the dashboards' backend
  selector and colours work on both views unchanged.
- **The Faults dashboard.** Its rows follow a fault from the backend to the client: right now,
  faults in force, what the backends did, what the balancer saw, what the clients got, and what the
  backends remember. Every dashboard shades the time a fault was in force.
- **The console** points at the new dashboard after injecting a fault, and its descriptions name
  the reason each fault is counted under.

### Decisions

- **The mocks' metrics are written by hand.** A few dozen counters and gauges and no histograms do
  not need the client library, and the mock stays a program of the standard library alone: its
  image is still built from `go.mod` and its own package. A test reads the output back with
  Prometheus's own parser, which is the check that matters for hand-written output.
- **Every series exists from the start, at zero.** A counter that first appears at 1 loses that
  first event to `rate()`.
- **The outcome is counted on the way out.** A dropped answer leaves the handler by panicking, and
  only a deferred count sees it. A request that ends any other way before it is answered is counted
  as abandoned.
- **`cut_off` is decided where the abort is seen, not from the error.** The error of a broken-off
  answer is `unexpected EOF`, which read on its own would say `reset`.
- **A failed connection is checked before a timeout**, since a connect timeout is both.
- **The balancer's failure counters are created at zero too**, six per backend, at start and on
  every reload. See below for why.
- **The injected-faults timeline has one row per backend, always present.** A query returning only
  the faults in force gives the timeline no data at all when there are none, which it reports as an
  error. The mode in force is encoded as a number and named by value mappings.

### What went wrong

- **The first stop of a backend showed no failures.** Stopping backend-8 cost it three `connect`
  failures and its place in the pool within half a second, and the dashboard showed none: the
  series for backend-8 and `connect` came into being with its first three failures, and `rate()`
  cannot see a jump from nothing. The reason label made this likely — a backend and a reason seen
  together for the first time — where before only a backend's very first failure was lost. Every
  backend's six counters are now created at zero; stopping backend-6 afterwards showed its three.
- **The injected-faults timeline showed nothing, twice.** Built from only the faults in force, it
  had no data when there were none and reported that as an error. Rebuilt with a row per backend,
  it merged every row into one state: under threshold colouring, Grafana's state timeline groups
  values by threshold range and ignores value mappings. It now uses a fixed colour. The health
  timeline had the same setting all along and looked right only because its thresholds coincide with
  its two values.
- **Stacked graphs showed failures that did not happen.** A series at zero on top of a stack draws
  its line at the stack's height, and the outcome graph seemed to show overloaded and failing
  requests during a hang. The fault graphs are no longer stacked, and the graph of what clients
  received uses a logarithmic axis, so that a few failures a second show next to forty answers.
- **Two descriptions promised more than was measured.** The latency panel is the time of the
  attempt that answered, so the three seconds lost to a hang before a retry do not appear in it; the
  panel now says so, and the metric is unchanged. And an answer cut off part way usually reaches the
  client as nothing at all: the part sent fits in the balancer's buffer, and the connection closes
  before it is flushed. At the client, 98 of the 101 cut-off answers arrived as a connection closed
  with no answer.
- **One live comparison was spoiled by health checking.** Faulting one backend with each mode in
  turn, the cut-off answer after three timeouts was the third failure in a row and took the backend
  out of the pool, so the next fault reached it less. Each fault now starts with the whole pool
  healthy.

### Verification

| Check | Result |
| --- | --- |
| Prometheus configuration | valid by `promtool`; the ten mock targets up, each labelled `backend="backend-N:5678"` |
| Every query of the Faults dashboard | runs against Prometheus without an error |
| backend-3 hanging half its requests for 60 s, at 40 req/s through the stack | 72 hangs counted by the mock, 72 `timeout` by the balancer, 71 retries, 7 POSTs refused as `not_retryable`; backend-3 out of the pool while it lasted |
| backend-5 dropping 30% of its answers half way for 60 s | 54 resets counted by the mock, 54 `cut_off` by the balancer; backend-5 out of the pool |
| backend-7 frozen for 45 s | 14 `timeout` by the balancer, about as many requests abandoned by the mock's count; its metrics scraped throughout; out of the pool |
| backend-9 answering 500 to everything for 45 s | 186 errors by the mock, 183 passed on to clients, none counted as failures with `retry_on_5xx` off |
| backend-4 four times slower for 45 s | its median from 19 to 71 ms; backend-1's from 21 to 23 ms |
| backend-2's cache cleared | sessions remembered from 435 to 4, then refilling |
| backend-8 stopped for 40 s | 3 `connect`, out of the pool within half a second, 9 of 10 mocks reachable; the failures invisible to `rate()` until the counters were created at zero; backend-6 stopped afterwards showed its 3 |
| All four dashboards | the time each fault was in force shaded; the timeline names each fault on its backend |
| Three mocks on the host through the balancer | hang counted as `timeout`, a dropped answer as `cut_off`, a stopped backend as `connect`, a 500 passed on and not counted; each count matching the mock's own |
| The clients over nine minutes | 21,291 answered 200, 180 answered 500, 16 POSTs refused with 503, 101 answers cut off against 100 `cut_off` counted by the balancer; 12 connection errors while the balancer's container was rebuilt part way |
| Tests | the mock's metrics read back by Prometheus's parser; each reason produced by a real backend; reverting the deferred count, the `cut_off` path, the order of the checks or the counters prepared on reload each fails a test |
| The whole suite with the race detector | green; `cmd/mockbackend` at 88.7%, `internal/proxy` at 96.9%, `internal/observability` at 99.0% |
