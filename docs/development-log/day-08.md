# Day 8 — Logging and metrics

**Planned deliverable:** an observable system — structured logs, a Prometheus compatible
`/metrics` endpoint, `/status`, and a Prometheus plus Grafana stack.

## What was built

- `internal/observability`: a `log/slog` logger honouring the configured level and format, a
  private Prometheus registry, a collector reading the live pool, and the `/metrics` and
  `/status` handlers.
- Metrics: requests by backend and status, a latency histogram, backend failures, and rejected
  requests by reason; plus gauges for active connections and backend health.
- Every `log.Printf` across the proxy, server and health packages replaced with structured
  `slog` calls carrying named fields.
- A second HTTP server for the observability endpoints, and Prometheus and Grafana added to the
  compose environment with a provisioned datasource and dashboard.
- Coverage: 98.2% in the new package; 91.5% in config after the new field.

## Decisions

**Observability listens on its own port**, added to the schema as `metrics_addr` with a default
of `:8081`. Serving `/metrics` on the traffic port would mean clients could never proxy those
two paths to a backend, and would expose internal state to anyone who can reach the balancer.
The observability server also carries no connection limit: it has to keep answering exactly when
the traffic port is saturated.

*This adds a field to the schema in section 3.3 and the design document should be updated.*

**Active connections and health are read at scrape time,** through a custom
`prometheus.Collector` over the pool, rather than mirrored into gauges on every change. There is
no second copy of the state to keep in step, and no way for the reported value to drift from
what the balancer is actually doing. A test asserts the gauge follows a release.

**Logging goes through `slog.SetDefault` rather than a logger passed to every constructor.**
Logging is cross-cutting; threading a logger through five packages would add a parameter to
every signature for no gain in testability here.

**The histogram buckets start at one millisecond and end at ten seconds,** which spans a local
backend answering in microseconds up to the range where the configured timeouts take over.

**Prometheus scrapes `host.docker.internal:8081`.** The balancer runs on the host during
development while the backends run in containers. Scraping the host avoids pulling day 12's
Dockerfile forward just to make the monitoring stack work. `extra_hosts: host-gateway` keeps
that working on Linux as well as Docker Desktop.

## What went wrong

**The first version of the two-server startup could hang.** Both servers ran until the shared
context was cancelled, so if the traffic server had stopped on its own — a listener error, say —
the process would have sat there with only the metrics port alive. Fixed by deriving a
cancellable context and cancelling it as soon as any server returns.

**Grafana refused to start after the datasource was changed.** The dashboard JSON referenced
the datasource by the identifier Grafana happens to generate for a provisioned Prometheus
source. That worked by luck, so the identifier was pinned explicitly — at which point Grafana
would not start at all: its database still held the datasource under the old identifier, and
provisioning failed with `data source not found`, taking every dependent module down with it.
Adding a `deleteDatasources` entry makes provisioning drop the previous registration first, so
changing the datasource no longer bricks a container that has already run once.

**Grafana was killed the moment the dashboard was opened.** The container exited with code 137
and `OOMKilled: true`. Section 7.6 of the design document proposes a 256 MB limit for both
monitoring containers; Grafana 13 idles inside that but exceeds it while rendering a dashboard.
Measured under load it sits at 331 MiB, so its limit was raised to 512 MB. Prometheus stays at
256 MB and uses 143 MiB with ten backends being scraped.

*This is a deviation from the design document and the document should be updated.*

**Wrapping the response writer had hidden a capability.** The per-attempt wrapper from day 7
embeds `http.ResponseWriter`, which stops `http.ResponseController` from finding the real
connection underneath, so flushing and deadline control would not have reached it. Adding an
`Unwrap` method restores that. This was noticed while adding status recording to the same
wrapper, not by a failing test — worth remembering that wrapping a response writer silently
drops whatever the inner type could do.

## Verification

The structured log is JSON with named fields:

```json
{"time":"…","level":"WARN","msg":"backend taken out of the pool","backend":"127.0.0.1:5684","failed_checks":3}
```

With one of ten backends killed, `/status` reported `"healthy_backends": 9` and `/metrics`
showed `lb_backend_healthy{backend="127.0.0.1:5684"} 0` alongside its failure count, while the
other nine each carried five requests in the duration histogram.

The monitoring stack was then run end to end. Prometheus reported the balancer target as `up`,
and with steady traffic the dashboard's own queries returned:

| Query | Value |
| --- | --- |
| `sum(rate(lb_requests_total[2m]))` | 2.78 requests per second |
| p50 / p95 / p99 latency | 2.3 ms / 4.7 ms / 4.9 ms |
| per-backend request rate | 0.279 for each of the ten, identical |
| `sum(lb_backend_healthy)` | 10 |

Grafana provisioned both the datasource and the dashboard, all five panels resolved to the
pinned datasource, and a query issued through Grafana's own datasource proxy returned live data
from the balancer.

The memory limits were then checked under the same load: Grafana 331 MiB of 512 MB, Prometheus
143 MiB of 256 MB, neither restarting.

Pulling the two images took roughly twenty minutes on the day, and the first attempt died with
`unexpected EOF` partway through and had to be restarted. Worth knowing before demonstrating
the stack on an unprepared machine.
