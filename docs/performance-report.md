# Performance report

Load testing and profiling of the load balancer, carried out on day 10 of the plan against the
ten mock backends. It records the method, the two bottlenecks the measurements exposed, the
changes made in response, and the numbers before and after.

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

After all three changes, with no client-visible failure at any level:

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

See [design deviations](design-deviations.md) for the two entries this report produced: the
fallback when health checking empties the pool, and the restated latency target.

Section 10.3 of the design document proposes p95 latency at a thousand concurrent connections in
the single digit to low tens of milliseconds. The measurement is 46.7 ms — above that, on a
machine also running the load generator and all ten backends. At 100 connections, p95 is 5.3 ms,
comfortably inside it. The threshold is worth restating in terms of the load actually expected
rather than as a single number.

The remaining CPU profile is still dominated by syscalls, which for a proxy that mostly moves
bytes between sockets is the expected shape. There is no hot spot left in the balancer's own
code to optimise; further gains would come from the operating system and the network stack.
