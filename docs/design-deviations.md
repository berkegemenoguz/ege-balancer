# Deviations from the design document

The technical design document is the source of truth for what this project builds. Where the
implementation departs from it, the reason is recorded here, so that the document can be brought
back in step deliberately rather than by accident.

Each entry names the section of the design document it touches and the day it was decided; the
[development log](development-log/) page for that day has the surrounding detail. The
[revision notes](technical-design/) turn these entries into the concrete edits the document needs,
in Turkish and English.

---

## 1. Branching and pull requests are not used

**Section 7.1 and 7.3, and the version control criterion in section 11.** Day 2.

The document prescribes a protected `main`, `feature/<module>` branches, and every change merged
through a pull request that CI has passed.

**What is done instead:** work is committed straight to `main`, and CI runs on every push rather
than on pull requests. The quality gate is unchanged — nothing reaches `main` without gofmt,
`go vet`, golangci-lint, govulncheck, build and the full test suite passing — but the branching
ceremony buys nothing with one developer and no reviewer.

**If the team grows,** the branch protection and pull request flow described in the document
should be turned back on; the CI pipeline already runs the checks a pull request would need.

---

## 2. Weighted round robin is deterministic, not probabilistic

**Section 5.3, and the day 5 row of the plan in section 9.** Day 5.

The document describes weighted selection as proportional and probabilistic.

**What is done instead:** the smooth weighted round robin algorithm, which is deterministic. Each
selection raises every backend's credit by its weight, the backend with the most credit serves
the request, and its credit then drops by the total weight.

**Why:** it hits the configured ratio exactly rather than approximately, needs no random source,
and spreads a heavy backend's turns across the cycle instead of bunching them. With weights
5, 1, 1 the sequence is `a a b a c a a`. It also makes the tests exact instead of statistical:
1200 requests over weights 1, 2 and 3 produce exactly 200, 400 and 600.

---

## 3. The health checker interface carries the passive path

**Section 6.2.** Day 6.

The example interface in the document is `Start(ctx, backends)` and `IsHealthy(addr)`.

**What is done instead:** the interface also has `ReportSuccess(addr)` and `ReportFailure(addr)`.

**Why:** section 6.1 requires passive health checking — a consecutive failure counter fed by real
traffic — and the proxy has no way to feed it through the interface as written. Both paths now
increment the same counters, so a backend that fails real traffic is removed without waiting for
the next probe.

---

## 4. An empty pool is still offered the request

**Related to section 5.5, which does not cover this case.** Day 10.

The document describes what happens when a backend fails, but not what the balancer should do
when health checking has marked every backend unhealthy at once.

**What is done instead:** when nothing in the pool is healthy, the untried backends are offered
the request anyway, and the fallback is counted separately as `no_healthy_backend`. This is what
Envoy calls panic mode.

**Why:** the load test made the case concrete. Under saturation the health probes are among the
first requests to time out, so every backend was marked unhealthy at once and the balancer
refused 275,769 requests — turning a slow system into a broken one. A backend that may still
answer is worth one attempt; a backend that is genuinely dead costs one failed attempt and the
client sees the same 503 it would have received anyway.

**Observable consequence:** when every backend is unhealthy but still answering, the client now
receives the backend's own response instead of a 503 from the balancer.

---

## 5. Two fields added to the configuration schema

**Section 3.3.** Days 8 and 10.

- `metrics_addr` (default `:8081`) — the address serving `/metrics` and `/status`. Serving them
  on the traffic port would mean clients could never proxy those two paths to a backend, and
  would expose internal state to anyone who can reach the balancer. The observability server also
  carries no connection limit, so it keeps answering exactly when the traffic port is saturated.
- `enable_pprof` (default `false`) — adds Go's profiling endpoints to the metrics server. Off by
  default because they hand out heap and goroutine state.

---

## 6. A response timeout was added to the timeouts block

**Section 3.3.** Day 11.

The document's timeouts are connect, read, write and idle. Read and write bound the conversation
with the client; connect bounds reaching a backend. Nothing bounded how long a backend may take
to *start answering* once connected.

**What is done instead:** `timeouts.response_timeout` was added, defaulting to the read timeout
when omitted. Without it a backend that accepts the connection and then stalls holds the request
until the client-side write timeout kills it, and the failure policy never gets the chance to try
another backend. With it, a stalled backend is abandoned and the request is retried elsewhere,
which is what the resilience scenario in section 10.4 asks for.

---

## 7. Grafana needs a memory budget, not just a bigger limit

**Section 7.6.** Days 8 and 12.

The document gives both monitoring containers a 256 MB memory limit.

**What is done instead:** Prometheus keeps 256 MB. Grafana was given a 1 GB limit and, more
importantly, a `GOMEMLIMIT` of 768 MiB.

**Why:** Grafana 13 idles inside 256 MB but is killed the moment a dashboard is rendered — the
container exited with code 137 and `OOMKilled: true`. Raising the limit to 512 MB was not enough
either: with a five second refresh and a browser watching, it was killed again.

Measuring showed why. Under continuous load Grafana's memory climbed steadily — 587 MiB at one
minute, 751 MiB at four, still rising. That is how the Go runtime behaves when it does not know
it is constrained: it lets the heap grow and collects late, so a higher ceiling only postpones
the kill. Told its budget through `GOMEMLIMIT`, it collects before reaching the limit: the same
load plateaued at about 774 MiB and stayed there, with no restart over five minutes.

The remaining 256 MB of the container limit is headroom for what the runtime allocates outside
the heap.

Prometheus, scraping ten backends every five seconds, uses 143 MiB and keeps its 256 MB as
specified. The scrape interval in the same stack is 5s rather than the 15s the document suggests,
and the dashboard refreshes every 5s to match: a dashboard that reacts within a few seconds is
what makes the monitoring stack useful while watching a change take effect. The production
guidance stays as written.

---

## 8. Load testing used a purpose-built driver above 500 connections

**Section 10.3.** Day 10.

The document names wrk or ab.

**What is done instead:** `ab` produced the reference measurements, but it is single threaded and
gave up at a thousand connections with `apr_socket_recv: Operation timed out` — below the
concurrency the document asks about. A small driver was written for the higher levels: one
goroutine per connection, keep-alive throughout, percentiles from recorded latencies. It was
pointed at a backend directly first, at 108,000 requests per second, to establish that the
measurements describe the balancer rather than the generator.

The driver was not kept in the repository: it exists to produce one report, and the report
carries its numbers.

---

## 9. A demo console was added, outside the document's scope

**Not in the design document.** After v1.0.

`cmd/demo` is a local console that brings the demo stack up and offers the actions one would
otherwise type: sustained traffic, measuring the distribution, stopping and starting backends,
switching the algorithm, demonstrating the rate limit, and putting everything back. It exists so
that a demonstration does not depend on typing long commands correctly under time pressure.

It is a development tool, not part of the product: it shells out to `docker`, writes to the
configuration file, never listens on a socket, and is excluded from the container image. Every
action prints the command it runs, so the console stays a shortcut for typing rather than a layer
that hides what happens.

---

## 10. The latency target needs restating

**Section 10.3.** Day 10.

The document proposes p95 latency at a thousand concurrent connections in the single digit to low
tens of milliseconds.

**What was measured:** 46.7 ms at a thousand connections, and 5.3 ms at a hundred — on a machine
that was also running the load generator and all ten backends. The target is met at the lower
level and missed at the higher one, but the test conditions are not those of a deployment where
the balancer has the machine to itself. The threshold is worth restating in terms of the load
actually expected, rather than as one number.
