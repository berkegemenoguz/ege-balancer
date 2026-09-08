# Day 3 — Listener and proxy core

**Planned deliverable:** a minimal proxy that can forward to a single backend, with graceful
shutdown.

## What was built

- `internal/server`: a `net.Listener` bound at construction, an `http.Server` carrying the
  configured read, write and idle timeouts, and `Run(ctx)` that drains on cancellation.
- `internal/proxy`: an `httputil.ReverseProxy` forwarding to one backend, with the configured
  connect timeout applied to every dial.
- Signal handling in `cmd/lb`, so SIGINT and SIGTERM start the drain.

## Decisions

**The socket is opened in `New`, not in `Run`.** That way `Addr()` can be read before serving
starts, which lets tests listen on port 0 and ask the operating system for a free port. Tests
that hard-code a port collide with whatever else is running on the machine.

**`Rewrite` instead of the older `Director` hook.** `Rewrite` receives both the inbound and the
outbound request, and its `SetXForwarded` helper overwrites `X-Forwarded-For` with the real
client address rather than appending to whatever the client sent. Appending would let a client
forge the address the backend sees.

**Failure answers `503` with `Retry-After: 5`,** as described in section 5.5, rather than a bare
connection error.

**A 30 second shutdown grace period.** Long enough for a normal request to finish, short enough
that a stuck backend cannot hold the process open indefinitely.

## What went wrong

**A mutex was written and then deleted.** `Server` initially guarded shutdown with a
`sync.Mutex`, but shutdown is only ever reached from one place in `Run`. Removed the same day
rather than left as harmless-looking extra state.

**A substring search was hand-written in a test** where `strings.Contains` exists. Replaced.
Twice more in later days the same reflex appeared, which is worth naming: reach for the standard
library before writing a helper.

**golangci-lint reported three errcheck findings** on `defer response.Body.Close()` in tests.

## Verification

Docker Desktop was not running, so ten local HTTP servers stood in for the mock backends.
Forwarding returned 200 with the backend's body; an unreachable backend produced 503 with
`Retry-After`; SIGTERM logged the drain and exited cleanly.
