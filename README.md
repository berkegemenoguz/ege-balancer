# Ege-Balancer

A modular, high-performance HTTP reverse proxy and load balancer written in Go.

Incoming HTTP traffic is distributed across multiple backends using a configurable strategy,
unhealthy backends are taken out of the pool automatically, failures are handled by a policy you
choose, and the whole system is observable through structured logs and Prometheus metrics.

> Status: day 12 of the 12-day plan — feature complete and released as v1.0.0. Everything below
> is implemented and tested, except where the *Out of scope for v1.0* list says otherwise.

## Features

- **Three load balancing algorithms**, selectable from configuration: round robin, least
  connections (the less busy of two backends drawn at random, so its cost does not grow with the
  pool and idle backends share the traffic), and weighted round robin (smooth, so a heavy
  backend's turns are spread across the cycle rather than bunched together)
- **Health checking**, active and passive: a periodic HTTP probe, plus the outcome of real
  traffic, feeding the same consecutive-failure thresholds. An unhealthy backend leaves the pool
  and rejoins when it recovers
- **Configurable failure policies**: `retry_next_backend` (never twice to the same backend),
  `fail_fast`, and `circuit_breaker` with a half-open probe
- **A retry budget**: retries in flight are capped at a share of the requests in flight, so a
  failing pool is not sent several times its traffic at the moment it can least absorb it
- **Resource protection**: per-IP rate limiting, a connection cap enforced at the listener, a
  request body limit, and timeouts on every phase of a request
- **Request validation**: ambiguously framed requests are refused before a backend sees them,
  and `X-Forwarded-For` is rewritten so a client cannot forge its own address
- **Graceful shutdown**: on SIGINT or SIGTERM the balancer stops accepting connections and lets
  the requests already in flight finish
- **Configuration reload on SIGHUP**: backends, weights, algorithm, failure policy and limits
  change without dropping a connection; an invalid file is refused and the balancer keeps
  running on what it had
- **Observability**: structured JSON logs, Prometheus metrics, a JSON status endpoint, and
  optional profiling endpoints

- **Shipped as a container**: a multi-stage build on a distroless base, running as a non-root
  user, published on every tag

### Out of scope for v1.0

- TLS termination and HTTP/2 — the balancer speaks plain HTTP
- Distributed or multi-node balancing, and service discovery
- Sticky sessions — backends are assumed stateless

## Requirements

- Go 1.27 or newer
- Docker with Compose, for the mock backend environment and the monitoring stack. Not needed to
  build, test or run the balancer itself.

## Getting started

Start the ten mock backends, plus Prometheus and Grafana:

```bash
docker compose -f deploy/docker-compose.yml up -d
```

Each backend answers with its own name, so you can see which one served a request:

```bash
curl localhost:5681
```

Build and run the balancer against them:

```bash
go build -o bin/lb ./cmd/lb && ./bin/lb -config configs/lb.localhost.yaml
```

Send it some traffic and watch the distribution:

```bash
for i in $(seq 20); do curl -s localhost:8080; echo; done | sort | uniq -c
```

Or drive all of it from one place — the [demo console](cmd/demo/) starts the stack and offers the
same actions as menu entries, printing the command behind each one:

```bash
go run ./cmd/demo
```

Run the tests:

```bash
go test -race -cover ./...
```

That includes `internal/integration`, which starts the balancer on real sockets against mock
backends and drives it over HTTP. It needs no Docker and runs in CI with everything else.

## Running in Docker

The whole environment, balancer included, comes up together:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

Or run a published image against your own configuration:

```bash
docker run --rm -p 8080:8080 -p 8081:8081 \
  -v "$PWD/configs/lb.yaml:/etc/lb/config.yaml:ro" \
  ghcr.io/berkegemenoguz/ege-balancer:latest
```

The image contains the binary and nothing else — no shell, no package manager — and runs as a
non-root user. Because there is no shell in it, a container healthcheck is not defined; `/status`
on the metrics port serves that purpose from outside.

## Configuration

Two configurations ship with the project, both documenting the full schema:

- `configs/lb.example.yaml` — backends addressed by their compose service names. Use it when the
  balancer runs inside the compose network.
- `configs/lb.localhost.yaml` — the same, with the backends addressed on the loopback ports the
  compose file publishes. Use it when the balancer runs on the host, which is how development
  works today.

Copy either to `configs/lb.yaml` for local changes; that path is gitignored.

Send `SIGHUP` to reload the file without restarting:

```bash
kill -HUP $(pgrep -f 'bin/lb')
```

Backends, weights, the algorithm, the failure policy, health check settings and the limits are
applied straight away. Settings bound to a socket — `listen_addr`, `metrics_addr`,
`max_connections`, the timeouts and `enable_pprof` — need a restart; a reload applies everything
else and logs which settings it left alone. An invalid file is refused in full, and the balancer
carries on with the configuration it already had. `/status` reports how many reloads have been
applied.

The numeric values in both are starting points. The [performance report](docs/performance-report.md)
records what the load test says about them.

Under `retry_next_backend`, `retry.max_retries` bounds the retries of one request and the retry
budget bounds them across all requests: at most `retry.budget_percent` (default 20) of the
requests in flight may be retries, and `retry.min_retry_concurrency` (default 3) are always
allowed, so light traffic can still be retried. A retry the budget refuses is answered with 503
and counted as `retry_budget_exhausted`. Both settings apply on reload.

## Observability

The balancer serves two ports: proxied traffic on `listen_addr`, and observability on
`metrics_addr`. Keeping them apart means `/metrics` and `/status` stay reachable when the traffic
port is saturated, and neither path is taken away from the backends.

- `/metrics` — Prometheus format: requests by backend and status, a latency histogram, backend
  failures, retries sent, rejected requests by reason, and live gauges for active connections
  and health
- `/status` — a JSON summary for a person: algorithm, healthy count, applied reload count, and
  each backend's weight, health and active connections
- `/debug/pprof/` — Go's profiling endpoints, served only when `enable_pprof` is set. They expose
  heap and goroutine state, so they are off by default.

The compose environment includes Prometheus (`localhost:9090`) and Grafana (`localhost:3000`,
dashboard *Ege-Balancer*, no login). Prometheus scrapes the balancer on the host at
`host.docker.internal:8081`, so the stack works while the binary runs outside Docker.

## Performance

Measured on a ten core machine that was also running the load generator and all ten backends,
after the three bottlenecks the profiling found:

| Concurrent connections | Throughput | p50 | p95 | p99 |
| --- | --- | --- | --- | --- |
| 100 | 40,616 req/s | 2.1 ms | 5.3 ms | 7.7 ms |
| 1,000 | 41,208 req/s | 23.6 ms | 46.7 ms | 60.2 ms |
| 2,000 | 41,421 req/s | 47.3 ms | 87.5 ms | 106.6 ms |

No request failed at any level. The [performance report](docs/performance-report.md) has the
method, the bottlenecks and the before-and-after numbers.

## Project layout

```
cmd/lb/                    entry point: reads configuration, builds the logger, runs the app
cmd/demo/                  local console for driving the demo stack
internal/app/              wiring, shared by the binary and the integration tests
internal/config/           configuration parsing, defaults and validation
internal/balancer/         LBStrategy interface and the three algorithms
internal/health/           active and passive health checking
internal/proxy/            proxy core: forwarding, failure policies, rate limiting, validation
internal/server/           listeners, connection limit and graceful shutdown
internal/observability/    structured logging, Prometheus metrics, status and pprof endpoints
internal/integration/      end-to-end tests over real sockets
configs/                   example configurations
deploy/                    compose environment, Prometheus and Grafana provisioning
docs/                      design document, development log and performance report
```

Modules talk to each other through interfaces — `balancer.LBStrategy`, `health.Checker` — so an
implementation can be replaced without touching the packages that use it.

## Documentation

- [Technical design](docs/technical-design/) — the design as a paper, in English and Turkish:
  architecture, algorithms, failure handling and the evaluation. The original design the project
  was built to and its revision notes are kept beside it.
- [Development log](docs/development-log/) — one page per day, and one for the work after the
  release: what was built, which decisions were taken and why, what went wrong, and how the result
  was verified.
- [Performance report](docs/performance-report.md) — load testing method, the bottlenecks
  profiling exposed, and throughput and latency before and after each fix.
- [Design deviations](docs/design-deviations.md) — every place the implementation departs from
  the design document, with the reasoning.
- [Deployment checklist](docs/deployment-checklist.md) — the production readiness criteria and
  their evidence, plus what to check before a release and before real traffic.

## Development

Work goes straight to `main`, and CI runs on every push: gofmt, `go vet`, golangci-lint,
govulncheck, build, and the full test suite under `-race`. Commit messages follow
[Conventional Commits](https://www.conventionalcommits.org/).

The suite includes two guards against the bottlenecks the profiling fixed: upstream connections
must be reused, and forwarding must not allocate a copy buffer per request. Benchmarks cover the
hot paths — selection, health lookups, rate limiting and forwarding:

```bash
go test -run '^$' -bench . -benchmem ./...
```

A separate workflow runs them on every push and compares them with the code before the push; the
comparison is in the run's summary on GitHub. See the
[performance report](docs/performance-report.md#keeping-the-fixes) for what they measure.

## License

[MIT](LICENSE)
