# Deployment checklist

What to check before a release, and what to do when running the balancer somewhere real. The
first section follows the production readiness criteria in section 11 of the design document.

## Definition of done

| Criterion | State | Evidence |
| --- | --- | --- |
| Three algorithms selectable from configuration, verified by integration tests | met | `internal/integration/balancing_test.go` — round robin exactly even over ten backends, weights 1:2:3 exact over 120 requests, least connections moves load off a slow backend |
| Health checking active; an unhealthy backend is removed and added back | met | `internal/integration/resilience_test.go`, and the live run in [day 6](development-log/day-06.md) |
| Graceful shutdown and configuration reload on SIGHUP | met | in-flight requests finish on shutdown; SIGHUP verified against the real binary in [day 11](development-log/day-11.md) |
| Throughput and latency thresholds met, no known bottleneck left in pprof | met, with a caveat | [performance report](performance-report.md): about 41,000 req/s, p99 7.7 ms at 100 connections. The p95 target at a thousand connections is discussed in [deviation 9](design-deviations.md) |
| High unit test coverage in the critical modules, green integration suite | met | balancer 100%, observability 98.8%, health 97.9%, proxy 93.7%, config 89.7%, server 83.7%; 22 integration tests |
| Structured logging, Prometheus `/metrics` and `/status` | met | [README, Observability](../README.md#observability) |
| Multi-stage, small, non-root image; CI running lint, test and build | met | `Dockerfile` on distroless nonroot; `.github/workflows/ci.yml` and `release.yml` |
| Security measures from section 6.3 applied | met | rate limiting, connection and body limits, framing validation, forwarded-header rewriting, govulncheck in CI, non-root image |
| Protected `main`, every change through PR and CI, v1.0.0 tagged | partly | CI gates every push; the branch and pull request flow is not used, see [deviation 1](design-deviations.md) |
| README, architecture diagrams and configuration reference complete | met | README, [development log](development-log/), [design deviations](design-deviations.md), [technical design](technical-design/) |

## Before tagging a release

- [ ] `go test -race -cover ./...` passes
- [ ] `golangci-lint run ./...` reports nothing
- [ ] `govulncheck ./...` reports nothing
- [ ] `docker build .` succeeds and the image still runs `-version`
- [ ] The benchmarks run for the last push shows no change in bytes or allocations per operation
      that the release does not explain, and no timing change well beyond runner noise
- [ ] `docs/development-log/` has a page for the work in the release
- [ ] The README status line matches what the release actually does
- [ ] The version in the tag follows semantic versioning

Tagging `vX.Y.Z` runs the release workflow, which tests, builds the image, pushes it to the
container registry as `vX.Y.Z` and `latest`, and publishes the release notes.

## Before pointing real traffic at it

- [ ] `backends` lists every upstream, with weights that reflect their capacity
- [ ] `health_check.path` is an endpoint that means something on the backend — a real check, not
      a static 200
- [ ] `health_check.timeout` is shorter than `health_check.interval`, and the thresholds are set
      with the circuit breaker's in mind: the lower one acts first
- [ ] `limits.max_connections` fits the file descriptor limit of the host
- [ ] `limits.rate_limit_per_ip` reflects what a legitimate client actually sends, or is 0
- [ ] `timeouts.response_timeout` is shorter than the client-facing `write_timeout`, so a stalled
      backend is abandoned and retried rather than killing the client's request
- [ ] `failure_policy` matches the priority: `retry_next_backend` for availability, `fail_fast`
      for latency, `circuit_breaker` to protect a struggling backend
- [ ] `enable_pprof` is false, or the metrics port is unreachable from outside
- [ ] The metrics port is not exposed publicly
- [ ] Prometheus is scraping the balancer and the Grafana dashboard shows data

## After deploying

- [ ] Every backend reports `lb_backend_healthy` as 1
- [ ] `lb_requests_total` grows across all backends in the expected proportion
- [ ] `lb_rejected_requests_total` is flat; a rising `rate_limited` or `no_healthy_backend` means
      the limits or the health thresholds need revisiting
- [ ] p99 latency is within the target under real traffic
- [ ] A rolling restart of one backend causes no client-visible errors

## Rolling back

The system runs in a local and simulated environment, so no formal rollback procedure is defined
(design document section 7.5). In principle, redeploying the previous image tag is the rollback:

```bash
docker pull ghcr.io/berkegemenoguz/ege-balancer:v0.9.0
```

Configuration is rolled back by restoring the previous file and sending SIGHUP; an invalid file
is refused and leaves the running configuration untouched.
