# Day 11 — Resilience and configuration reload

**Planned deliverable:** verified failure behaviour, configuration hot-reload on SIGHUP, and
documentation.

The documentation was brought up to date the day before, when the README was rewritten and the
[design deviations](../design-deviations.md) were collected in one place. Today was the reload
and the resilience scenarios from section 10.4.

## What was built

- Configuration reload on SIGHUP: the file is re-read, validated, and everything that can be
  changed while serving is swapped in.
- `lb_config_reloads_total{result}` and a `reloads` count in `/status`, so an operator who sent a
  signal can see whether it took effect.
- `timeouts.response_timeout`, bounding how long a backend may take to start answering.
- Six reload tests and four resilience tests, on top of the twelve integration tests already
  there.

## Decisions

**A request works from one snapshot.** The proxy keeps its settings — strategy, backend pool,
failure policy, breaker, limits — behind an `atomic.Pointer`, and a request reads it once at the
start. A reload swaps the pointer. Without that, a request could pick a backend from the new pool
and then be judged by the old retry policy, and no reader ever has to take a lock.

**Backends that survive a reload keep their objects.** The connection counters live on the
`Backend` values, so replacing a backend that is still serving would lose track of the requests
in flight and mislead least connections until they drained. Addresses that persist keep their
object, their counter and their health; only their weight is updated.

**A reload never restarts the balancer, and never stops it either.** Settings bound to a socket —
`listen_addr`, `metrics_addr`, `max_connections`, the timeouts, `enable_pprof` — cannot change
under a running server. Rather than refusing the whole reload, the balancer applies everything
else and logs exactly which settings were left alone and what they would have become.

**An invalid file changes nothing.** The configuration is validated before any of it is applied,
so a typo during an incident leaves the balancer running on what it already had, with the reason
in the log and a `rejected` counter.

**The reload trigger is a channel, not a signal handler buried in the app.** `main` turns SIGHUP
into reload requests; the app takes a plain channel. That keeps the signal handling in one place
and lets the integration tests drive reloads directly, without sending signals to the test
process.

## What went wrong

**Two reload tests failed on timing, and the fix was a feature.** The tests asked for a reload
and immediately sent traffic, but the reload is applied on the app's own goroutine, so the first
requests were still being served under the old configuration. Rather than sleeping in the tests,
the reload count went into `/status` and the metrics: the harness now waits until the balancer
says the new configuration is live. An operator wanting the same confirmation after a SIGHUP now
has it too.

**A resilience test named a configuration field that did not exist.** The scenario is section
10.4's slow backend: the test set a response timeout, and there was none. The timeouts in the
design document bound the client conversation and the initial connect, but nothing bounded a
backend that accepts a connection and then goes quiet. Such a request would occupy the balancer
until the client-side write timeout killed it, with no retry — the failure policy never got a
chance. `response_timeout` was added, defaulting to the read timeout.

**A field name collided during the refactor.** `App.metrics` already meant the observability
server; adding the metrics registry under the same name broke the build immediately. The server
field is now `admin`, which is what it always was.

## Verification

Ten new tests, all green under `-race`, plus the existing suite unchanged.

Reload, against the real binary and a real SIGHUP:

| Step | Result |
| --- | --- |
| Start | `round_robin`, 10 backends, `reloads: 0` |
| Edit the file, `kill -HUP` | `weighted_round_robin`, 8 backends, `reloads: 1` |
| 24 requests, weights 5:1:1:1:1:1:1:1 | 10 to the heavy backend, 2 to each of the others — the exact ratio |
| Replace the file with an invalid one, `kill -HUP` | configuration unchanged, traffic uninterrupted, error logged, `lb_config_reloads_total{result="rejected"} 1` |

Resilience, from section 10.4:

| Scenario | Result |
| --- | --- |
| Half the pool killed mid-flight | every request still answered, by the survivors |
| A backend stalling for three seconds | abandoned on the response timeout, retried, answered by the fast backend |
| Every backend failing, then recovering | pool empties, recovers, and traffic returns to all of them |
| One backend flapping up and down during traffic | no request lost to the balancer's own machinery |
