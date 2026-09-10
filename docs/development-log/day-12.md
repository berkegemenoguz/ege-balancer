# Day 12 — Production readiness and release

**Planned deliverable:** v1.0.0 — a multi-stage, non-root, minimal image, a CI/CD pipeline, a
deployment checklist, and a final review against the production readiness criteria.

## What was built

- A `Dockerfile`: the binary is built in a full Go toolchain image and copied into
  `distroless/static:nonroot`, which carries no shell and no package manager.
- The balancer as a compose service, so the whole environment now comes up together.
- A release workflow that runs on a version tag: test, build the image, push it to the container
  registry as the tag and as `latest`, and publish the release notes.
- An image build step in the ordinary CI pipeline.
- `-version`, stamped in at build time and logged at startup.
- A [deployment checklist](../deployment-checklist.md), including the design document's
  production readiness criteria with the evidence for each.

## Decisions

**Distroless rather than alpine.** Section 6.3 allows either. Distroless has no shell, no package
manager and no user database beyond the `nonroot` account, so a process that does get compromised
has very little to work with. The cost is that debugging inside the container is not possible and
a container healthcheck cannot be defined — there is nothing to run one with. `/status` on the
metrics port answers that need from outside, which is where a health check for a load balancer
belongs anyway.

**The image was added to compose only now.** It was deliberately left out on day 1, when the
proxy could not forward anything, and again on day 8, when Prometheus was pointed at the host
instead. Adding it on the day the image exists keeps every intermediate state honest: the compose
file has never described something that did not work.

**Prometheus scrapes both deployments.** The balancer can run in compose or on the host during
development, and the scrape configuration lists both, labelled. Whichever is not running shows up
as a target that is down, which is clearer than editing the configuration to switch between them.

**The release workflow re-runs the tests before it builds.** The tag has to be built from a tree
that passes, not merely from one that passed when it was pushed to `main`.

**The version is stamped through `-ldflags`, not a constant in the source.** A constant has to be
edited and committed before every tag and is wrong the moment someone forgets; the build argument
comes from the tag itself.

## What went wrong

**Prometheus was counting everything twice.** Scraping both the compose service and
`host.docker.internal` looked like a convenience — whichever deployment is not running simply
shows as down. But the compose service publishes the metrics port on the host, so both addresses
reach the same process: `sum(lb_backend_healthy)` reported 20 for ten backends. Every aggregate
on the dashboard would have been double. The scrape configuration now lists the compose service
only, with a comment on how to point it at a host binary instead.

Confirming the fix took a second attempt. After the change the query returned 30 rather than 10,
because the old series were still inside Prometheus's five minute lookback — and recreating the
container did not clear them, since `prom/prometheus` declares a volume that compose reuses. With
`--renew-anon-volumes` the count was exactly ten. The lesson is about the tool, not the balancer:
a metric that looks wrong after a scrape change may be historical data rather than a live fault.

Otherwise nothing broke. The day was assembling parts whose behaviour was already established,
and the checklist review found no criterion unmet except the one decided against deliberately on
day 2: the branch and pull request flow, recorded as
[deviation 1](../design-deviations.md).

## Verification

The image:

| Check | Result |
| --- | --- |
| Size | 22.6 MB |
| User | `nonroot:nonroot` |
| Shell present | no — `docker run --entrypoint /bin/sh` fails, there is no shell to run |
| Version stamping | `docker run --rm ege-balancer:v1.0.0 -version` prints `ege-balancer v1.0.0` |

The stack, with the balancer running as a container alongside the backends:

| Check | Result |
| --- | --- |
| Traffic through the container | 20 requests, 2 to each of the ten backends |
| `/status` | `round_robin`, 10 of 10 healthy |
| Prometheus | the compose target `up`, ten health series, `sum(lb_backend_healthy)` exactly 10 |
| Grafana | the dashboard's query returns live data through its own datasource |
| Graceful shutdown | `docker compose stop` logs the drain on both servers before exiting |
| Memory | balancer 18 MiB of its 128 MB limit; Prometheus 42 MiB of 256 MB; Grafana 299 MiB of the 512 MB it had at the time |

Grafana's limit was revisited afterwards: 512 MB was not enough once the dashboard refreshed
every five seconds, and the fix was a memory budget rather than a higher ceiling. See
[design deviations](../design-deviations.md).
