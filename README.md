# Ege-Balancer

A modular, high-performance HTTP reverse proxy and load balancer written in Go.

Incoming HTTP traffic is distributed across multiple backends using a configurable
strategy, unhealthy backends are taken out of the pool automatically, and the whole
system is observable through structured logs and Prometheus metrics.

> Status: day 10 of the 12-day plan — load tested and profiled, at about 41,000 requests per
> second with no failures up to 2,000 concurrent connections. Resilience testing, config
> hot-reload and the production image are still to come.

## Features (target for v1.0.0)

- Three load balancing algorithms, selectable from configuration: round robin,
  least connections, weighted round robin
- Active (periodic HTTP probe) and passive (consecutive failure) health checking
- Configurable failure policies: `retry_next_backend`, `fail_fast`, `circuit_breaker`
- Graceful shutdown and config hot-reload (SIGHUP)
- Structured JSON logging and a Prometheus compatible `/metrics` endpoint
- Basic security baseline: per-IP rate limiting, resource limits, header sanitisation

## Requirements

- Go 1.27 or newer
- Docker with Compose (for the mock backend environment)

## Getting started

Start the ten mock backends:

```bash
docker compose -f deploy/docker-compose.yml up -d
```

Each backend answers with its own name, so you can verify which one served a request:

```bash
curl localhost:5681
```

Build and run the load balancer:

```bash
go build -o bin/lb ./cmd/lb && ./bin/lb -config configs/lb.example.yaml
```

Run the tests:

```bash
go test -race -cover ./...
```

That includes `internal/integration`, which starts the balancer on real sockets against mock
backends and drives it over HTTP. It needs no Docker and runs in CI with everything else.

## Observability

The balancer serves two ports: proxied traffic on `listen_addr`, and observability on
`metrics_addr`. Keeping them apart means `/metrics` and `/status` stay reachable when the
traffic port is saturated, and neither path is stolen from the backends.

- `/metrics` — Prometheus format: request rate and latency histogram per backend, backend
  failures, rejected requests by reason, and live gauges for active connections and health.
- `/status` — a JSON summary of the pool for a person: algorithm, healthy count, and each
  backend's weight, health and active connections.

The compose environment includes Prometheus (`localhost:9090`) and Grafana
(`localhost:3000`, dashboard *Ege-Balancer*). Prometheus scrapes the balancer on the host at
`host.docker.internal:8081`, so the stack works while the binary runs outside Docker.

## Project layout

```
cmd/lb/                    entry point, wires the modules together
internal/config/           configuration parsing and validation
internal/balancer/         LBStrategy interface and the three algorithms
internal/health/           active and passive health checking
internal/proxy/            proxy core: forwarding, retry, circuit breaker
internal/server/           listener and connection lifecycle
internal/observability/    structured logging and Prometheus metrics
configs/                   example configuration
deploy/                    docker-compose environment
docs/                      technical design document
```

## Configuration

`configs/lb.example.yaml` documents the full schema. Copy it to `configs/lb.yaml`
for local changes — that path is gitignored.

## Documentation

The full technical design, including the architecture rationale, the 12-day plan and
the production readiness criteria, is in [docs/technical-design-v1.6.pdf](docs/technical-design-v1.6.pdf).

The [development log](docs/development-log/) keeps one page per day of that plan: what was
built, which decisions were taken and why, what went wrong, and how the result was verified.

The [performance report](docs/performance-report.md) records the load testing method, the
bottlenecks profiling exposed, and the throughput and latency before and after each fix.

## License

[MIT](LICENSE)
