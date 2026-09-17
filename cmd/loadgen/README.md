# Load generator

Closed-loop HTTP load through the balancer, with the report the measurements in
[the performance report](../../docs/performance-report.md) quote: throughput, latency percentiles,
the status codes the client saw, the share each backend served, and what the balancer's own counters
did while the load ran.

It is a development tool. It lives in the repository so that the numbers in the report can be
produced again instead of being taken on trust — the first campaign's driver was thrown away, and
with it the ability to repeat it.

## Running it

With the stack up and the measurement configuration mounted:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.measure.yml up -d
```

```bash
go run ./cmd/loadgen -connections 300 -duration 20s -label round_robin
```

```
300 connections for 20.339s · round_robin
  requests   104098 (5118/s, 169.7 MB/s)
  latency    p50 47.6ms   p95 117.3ms   p99 184.9ms   max 1.2117s
  statuses   200: 104098
  backends   backend-1 12643 (12.1%)   ...   backend-9 2792 (2.7%)
```

`scripts/measure.sh` drives the whole matrix — every algorithm at every load level, `REPEATS`
times each — and samples `docker stats` alongside every run. `scripts/measure-failure.sh` takes a
backend away in the middle of a run. `scripts/summarise.py` turns the results into the report's
tables: the median of each cell with its range, the CPU each container used, and the latency
distribution at one level.

## What it measures, and how

- **One goroutine per connection**, keep-alive throughout, each sending the next request as soon as
  the answer to the last one is read. The load is therefore bounded by the pool's own speed: this
  measures what the system delivers, not a rate chosen in advance.
- **A warmup, discarded.** Connections are still being opened in the first seconds, and the
  balancer's upstream pools are still filling. `-warmup` defaults to five seconds.
- **A clean stop.** When the time is up, workers finish the request they are on rather than being
  cut off. A cancelled request fails every attempt inside the balancer and is counted there as a
  refusal the load never caused.
- **The backend that answered**, from the `X-Backend` header the mock backends set, which is how
  the distribution is reported without reading the balancer's metrics.
- **The balancer's own counters**, read from `/metrics` at both ends of the measured window, so
  retries and refusals belong to the run rather than to everything since the balancer started.
- **The shape of the latency**, as a histogram in the JSON and, with `-histogram`, on the terminal.
  Percentiles hide whether a run has one hump or two, and under overload it has two.

Percentiles are by nearest rank over every recorded latency; nothing is sampled away.

## Flags

| Flag | Default | |
| --- | --- | --- |
| `-addr` | `http://127.0.0.1:8080` | balancer to drive |
| `-path` | `/` | path to request |
| `-connections` | 100 | connections held open, one goroutine each |
| `-duration` | 20s | how long to measure |
| `-warmup` | 5s | load applied before measuring, and discarded |
| `-timeout` | 10s | timeout of a single request |
| `-metrics` | `http://127.0.0.1:8081/metrics` | counters endpoint; empty to skip |
| `-histogram` | off | also print the latency distribution |
| `-label` | — | recorded with the result, such as the algorithm in force |
| `-json` | — | also write the result as JSON to this file |
