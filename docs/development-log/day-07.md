# Day 7 — Proxy core completion

**Planned deliverable:** a proxy core close to production: header handling, rate limiting,
timeout management and configurable failure policies.

## What was built

- The three failure policies from section 5.5, driven by `failure_policy`.
- A retry loop that never offers a request to the same backend twice.
- `retry_on_5xx`, turning a 5xx answer into a failed attempt when enabled.
- A circuit breaker with an open period and a single half-open probe.
- Per-IP token bucket rate limiting answering `429`.
- Request framing validation answering `400`.
- A body size limit answering `413`, and a connection limit enforced at the listener.
- Coverage: 95.5% in the proxy package, 90.2% in the server package.

## Decisions

**Retry needed the response to stay untouched until an attempt succeeds.** Each attempt runs
through a thin `http.ResponseWriter` wrapper; the reverse proxy's error handler writes nothing
and only records the error, so the response is still free for the next backend. Without that,
the first failing attempt would have written a 503 that the second attempt could not take back.

**A 5xx answer is passed through by default.** The backend's own error message reaching the
client is more useful than the proxy replacing it, and retrying a 500 can duplicate work the
backend already did. `retry_on_5xx: true` is there for deployments that prefer the opposite.

**The request body is buffered only when retries are possible.** A retry has to send the body
again, and a body already read cannot be replayed. When the policy allows one attempt, the body
streams straight through with no buffering. The buffer is bounded by the configured body limit,
so it cannot be used to exhaust memory.

**Policy to attempt count:** `retry_next_backend` gets `1 + max_retries` attempts; `fail_fast`
and `circuit_breaker` get one. The circuit breaker's value is not retrying a single request but
keeping a failing backend out of later selections.

**The client address for rate limiting comes from `RemoteAddr`,** never from a forwarding
header. The proxy overwrites those headers precisely because clients can set them.

**Idle rate limit buckets are swept when the map grows past a threshold,** rather than by a
background goroutine that would need its own lifecycle.

**The connection limit is enforced at the listener,** by handing out a fixed number of slots.
Rejecting inside the handler would be too late: the connection, its buffers and its file
descriptor already exist by then.

## What went wrong

**staticcheck rejected a deliberately repeated call** (SA4000): the rate limit test asserted
`!allow(x) || !allow(x)`, two calls that look identical but each consume a token. The linter was
right that it reads as a mistake; replaced with a loop that spends a stated number of tokens.

**The circuit breaker did not trip during the first live test.** With the default configuration
the passive health threshold is three consecutive failures and the breaker threshold is five, so
the health checker removes a backend before the breaker ever reaches its count. Not a bug — the
faster mechanism wins — but it means the two thresholds should be chosen together. The breaker
was verified separately with the health threshold raised.

## Verification

Two of ten backends were killed, and the same load was run under each policy:

| Policy | Result over 20 requests |
| --- | --- |
| `retry_next_backend` | 20 × 200, no 503 at all |
| `fail_fast` | 4 × 503 |

Rate limiting at five per second answered `200 200 200 200 200 429 429 …`, then allowed traffic
again after a second of quiet.

The circuit breaker, tested with the health threshold raised so it would act first, logged
`shut out for 10s after 5 consecutive failures` for both dead backends, after which twenty
consecutive requests all returned 200.
