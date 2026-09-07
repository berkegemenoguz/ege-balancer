# Ege-Balancer

A modular, high-performance HTTP reverse proxy and load balancer written in Go.

Incoming HTTP traffic is distributed across multiple backends using a configurable
strategy, unhealthy backends are taken out of the pool automatically, and the whole
system is observable through structured logs and Prometheus metrics.

> Status: day 6 of the 12-day plan — unhealthy backends leave the pool automatically and
> rejoin once they recover. Retry, circuit breaking and rate limiting are not implemented yet.

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

## License

[MIT](LICENSE)
