# Performance report

Two measurement campaigns, which answer different questions.

The first, on day 10 of the plan, drove load through the balancer against ten trivial backends to
find out where the balancer itself was slow. It found three bottlenecks and is recorded below as it
was carried out.

The second, after v1.3.0, repeats the exercise against the ten profiled mock backends — different
latencies, capacities and answer sizes — to compare the three balancing algorithms under a pool
that is not uniform. It is in [Measurements against the profiled
backends](#measurements-against-the-profiled-backends), and it is the one to read for a question
about choosing an algorithm. Its load generator, `cmd/loadgen`, is part of the repository, so the
numbers can be produced again rather than taken on trust.

## Method

| | |
| --- | --- |
| Machine | Apple silicon, 10 cores, macOS |
| Balancer | one process, `GOMAXPROCS` at the default |
| Backends | ten trivial HTTP servers in one process, answering a ten byte body |
| Configuration | round robin, `retry_next_backend` with two retries, rate limiting off, `max_connections: 10000` |
| Duration | ten seconds per level, connections held open for the whole run |

Everything runs on the same machine, so the load generator and the backends compete with the
balancer for the same ten cores. The absolute numbers are therefore a floor rather than a
ceiling; the comparisons between runs are what carry meaning, since every run shares the
handicap.

**On the tools.** Section 10.3 of the design document names wrk or ab. `ab` was used first and
served for the reference measurements, but it is single threaded and gave up at a thousand
connections with `apr_socket_recv: Operation timed out` — it could not reach the concurrency the
design document asks about. A small purpose-built driver was used beyond that point: one
goroutine per connection, keep-alive throughout, latency recorded per request and reported as
percentiles. It was checked against the backends directly first, at 108,000 requests per second,
to establish that the measurements describe the balancer and not the generator.

## Baseline

The first run against the balancer was far worse than the backends alone, and roughly six per
cent of requests failed outright:

| Connections | Throughput | p50 | p95 | p99 | Failures |
| --- | --- | --- | --- | --- | --- |
| 100 | 5,888 req/s | 4.2 ms | 112.6 ms | 261.1 ms | 6% |
| 1,000 | — | — | — | — | every request failed |

## Bottleneck 1 — upstream connections were not reused

The CPU profile put 93% of the time in raw syscalls, with the balancer's own logic at about 2%.
The process was not computing; it was opening and closing sockets. The logs named the failure
directly:

```
dial tcp 127.0.0.1:5684: connect: can't assign requested address
```

with over a thousand sockets in `TIME_WAIT`. The cause was the transport: `http.DefaultTransport`
keeps **two** idle connections per host, so under load nearly every request opened a fresh TCP
connection to a backend, and the ephemeral port range ran out.

**The change.** The transport's pool is now sized from the configuration: idle connections are
capped at `max_connections` overall and divided across the pool per backend, with a floor of 32,
and the idle timeout follows the configured one. Inbound connections are already capped, so this
bounds the pool without throttling reuse.

| Connections | Before | After |
| --- | --- | --- |
| 100 | 5,888 req/s, p99 261 ms, 6% failures | 28,431 req/s, p99 10 ms, no failures |
| 1,000 | total failure | 35,075 req/s |

## Bottleneck 2 — health checking emptied the pool under load

With connections reused, a second failure appeared at high concurrency: 275,769 requests refused
with `no_backend_available`, and the log showed 172 removals paired with 172 recoveries. Under
saturation the health probes are among the first requests to time out, so every backend was
marked unhealthy at once and the balancer refused all traffic — turning a slow system into a
broken one.

**The change.** When health checking leaves nothing in the pool, the untried backends are offered
anyway, and the fallback is counted as `no_healthy_backend`. This is the behaviour Envoy calls
panic mode: a backend that may still answer is worth one attempt, and a backend that is genuinely
dead costs one failed attempt and the client sees the 503 it would have received regardless.

It also changed a real behaviour, which the integration tests now state explicitly: when every
backend is unhealthy but still answering, the client receives the backend's own response instead
of a 503 from the balancer.

## Bottleneck 3 — a 32 KB allocation per request

The heap profile attributed 80.8% of all allocation to `ReverseProxy.copyBuffer`: a fresh 32 KB
buffer for every response copied, 56 GB over a twenty second run. Live heap stayed small, around
22 MB, so this was pressure on the garbage collector rather than a leak.

**The change.** The reverse proxy is given a `BufferPool` backed by `sync.Pool`. The copy buffer
left the allocation profile entirely.

| Connections | Before pooling | After pooling |
| --- | --- | --- |
| 500 | 32,147 req/s, p99 41.4 ms | 41,829 req/s, p99 32.4 ms |

## Results

After all three changes, with no client-visible failure at any level. The backends here answer a
ten byte body in microseconds, so these figures are the balancer's own ceiling on this machine, not
what a pool of real backends delivers — the second campaign measures that:

| Connections | Throughput | p50 | p95 | p99 | max |
| --- | --- | --- | --- | --- | --- |
| 100 | 40,616 req/s | 2.1 ms | 5.3 ms | 7.7 ms | 26.7 ms |
| 500 | 41,829 req/s | 11.2 ms | 24.0 ms | 32.4 ms | 147.5 ms |
| 1,000 | 41,208 req/s | 23.6 ms | 46.7 ms | 60.2 ms | 122.5 ms |
| 2,000 | 41,421 req/s | 47.3 ms | 87.5 ms | 106.6 ms | 712.6 ms |

Throughput is flat from 100 connections upwards at roughly 41,000 requests per second, which is
where this machine saturates with the generator and the backends sharing it. Latency then grows
in proportion to concurrency, as queueing theory predicts for a saturated server: the work per
request is unchanged, so each additional connection waits behind the others.

Against the baseline, throughput at 100 connections improved sevenfold and p99 fell from 261 ms
to 7.7 ms.

## What this says about the configuration

Appendix B of the [technical design](technical-design/technical-design-v1.8-en.md#appendix-b--deviations-from-the-original-design) records the two entries this report
produced: the fallback when health checking empties the pool (entry 4), and the restated latency
target (entry 10).

Section 10.3 of the design document proposes p95 latency at a thousand concurrent connections in
the single digit to low tens of milliseconds. The measurement is 46.7 ms — above that, on a
machine also running the load generator and all ten backends. At 100 connections, p95 is 5.3 ms,
comfortably inside it. The threshold is worth restating in terms of the load actually expected
rather than as a single number.

The remaining CPU profile is still dominated by syscalls, which for a proxy that mostly moves
bytes between sockets is the expected shape. There is no hot spot left in the balancer's own
code to optimise; further gains would come from the operating system and the network stack.

## Measurements against the profiled backends

The first campaign's backends answered in microseconds with ten bytes. That was the right shape for
finding the balancer's own bottlenecks, and the wrong shape for everything else: nothing was ever in
flight, so least connections had nothing to measure; every backend was identical, so weights and
differences in capacity were never exercised. This campaign repeats the measurements against
`cmd/mockbackend`, whose ten instances have the latencies, capacities and answer sizes the Compose
file gives them.

### Method

| | |
| --- | --- |
| Machine | Apple silicon, 10 cores, macOS; Docker Desktop |
| Balancer | the release image, one container, `configs/lb.measure.yaml` |
| Backends | the ten Compose mock backends: six fast, two medium, one slow, one answering 256 KiB |
| Generator | `cmd/loadgen` on the host: one goroutine per connection, keep-alive, closed loop |
| Levels | 50, 100, 300, 600, 1,000 and 2,000 connections |
| Run | 4 s warmup, discarded, then 15 s measured |
| Repeats | three runs per cell; every figure below is the median, with the range where the runs differ |
| Order | the three algorithms are interleaved at each level, and their order is rotated each repeat |
| Resources | `docker stats` sampled every two seconds through each measured window |
| Algorithms | round robin, weighted round robin with capacity-proportional weights, least connections |

The measurement configuration differs from the example in three ways, each of which would otherwise
measure something other than the balancer: rate limiting is off, because the load comes from one
address and a per-client limit refuses nearly all of it; the weights are proportional to capacity,
so that a weighted run has weights worth following; and logging is at warn, because an info line
per request costs more than the forwarding.

Reproducing the campaign takes three commands — the first brings the stack up, the second runs
eighteen cells three times each, and the third prints the tables below:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.measure.yml up -d
```

```bash
REPEATS=3 scripts/measure.sh
```

```bash
scripts/summarise.py
```

**Caveats.** The generator, the balancer and all ten backends share one machine, so the absolute
figures are a floor; every run carries the same handicap, which is what makes the comparisons
meaningful. The mock backends draw their latencies at random from a log-normal distribution with a
fresh seed per run, which is why each cell is run three times and reported as a median with its
range: a gap narrower than that range is not a result. The generator runs on the host and is
therefore absent from the CPU table below, while competing for the same cores as everything in it.

The generator used for this campaign divided each run's requests by the time until its last request
finished rather than by the measured window. Waiting for the requests still in flight stretched the
window by 2 to 5 per cent at the median and by 12 per cent at worst, so the throughput figures below
are understated by about as much. Every algorithm at a level carried a similar stretch, so the
comparisons hold; the generator has since been corrected to divide by the window itself.

### What the pool can take

The profiles bound the pool before the balancer does. Each backend serves `capacity` requests at
once and queues `queue` more; beyond that it answers 503 immediately, as an overloaded server does.

| Profile | Backends | Latency, median/p99 | Capacity | Queue | Requests per second it can serve |
| --- | --- | --- | --- | --- | --- |
| fast | 1-6 | 15 ms / 80 ms | 64 | 128 | about 4,300 each |
| medium | 7-8 | 30 ms / 200 ms | 32 | 64 | about 1,100 each |
| slow | 9 | 80 ms / 500 ms | 16 | 32 | about 200 |
| large answers | 10 | 15 ms / 80 ms, 256 KiB | 64 | 128 | about 4,300, and 1 GB/s of it |

Two numbers follow from the table and explain most of what the runs show. The pool holds 528
requests at once, so load above that queues and then gets refused. And the slow backend is worth
3.0% of that capacity, so any algorithm that sends it more than that makes it the pool's limit.

### The runs

Fifty-four runs: three algorithms at six load levels, three times each. Every figure is the median
of a cell's three runs, with the range beside it where they differed. *Answered* counts the
requests a backend served; *refused* is the share the client saw as 503.

| Connections | Algorithm | Answered | p95 | p99 | Refused | backend-9 |
| --- | --- | --- | --- | --- | --- | --- |
| 50 | round robin | 1,345/s (1,317–1,381) | 114.4 ms | 238.4 ms | none | 10.0% |
| 50 | weighted round robin | 1,898/s (1,891–1,913) | 70.1 ms | 148.4 ms | none | 3.0% |
| 50 | least connections | 1,858/s (1,816–1,880) | 71.3 ms | 151.6 ms | none | 3.1% |
| 100 | round robin | 2,141/s (2,124–2,149) | 253.0 ms | 397.9 ms | 3.6% | 10.0% |
| 100 | weighted round robin | 3,660/s (3,623–3,694) | 71.7 ms | 150.6 ms | none | 3.0% |
| 100 | least connections | 3,677/s (3,666–3,739) | 71.2 ms | 151.0 ms | none | 2.8% |
| 300 | round robin | 5,523/s (5,405–5,575) | 107.0 ms | 334.3 ms | 7.6% | 10.0% |
| 300 | weighted round robin | 5,082/s (4,897–5,229) | 126.5 ms | 325.4 ms | 0.3% | 3.0% |
| 300 | least connections | 5,352/s (5,299–5,410) | 114.2 ms | 177.4 ms | none | 2.6% |
| 600 | round robin | 4,222/s (4,105–4,364) | 493.8 ms | 875.1 ms | 6.8% | 10.0% |
| 600 | weighted round robin | 3,628/s (3,596–4,333) | 644.2 ms | 1.064 s | none | 3.0% |
| 600 | least connections | 4,053/s (4,004–4,078) | 626.8 ms | 1.038 s | none | 2.9% |
| 1,000 | round robin | 3,710/s (3,666–3,732) | 486.9 ms | 751.3 ms | 6.5% | 10.0% |
| 1,000 | weighted round robin | 3,521/s (3,381–3,567) | 536.6 ms | 774.7 ms | none | 3.0% |
| 1,000 | least connections | 3,636/s (3,607–3,700) | 517.5 ms | 692.6 ms | 1.8% | 5.3% |
| 2,000 | round robin | 3,518/s (3,503–3,668) | 2.248 s | 2.930 s | 6.4% | 10.0% |
| 2,000 | weighted round robin | 3,292/s (3,169–3,627) | 2.691 s | 3.586 s | none | 3.0% |
| 2,000 | least connections | 3,266/s (3,171–3,703) | 2.661 s | 3.251 s | 6.0% | 10.1% |

**The balancer refused nothing.** In all fifty-four runs `lb_rejected_requests_total` and
`lb_retries_total` did not move: every 503 a client saw came from a backend saying it was full, and
with `retry_on_5xx` off the balancer passed those answers through rather than retrying them onto
another backend. The generator reads both counters at the two ends of the measured window, so this
is measured rather than assumed.

**What the machine was doing**, from the `docker stats` samples taken through each measured window.
The generator is not in the table — it runs on the host — and its share is what the missing cores
are spent on.

| Connections | Balancer CPU | Ten backends, together | Busiest backend |
| --- | --- | --- | --- |
| 50 | 21–34% | 19–32% | backend-10 at 2–4% |
| 100 | 39–91% | 36–89% | backend-10 at 4–12% |
| 300 | 125–138% | 123–137% | backend-10 at 18–19% |
| 600 | 86–100% | 86–97% | backend-10 at 13–14% |
| 1,000 | 167–179% | 112–116% | backend-10 at 20–23% |
| 2,000 | 165–187% | 116–125% | backend-10 at 20–22% |

The ranges cover the three algorithms; within a level they differ less between algorithms than
between repeats. At the knee the balancer costs about as much CPU as all ten backends together —
138% against 137% at 300 connections — and above it the balancer keeps climbing while the backends
fall back, because the backends are no longer the part that is busy.

### What the runs show

**Round robin does exactly what it promises, and that is the problem.** Its distribution is 10.0%
per backend at every level, to three digits: the strategy is not misbehaving. But the slow backend
is worth 3.0% of the pool's capacity, so from 100 connections it is past its 200 requests per second
and refusing, and every one of round robin's 503s comes from it. Those refusals never go away: 3.6%
at 100 connections, and between 6.4% and 7.6% at every level above. Equal distribution over an
unequal pool caps the pool at its weakest member — a result about choosing a strategy, not about the
implementation.

**Weights and least connections both find the capacity, by different means.** Weighted round robin
gave the slow backend 3.0% at every level: the capacity-proportional weight, followed exactly. Least
connections arrived at 2.6–3.1% up to 600 connections without being told anything about capacity — a
slower backend holds its requests longer, so it looks busier and is chosen less. The two agree to
within half a point, one from configuration and one from observation.

**Below the pool's capacity that is worth 41% to 72% more throughput.** At 50 connections, 1,898
answered requests per second against 1,345, and at 100 connections 3,677 against 2,141 — with no
refusals against 3.6%, and p95 of 71 ms against 253 ms. This is where the choice of strategy matters
most, and it is the load a pool of this size is meant to carry.

**At the knee the three converge on throughput and separate on everything else.** At 300
connections, where the pool is near its 528 concurrent requests, the three are within 8% of each
other — 5,523, 5,352 and 5,082 answered per second — but round robin refuses 7.6% of requests and
its p99 is 334 ms against least connections' 177 ms. The gain has moved from throughput to the tail
and to whether the client gets an answer at all.

**Above the knee, round robin's throughput lead is bought with refusals.** It answers slightly more
than the others at 600 to 2,000 connections while refusing 6.4% to 6.8%, and an overloaded mock
refuses in microseconds: shedding load is cheap and frees the connection for another request. A
throughput figure that does not separate answers from refusals rewards exactly that.

**Least connections loses its signal under deep overload.** Its share to the slow backend goes 2.6%
at 300 connections, 5.3% at 1,000 and 10.1% at 2,000 — by then no better than round robin, and with
6.0% of requests refused. The cause is the same instant refusal: a backend whose queue is full
answers 503 at once, so its in-flight count drops to nothing and it becomes the least busy backend
in the pool. Counting requests in flight measures occupancy, and an instant refusal is
indistinguishable from idleness. Capacity-proportional weights do not have this failure mode,
because they are not reading anything.

**The machine holds still enough to compare.** Each cell's three runs are spread across the
campaign, so comparing a cell's later runs with its first measures the machine rather than the
strategy: the median cell was at 99.8% of its first run in the second repeat and 99.2% in the third.
That was not true of an earlier attempt at this campaign, and the caveat below says what changed.

**Drift, and what was done about it.** A first version of this campaign ran each algorithm's
eighteen runs in a block, with four seconds between runs. Throughput then fell steadily through the
session — six runs at 300 connections, back to back, went from 5,405 to 3,395 answered per second —
which would have credited whichever algorithm was measured first. Restarting every container did not
recover it, so it was not the containers; the host had no accumulated sockets in `TIME_WAIT` and 56%
of its memory free, so neither of those. Host CPU frequency under sustained load is the remaining
suspect, and confirming it needs `powermetrics` and root, so the cause is recorded as unidentified.
The campaign above answers it by design rather than by diagnosis: the algorithms are interleaved at
each level, their order is rotated each repeat, and the gap between runs is eight seconds. Under
those conditions the drift is no longer measurable, which is what the repeat figures above show.

### Where the client's latency comes from

At 2,000 connections the figures look contradictory: p50 of 54 ms beside p95 of 2.9 s, and a median
*lower* than at 1,000 connections. The distribution explains it. Asking the generator for the shape
rather than the percentiles gives two humps:

| Answered within | Requests | Share |
| --- | --- | --- |
| 20 ms | 6,480 | 10.5% |
| 50 ms | 22,544 | 36.5% |
| 100 ms | 10,916 | 17.7% |
| 200 ms | 4,197 | 6.8% |
| 500 ms | 4,209 | 6.8% |
| 1 s | 214 | 0.3% |
| 2 s | 3,677 | 6.0% |
| 5 s | 9,329 | 15.1% |
| more | 150 | 0.2% |

A request is either answered in about 50 ms or waits for seconds; almost nothing lands in between.
The median sits in the first hump and p95 in the second, which is how the median can fall while the
tail grows. At 1,000 connections the same run has one hump, with 54% of requests between 200 and
500 ms, and the median moves up to where the mass is.

Two measurements say where those seconds are spent. The balancer's own histogram,
`lb_request_duration_seconds`, which is taken inside the handler, reports p50 of 23 ms and p95 of
241 ms for the same traffic — from 98 ms for a fast backend to 499 ms for the slow one. And the
generator's per-backend latencies show p95 between 2.91 s and 3.19 s on all nine backends answering
4 KiB, with the same p50 pattern as the balancer reports:

| Backend | Profile | Requests | Client p50 | Client p95 | Balancer p95 |
| --- | --- | --- | --- | --- | --- |
| 1–6 | fast | 6,364–6,686 each | 43.2–44.0 ms | 2.91–2.97 s | 98–123 ms |
| 7–8 | medium | 5,393, 5,541 | 69.6, 73.2 ms | 3.19, 3.18 s | 229 ms |
| 9 | slow | 5,315 | 274.2 ms | 3.14 s | 499 ms |
| 10 | large answers | 6,201 | 122.4 ms | 404.6 ms | 118 ms |

So about 2.7 seconds of the client's p95 is spent before the request reaches the handler, in the
connection backlog. The pool cannot account for it: a mock's queue is bounded at 128 waiting
requests over 64 workers with a 15 ms median, which is tens of milliseconds, not seconds. And the
wait is the same on every 4 KiB backend, which is what a queue *in front of* the choice looks like —
a queue behind it would differ by backend, as the balancer's own numbers do. With 2,000 connections
against a balancer using about 1.8 cores, on a machine also running the generator and ten backends,
a request is either picked up at once or waits behind the others.

The backend answering 256 KiB is the exception, at 405 ms. The likely reason is the generator: a
worker that has just read a 256 KiB body comes back for its next request later than one that read
4 KiB, so those requests queue less. That part is inference; everything above it is measured.

Two things follow. The latency at 2,000 connections describes the machine more than the balancer,
which is why this report leads with the levels below it. And percentiles alone would have hidden
it: a p95 of 2.9 s reads as a slow system, where the distribution shows a fast system with a
queue in front of it.

### When a backend goes away

Three runs per algorithm and per removal at 600 connections, 30 seconds each, with `backend-1` — a
fast backend carrying 12% of the traffic — taken away 10 seconds into the measurement and started
again afterwards. Both ways of taking it away were measured: `stop` sends SIGTERM, which the mock
handles by finishing the requests it holds and closing its listener, and `kill` sends SIGKILL, which
drops every open connection at once.

| Algorithm | Removal | Answered | p95 | p99 | Refused | Retries |
| --- | --- | --- | --- | --- | --- | --- |
| round robin | drained | 4,155/s (3,739–5,184) | 547.6 ms | 915.2 ms | 7.5% | 11 (10–16) |
| weighted round robin | drained | 2,690/s (2,528–2,751) | 993.3 ms | 1.648 s | none | 12 (7–13) |
| least connections | drained | 2,867/s (2,786–2,867) | 907.7 ms | 1.470 s | none | 10 (9–13) |
| round robin | crashed | 3,111/s (3,089–3,214) | 732.0 ms | 1.172 s | 6.5% | 11 (10–13) |
| weighted round robin | crashed | 2,859/s (2,849–2,902) | 879.9 ms | 1.396 s | none | 18 (10–28) |
| least connections | crashed | 2,913/s (2,874–3,002) | 879.4 ms | 1.411 s | none | 11 (6–12) |

**Losing a backend under load costs between 6 and 28 retries, and no client-visible error.** Each
run carries roughly 90,000 requests, so even the worst case is three retries in ten thousand. Under
weighted round robin and least connections no client saw a 503 at all.

**A crash is barely worse than a planned drain.** The retries are within each other's ranges — 11
against 11 for round robin, 18 against 12 for weighted round robin, 11 against 10 for least
connections — although the crash resets the connections the balancer was holding while the drain
lets them finish. Passive health checking is why: the proxy reports each failed attempt to the
health checker, three consecutive failures take the backend out of the pool, and at these rates
three failures happen within milliseconds. A planned stop produces failures too, as soon as the
listener closes, so both paths end the same way. The active probe, at two seconds and three
thresholds, would have needed up to six seconds to notice either.

**Round robin's refusals are unchanged by the removal** — 7.5% and 6.5% against 6.8% in the steady
run at the same load. They were never about `backend-1`; they are the slow backend, still being sent
three times the traffic it can serve.

### What this says about the configuration

The runs support three recommendations, in the order they matter.

- **Match the algorithm to the pool.** Round robin is right for backends that are alike. For a pool
  that is not, weighted round robin needs weights that reflect capacity, and least connections needs
  nothing at all — which also makes it the safer default when capacities are unknown or change.
- **Least connections is not a substitute for an admission limit.** It tracks capacity well until
  the pool is deeply overloaded, and then follows a signal that overload has destroyed. A limit on
  the requests in flight per backend would fix what it cannot see; the balancer has no such setting
  today, and that is the clearest gap these measurements found.
- **Read throughput and refusals together.** In this pool, the configuration with the highest
  requests per second was also the one refusing 7% of them.

## Keeping the fixes

The load test above needs the whole environment and a quiet machine, so it cannot run on every
change. After the release, the two fixes that a harmless looking edit could undo were each given
a test that runs with the ordinary suite, and the hot paths were given benchmarks.

**Guards.** Both run in CI under `go test ./...` and fail the build.

| Test | What it measures | Healthy | With the fix reverted |
| --- | --- | --- | --- |
| `TestUpstreamConnectionsAreReused` (`internal/proxy/transport_test.go`) | backend connections opened by 5 rounds of 24 concurrent requests; the limit is 48 | 24 | 112 with two idle connections per host, fails |
| `TestForwardingAllocatesLessThanACopyBuffer` (`internal/proxy/budget_test.go`) | bytes allocated per forwarded request; the limit is one 32 KB copy buffer | about 13 KB | about 46 KB without the buffer pool, fails |

Both were checked by reintroducing the original bug and watching them fail. The second test gates
on bytes rather than on the number of allocations, because the count is 104 either way: the
missing pool changes the size of one allocation, not how many there are.

**Benchmarks.** One run on the machine described under Method:

| Benchmark | Time per operation | Allocations |
| --- | --- | --- |
| Round robin, 10 and 100 backends | 1.9 ns, 1.8 ns | none |
| Least connections, 10, 100 and 1,000 backends | 14 ns at every size | none |
| Weighted round robin, 10 and 100 backends | 177 ns, 1.95 µs | none |
| Round robin, least connections, weighted round robin, 10 goroutines | 36 ns, 2.8 ns, 274 ns | none |
| Health lookup, alone and alongside reports | 7.6 ns, 35 ns | none |
| Rate limiter, one client and many clients | 12 ns, 102 ns | none |
| Forwarding one request, sequential and parallel | 32 µs, 11 µs | 13 KB, 104 |

What they show:

- Selection is cheap next to forwarding. The slowest case, weighted round robin over a hundred
  backends, is about 6% of the cost of forwarding one request; over ten backends it is under 1%.
- Weighted round robin grows linearly with the pool, since it visits every backend to choose one.
  Round robin does not, and neither does least connections since it compares two random backends
  instead of scanning: the scan it replaced took 484 ns at a thousand backends.
- Round robin is the fastest strategy alone and slows twentyfold under parallel load, because
  every goroutine increments the same counter and the cache line holding it moves between cores.
  Least connections only reads shared state and does not pay that cost.
- Nothing on the request path allocates except forwarding itself.

**Tracking.** The benchmarks workflow (`.github/workflows/benchmarks.yml`) runs them on every
push to `main`, runs them again on the code as it was before the push, on the same runner, and
compares the two with benchstat in the run's summary. It never fails the build: shared runners
vary by several per cent between identical runs, which is too much to gate on. Timing changes
under about 10% are noise there; any change in bytes or allocations per operation is real.
