# Day 6 — Health checker

**Planned deliverable:** unhealthy backends taken out of the pool automatically and added back
when they recover.

## What was built

- `internal/health` with the `Checker` interface and an HTTP implementation.
- Active checking: one goroutine per backend probing the configured path on the configured
  interval, stopping when the context is cancelled.
- Passive checking: the proxy reports the outcome of every request it forwards.
- The proxy now offers the strategy only the healthy backends.
- 97% coverage in the health package, 100% in the proxy package.

## Decisions

**The `Checker` interface gained `ReportSuccess` and `ReportFailure`.** The example interface in
section 6.2 has only `Start` and `IsHealthy`, which is enough for active checking but leaves no
way for the proxy to feed passive checking. Both paths now increment the same consecutive
counters, so a backend that fails real traffic is removed without waiting for the next probe.

*This is a deviation from the design document and the document should be updated to match.*

**Backends start healthy.** Starting them as unknown and unhealthy would reject all traffic for
up to one probe interval after startup. Optimism costs at most a few failed requests, handled by
the passive path; pessimism costs a guaranteed outage window on every restart.

**Any success resets the failure streak and any failure resets the success streak,** so only an
unbroken run crosses a threshold. A backend flapping between success and failure stays in
whatever state it was in rather than oscillating.

**`sync.RWMutex` rather than atomics.** Health is read on every request and written only on a
state change, which is exactly the read-heavy pattern section 5.4 names.

**An unknown address is treated as healthy,** so a pool without an active checker still serves.
That is what lets the proxy tests use a real checker with no probes running instead of a
purpose-built fake.

## What went wrong

Nothing failed, but two design points only became visible while wiring it up:

- The strategies had to keep working on a shrinking pool. That had been anticipated on day 5 for
  weighted round robin, and the test written then paid off here.
- Filtering allocates a slice per request. Acceptable for now; day 10's profiling will show
  whether it matters.

## Verification

Against live backends: one backend was killed, and after three failed probes the log recorded
`taken out of the pool`. Eighteen requests then went through with **zero** 503 responses and the
dead backend was never selected. Restarting it produced `is healthy again after 2 successful
checks`, and the next twenty requests included it in an even split again.
