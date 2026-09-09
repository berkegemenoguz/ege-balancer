# Day 10 — Load testing and profiling

**Planned deliverable:** a performance report and optimised code, from load testing at rising
concurrency and profiling with pprof.

The measurements and the full before-and-after tables are in
[the performance report](../performance-report.md). This page records how the day went.

## What was built

- Go's profiling endpoints on the metrics server, behind a new `enable_pprof` configuration
  field that defaults to off.
- Three changes driven by the measurements: a properly sized upstream connection pool, a
  fallback for the case where health checking empties the pool, and a pooled copy buffer.
- A load driver and a fast mock backend, both kept outside the repository as measurement tools.

## Decisions

**pprof is off by default and lives on the metrics port.** Those endpoints hand out heap and
goroutine state, which an operator should have to reach for deliberately. On the metrics port
they are already off the path clients can reach.

**The load generator was written rather than installed.** `ab` is what the design document names
and it was used for the reference runs, but it is single threaded and failed at a thousand
connections with `apr_socket_recv: Operation timed out`, which is below the concurrency the plan
asks about. Rather than change the plan to fit the tool, a small driver was written: one
goroutine per connection, keep-alive throughout, percentiles from recorded latencies. It was
pointed at a backend directly first — 108,000 requests per second — to prove the numbers describe
the balancer and not the generator.

**The measurement tools stayed out of the repository.** They exist to produce one report, and the
report carries their numbers; carrying the tools as project code would mean maintaining them.

## What went wrong

The whole day was finding out what was wrong, so this section is the substance of it.

**The balancer was eighteen times slower than the backends behind it, and lost six per cent of
requests.** The CPU profile said 93% syscalls against 2% in our own code, which ruled out the
balancing logic immediately and pointed at connection handling. The logs then named it exactly:
`connect: can't assign requested address`, with a thousand sockets in `TIME_WAIT`. Go's default
transport keeps two idle connections per host, so the proxy was opening a new connection for
almost every request and exhausting the ephemeral port range. Sizing the pool from the
configured connection budget took throughput at 100 connections from 5,888 to 28,431 requests
per second and removed the failures.

This is worth remembering as a general point: the default `http.Transport` is tuned for a
program that talks to a few hosts occasionally, not for a proxy. Reusing it unchanged was the
single biggest mistake in the codebase so far, and it was invisible in every test until real
load arrived.

**Then the balancer started refusing everything at high concurrency.** 275,769 requests rejected
with `no_backend_available`, and the log showed 172 removals matched by 172 recoveries — the
whole pool flapping. Under saturation the health probes time out before client traffic does, so
every backend was declared unhealthy at once. Refusing all traffic because the balancer is busy
is worse than trying a backend that might answer, so an empty pool now falls back to the untried
backends, counted separately as `no_healthy_backend`.

That changed observable behaviour, and an integration test caught it within seconds: a pool of
backends answering 500 used to give the client a 503 from the balancer, and now gives the client
the backend's own 500. The test was split in two — one for backends that are unreachable, which
still produces 503, and one stating the new behaviour explicitly.

**A counter disagreed with what the clients saw.** The metrics recorded 1,138 rejections while
the load generator reported no failed request at all. The explanation was in the arithmetic: 3,414
backend failures divided by three attempts per request is exactly 1,138. Those were requests the
generator itself cancelled as each run ended; the balancer dutifully retried them three times and
answered a client that had already gone. Not a defect, but a reminder that a rejection counter
counts what the balancer decided, not what a client experienced.

**A 32 KB allocation per request was hiding in plain sight.** The heap profile put 80.8% of all
allocation in `ReverseProxy.copyBuffer` — 56 GB over twenty seconds. Live heap was only 22 MB,
so nothing leaked and no test would ever have noticed; it was pure garbage collector pressure.
A `sync.Pool` of copy buffers removed it from the profile entirely and added 27% throughput.

## Verification

After the three changes, at 100 through 2,000 connections, with no client-visible failure at any
level: throughput flat at about 41,000 requests per second, p99 from 7.7 ms at 100 connections to
106.6 ms at 2,000. Against the baseline that is seven times the throughput at 100 connections,
with p99 down from 261 ms to 7.7 ms.

The full unit and integration suites pass unchanged apart from the one test whose expectation the
panic-mode fallback deliberately changed.
