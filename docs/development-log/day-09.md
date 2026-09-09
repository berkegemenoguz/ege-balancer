# Day 9 — Integration tests

**Planned deliverable:** a green end-to-end suite over ten mock backends, verifying the three
algorithms with real traffic and covering the health check scenarios.

## What was built

- `internal/app`, holding the wiring that used to live in `main`.
- `internal/integration`: twelve tests that start the assembled balancer on real sockets and
  drive it over HTTP, with mock backends that can be slowed down, made to fail, or killed while
  the test runs.
- Coverage: the integration suite alone exercises 63.3% of the statements under `internal/`,
  measured with `-coverpkg`.

## Decisions

**The wiring moved out of `main` into `internal/app`.** Tests cannot call `main`, so an
integration test would otherwise assemble its own copy of the chain — and then verify a wiring
the binary does not use. Now `main` reads configuration, builds the logger and calls `app.New`,
and the tests exercise exactly the assembly that ships.

**The suite runs in the normal `go test ./...`, with no Docker and no build tag.** The mock
backends are `httptest` servers, so the whole suite finishes in about two seconds and runs on
every push in CI. Requiring Docker would have made the most valuable tests the ones nobody runs.

**Backends count client requests and health probes separately.** That distinction is what lets a
test assert "no client traffic reached this backend" while the health checker is still polling
it every ten milliseconds.

**Failure is simulated in two different ways, because they are two different failures.** A
backend answering 500 is reachable but unwell — the client should see its error when the policy
says so. A closed listener refuses connections — the transport fails before any answer exists,
which is what retry and the circuit breaker react to.

**Health thresholds are raised to an unreachable value in the retry and fail-fast tests.**
Otherwise the health checker removes the broken backend within milliseconds and the test would
pass without the retry logic ever running. Making one mechanism inert is the only way to test
the other one honestly.

## What went wrong

**Shutdown took 5.3 seconds in the concurrency test and tripped the harness timeout.** The
number matched the configured read timeout exactly, which gave it away: sending sixty requests
at once makes Go's transport open more connections than it uses, and the spare ones are accepted
without ever sending a request. `http.Server.Shutdown` closes idle connections but treats those
as active, so they only fall away when their read timeout expires. Not a defect — the wait is
bounded and well inside the thirty second grace period — but the test configuration now uses
short timeouts so the suite does not sit through it.

**The rate limit test failed on a count of seven against a limit of five.** The mock backend was
counting health probes as client traffic. The fix was in the harness, not the balancer, and it
made every other assertion sharper as well.

**A hand-written number formatter shadowed `strconv`.** Deleted in favour of `strconv.Itoa` —
the third time this session that a standard library function was reimplemented by reflex.

## Verification

Twelve tests, stable across three consecutive `-race` runs:

| Area | What is asserted |
| --- | --- |
| Round robin | 100 requests over 10 backends, exactly 10 each, all 200 |
| Weighted round robin | weights 1:2:3 give exactly 20, 40 and 60 of 120 requests |
| Least connections | a backend slowed to 40 ms receives fewer requests than the fast ones |
| Health | a failing backend leaves the pool with no client seeing an error, then rejoins |
| Retry | one dead backend, every one of 30 requests still answered 200 |
| Fail fast | the same setup surfaces 503s, while healthy backends keep serving |
| Total outage | 503 with `Retry-After` when every backend is unhealthy |
| Rate limiting | the burst gets through, the rest are refused before reaching a backend |
| Proxying | method, path, query, body, request and response headers, and status all survive |
| Forwarded headers | a client's forged `X-Forwarded-For` is replaced with its real address |
| Observability | `/metrics` and `/status` report the traffic that was actually sent |
| Shutdown | a request in flight when the signal arrives is answered in full |
