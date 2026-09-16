# Ege-Balancer

A modular HTTP reverse proxy and load balancer written in Go.

Incoming HTTP traffic is spread across a pool of backends by a strategy you choose, unhealthy
backends leave the pool on their own and come back when they recover, failures are handled by a
policy you choose, and the whole system can be watched through structured logs, Prometheus metrics
and Grafana dashboards.

> Status: v1.3.0. The twelve day plan was released as v1.0.0, and the releases since have added a
> retry budget and least connections by the power of two choices (v1.1.0); liveness and readiness
> endpoints, a container health check, profiled mock backends and three Grafana dashboards
> (v1.2.0); and a retry rule for requests that are not idempotent, with an identifier on every
> request (v1.3.0). Everything below is implemented and tested, except where *Out of scope* says
> otherwise.

## Contents

- [Features](#features)
- [Requirements](#requirements)
- [Quick start: the demo console](#quick-start-the-demo-console)
- [Running it by hand](#running-it-by-hand)
- [Tests and benchmarks](#tests-and-benchmarks)
- [Configuration](#configuration)
- [Observability](#observability)
- [Performance](#performance)
- [Project layout](#project-layout)
- [Documentation](#documentation)
- [Development](#development)
- [License](#license)

## Features

- **Three load balancing algorithms**, chosen in configuration: round robin; least connections,
  which sends each request to the less busy of two backends drawn at random, so its cost does not
  grow with the pool and idle backends share the traffic; and smooth weighted round robin, which
  spreads a heavy backend's turns across the cycle rather than bunching them together
- **Health checking**, active and passive: a periodic HTTP probe and the outcome of real traffic
  feed the same thresholds, so a backend leaves the pool as soon as either shows it failing
- **Failure policies**: `retry_next_backend` (never twice to the same backend), `fail_fast`, and
  `circuit_breaker` with a half-open probe
- **A retry budget**: retries in flight are capped at a share of the requests in flight, so a
  failing pool is not sent several times its traffic at the moment it can least absorb it
- **Resource protection**: per-client rate limiting, a connection cap, a request body limit, and a
  timeout on every phase of a request
- **Request validation**: ambiguously framed requests are refused before a backend sees them, and
  `X-Forwarded-For` is rewritten so a client cannot forge its own address
- **Graceful shutdown** on SIGINT or SIGTERM, letting requests already in flight finish
- **Configuration reload on SIGHUP** without dropping a connection; an invalid file is refused and
  the balancer keeps running on what it had
- **Observability**: structured logs carrying a request identifier, Prometheus metrics, a JSON
  status endpoint, liveness and readiness endpoints, optional profiling endpoints, and three
  Grafana dashboards
- **Shipped as a container**: a 22.6 MB distroless image running as a non-root user, published on
  every tag

### Out of scope

- TLS termination and HTTP/2 — the balancer speaks plain HTTP
- Distributed or multi-node balancing, and service discovery
- Sticky sessions — backends are assumed stateless

## Requirements

- **Go 1.27** or newer, to build, test and run everything
- **Docker with Compose**, for the demo environment: ten mock backends, the balancer, Prometheus
  and Grafana. Docker Desktop needs about 4 GB of memory for the whole stack. The balancer itself
  and its tests do not need Docker.
- **curl 7.84** or newer for the distribution command below, which reads a response header

## Quick start: the demo console

The quickest way to see the balancer working is the demo console, a menu that runs in your
terminal. From the repository root:

```bash
go run ./cmd/demo
```

It checks that Docker is running, starts the whole environment with
`docker compose up -d --build`, and waits until the balancer reports all ten backends healthy. The
first run builds the images and can take a few minutes; later runs start in seconds. Then it
offers the actions you would otherwise type by hand:

```
Actions
  1  start or stop sustained traffic
  2  measure the distribution over 30 requests
  3  stop a backend            4  start a backend
  5  change the algorithm      6  demonstrate the rate limit
  7  show the status           r  reset everything
  q  quit                      ?  show this again
```

The prompt shows the algorithm in force and whether traffic is flowing, so a change made several
actions ago cannot quietly make the next measurement look wrong:

```
[least_connections · traffic on] action?
```

A good first tour:

1. Press `1` to start traffic of about 20 requests a second, and open the
   [Overview dashboard](http://localhost:3000/d/ege-balancer) — Grafana needs no login.
2. Press `2` to measure which backend answered each of 30 requests.
3. Press `5` and choose least connections. The reload appears as a marker on every graph, and on
   the [Backends dashboard](http://localhost:3000/d/ege-balancer-backends) the slow backend's share
   of traffic starts to fall.
4. Press `3` and stop a backend. Its health timeline turns red after three failed checks, and the
   traffic moves to the others; `4` brings it back.
5. Press `6` to watch the rate limit refuse a burst and let a second one through after it refills.

Every action prints the command it runs before running it, so anything the console does can be
repeated by hand. The algorithm and rate limit actions edit `configs/lb.example.yaml` and send
SIGHUP; `r` restores the file at any time, and `q` restores it on the way out and then asks
whether to stop the stack. The [console's own page](cmd/demo/) has the details.

It is a development tool: it drives Docker on your machine and never listens on a socket. It is
not part of the container image.

### The mock backends

The ten backends are one program, `cmd/mockbackend`, run with different profiles so that the pool
behaves like a real one rather than answering everything at once:

| Backends | Profile | Latency (median, p99) | Capacity, queue | Answer |
| --- | --- | --- | --- | --- |
| 1–6 | fast | 15 ms, 80 ms | 64, 128 | 4 KiB |
| 7–8 | medium | 30 ms, 200 ms | 32, 64 | 4 KiB |
| 9 | slow | 80 ms, 500 ms | 16, 32 | 4 KiB |
| 10 | large answers | 15 ms, 80 ms | 64, 128 | 256 KiB |

A request waits for a worker and holds it while it is served, so a backend with little capacity
slows down under load as its queue grows; beyond the queue it answers 503. Each backend names
itself in an `X-Backend` header and on the first line of its answer, and they are published on
ports 5681 to 5690:

```bash
curl -s localhost:5689 | head -1
```

The profiles live in `deploy/docker-compose.yml`; every setting is a flag or a `MOCK_` environment
variable, listed by `go run ./cmd/mockbackend -h`.

## Running it by hand

### Everything in Docker

The same environment the console starts:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

The balancer serves traffic on port 8080 and its status on 8081:

```bash
curl -s localhost:8081/status
```

`/readyz` on the same port answers 200 while there is a healthy backend to send a request to, and
`docker compose -f deploy/docker-compose.yml ps` shows the balancer as `healthy` once its own
health check passes.

Send it traffic and count which backend answered:

```bash
for i in $(seq 20); do curl -s -o /dev/null -w '%header{x-backend}\n' localhost:8080; done | sort | uniq -c
```

Edit `configs/lb.example.yaml`, which the container reads, and apply it without a restart:

```bash
docker compose -f deploy/docker-compose.yml kill -s HUP loadbalancer
```

Compose reports "Killed" for any signal it sends; the balancer keeps running. Stop everything with:

```bash
docker compose -f deploy/docker-compose.yml down
```

### The balancer on your machine

To run the balancer from source against the same backends — to debug it, or to try a change
without rebuilding the image — stop the container first, because both listen on ports 8080 and
8081:

```bash
docker compose -f deploy/docker-compose.yml stop loadbalancer
```

```bash
go build -o bin/lb ./cmd/lb && ./bin/lb -config configs/lb.localhost.yaml
```

`configs/lb.localhost.yaml` addresses the backends on their published ports. Reload it with
`kill -HUP $(pgrep -f 'bin/lb')`. Prometheus keeps scraping the container, so the dashboards stay
empty until `deploy/prometheus.yml` is pointed at the host, as the comments in that file explain.

### A published image

```bash
docker run --rm -p 8080:8080 -p 8081:8081 \
  -v "$PWD/configs/lb.yaml:/etc/lb/config.yaml:ro" \
  ghcr.io/berkegemenoguz/ege-balancer:latest
```

The image holds the binary and nothing else — no shell, no package manager, no curl. A container
health check therefore runs the binary itself:

```bash
docker exec <container> /usr/local/bin/lb -probe http://127.0.0.1:8081/healthz
```

`-probe` requests the URL and exits 0 if it answers 200 and 1 otherwise, which is what Docker and
Compose health checks expect.

## Tests and benchmarks

```bash
go test -race -cover ./...
```

That includes `internal/integration`, which starts the assembled balancer on real sockets and
drives it over HTTP; it needs no Docker. The suite also guards the fixes the load test led to:
upstream connections must be reused, and forwarding must not allocate a copy buffer per request.

Benchmarks cover the hot paths — selection, health lookups, rate limiting, the retry budget and
forwarding:

```bash
go test -run '^$' -bench . -benchmem ./...
```

CI runs them on every push and compares them with the code before the push; the comparison is in
the run's summary on GitHub.

## Configuration

Two configurations ship with the project, both documenting the full schema:

- `configs/lb.example.yaml` — backends addressed by their compose service names, for the balancer
  running in Docker. The demo console edits this one.
- `configs/lb.localhost.yaml` — the same, with the backends on their published ports, for the
  balancer running on your machine.

Copy either to `configs/lb.yaml` for changes of your own; that path is ignored by git.

A reload on SIGHUP applies backends, weights, the algorithm, the failure policy, health checking,
the retry budget and the limits straight away. Settings bound to a socket — `listen_addr`,
`metrics_addr`, `max_connections`, the timeouts and `enable_pprof` — need a restart; a reload
applies everything else and logs which settings it left alone. An invalid file is refused in full,
and `/status` reports how many reloads have been applied.

Under `retry_next_backend`, `retry.max_retries` bounds the retries of one request and the retry
budget bounds them across all requests: at most `retry.budget_percent` (default 20) of the requests
in flight may be retries, and `retry.min_retry_concurrency` (default 3) are always allowed, so
light traffic can still be retried. A retry the budget refuses is answered with 503 and counted as
`retry_budget_exhausted`.

A request that is not idempotent — POST, PATCH, or a method the balancer does not know — is retried
only while nothing can have acted on it, which means the connection to the backend was never made.
Once the request is on the wire the backend may have carried it out and answered into a connection
that then broke, so sending it again could place a second order; the client receives 503 instead,
counted as `not_retryable`. Idempotent requests are retried after any failure.

The numbers in both files are starting points; the [performance report](docs/performance-report.md)
records what the load test says about them.

## Observability

The balancer serves two ports: traffic on `listen_addr`, and observability on `metrics_addr`.
Keeping them apart means these endpoints stay reachable when the traffic port is saturated, and
none of their paths is taken away from the backends.

- `/metrics` — Prometheus format: requests by backend and status, a latency histogram, failed
  attempts, retries, refusals by reason, reloads, and live gauges for requests in flight and health
- `/status` — a JSON summary for a person: algorithm, healthy count, reloads applied, and each
  backend's weight, health and requests in flight
- `/healthz` — liveness: 200 for as long as the process answers. Backends being down does not
  change it, because restarting the balancer would bring none of them back
- `/readyz` — readiness: 200 while at least one backend is healthy, 503 when none is and from the
  moment a shutdown begins, so that whatever routes traffic here stops while requests in flight
  finish. The metrics port stays up until they have
- `/debug/pprof/` — Go's profiling endpoints, served only when `enable_pprof` is set, because they
  expose heap and goroutine state

Every request is given an identifier, returned to the client and sent on to the backend as
`X-Request-Id`, and every log line about that request carries it as `request_id`. A client that
already sends the header keeps its own value, as long as it is printable ASCII of at most 64
characters; anything else is replaced. The identifier is assigned before rate limiting and
validation, so a refused request can be traced too:

```bash
curl -si localhost:8080 | grep -i x-request-id
```

The environment includes Prometheus on [localhost:9090](http://localhost:9090), scraping the
balancer every five seconds, and Grafana on [localhost:3000](http://localhost:3000) with three
linked dashboards in an *Ege-Balancer* folder:

| Dashboard | What it shows |
| --- | --- |
| [Overview](http://localhost:3000/d/ege-balancer) | requests, error-free share, p99, healthy backends and reloads at a glance; request rate and share of traffic per backend; latency; a health timeline |
| [Backends](http://localhost:3000/d/ege-balancer-backends) | a sortable table per backend, share of traffic, requests in flight averaged over a window, p95 latency per backend, a latency heatmap |
| [Resilience](http://localhost:3000/d/ege-balancer-resilience) | answers by status class, refusals by reason, retries against the budget, failed attempts, health and reloads |

Every graph marks configuration reloads, so the moment an algorithm changes is visible. Each
backend keeps its profile's colour, and a window selector trades detail for smoothness. The
dashboards are generated by `deploy/grafana/generate_dashboards.py`; edit the script and run it,
rather than editing the JSON.

## Performance

Measured on a ten core machine that was also running the load generator and all ten backends,
after the three bottlenecks the profiling found:

| Concurrent connections | Throughput | p50 | p95 | p99 |
| --- | --- | --- | --- | --- |
| 100 | 40,616 req/s | 2.1 ms | 5.3 ms | 7.7 ms |
| 1,000 | 41,208 req/s | 23.6 ms | 46.7 ms | 60.2 ms |
| 2,000 | 41,421 req/s | 47.3 ms | 87.5 ms | 106.6 ms |

No request failed at any level. The [performance report](docs/performance-report.md) has the
method, the bottlenecks and the numbers before and after each fix.

## Project layout

```
cmd/lb/                    entry point: reads configuration, builds the logger, runs the app
cmd/demo/                  terminal console for driving the demo environment
cmd/mockbackend/           mock backend with profiles: latency, capacity, answer size, errors
internal/app/              wiring, shared by the binary and the integration tests
internal/config/           configuration parsing, defaults, validation and reload rules
internal/balancer/         Backend, the LBStrategy interface and the three algorithms
internal/health/           active and passive health checking
internal/proxy/            forwarding, failure policies, retry budget, rate limiting, validation
internal/server/           listeners, connection limit and graceful shutdown
internal/observability/    structured logging, Prometheus metrics, status and pprof endpoints
internal/integration/      end-to-end tests over real sockets
configs/                   example configurations
deploy/                    compose environment, mock backend image, Prometheus and Grafana
docs/                      design paper, development log, performance report and more
```

Modules talk to each other through interfaces — `balancer.LBStrategy`, `health.Checker` — so an
implementation can be replaced without touching the packages that use it.

## Documentation

- [Technical design](docs/technical-design/) — the design as a paper, in English and Turkish:
  architecture, algorithms, failure handling and the evaluation. The original design the project
  was built to and its revision notes are kept beside it.
- [Development log](docs/development-log/) — one page per day of the plan, and one for the work
  after the release: what was built, which decisions were taken and why, what went wrong, and how
  the result was verified.
- [Performance report](docs/performance-report.md) — the load testing method, the bottlenecks
  profiling exposed, throughput and latency before and after each fix, and the benchmarks.
- [Design deviations](docs/design-deviations.md) — every place the implementation departs from
  the original design, with the reasoning.
- [Deployment checklist](docs/deployment-checklist.md) — the production readiness criteria and
  their evidence, and what to check before a release and before real traffic.

## Development

Work goes straight to `main`, and CI runs on every push: gofmt, `go vet`, golangci-lint,
govulncheck, the build, the full test suite under `-race`, and both container images. A tag
`vX.Y.Z` runs the release workflow, which tests again, pushes the image to the GitHub container
registry and publishes the release. Commit messages follow
[Conventional Commits](https://www.conventionalcommits.org/).

## License

[MIT](LICENSE)
