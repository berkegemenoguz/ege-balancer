# Demo console

A local console for driving the demo: it brings the stack up, then offers the actions one would
otherwise type by hand — sustained traffic, measuring the distribution, stopping and starting
backends, switching the algorithm, demonstrating the rate limit, reading the status, and putting
everything back.

The prompt names the algorithm in force and whether traffic is flowing, so a change made several
actions ago cannot quietly make the next measurement look wrong:

```
[weighted_round_robin · traffic on] action?
```

**This is a development tool.** It shells out to `docker` and writes to the configuration file,
so it can stop containers and change how the balancer behaves. It never listens on a socket and
every action it can take is fixed in the code, but it has no place on a server: run it on your
own machine, against your own stack, and nowhere else. It is not part of the container image —
the `Dockerfile` builds `./cmd/lb` alone.

## Running it

From the repository root:

```bash
go run ./cmd/demo
```

In VS Code, opening `cmd/demo/main.go` and using the **run** link above `func main` does the same
thing.

It needs Docker running and the repository's compose file; it starts the stack itself, so there
is nothing to prepare. Flags exist for every path and address it uses (`-compose`, `-config`,
`-traffic`, `-status`, `-service`) if you run it from somewhere else or against a balancer on a
different port.

## What it prints

Every action shows the command it runs before running it:

```
  $ docker compose -f deploy/docker-compose.yml kill -s HUP loadbalancer
```

That is deliberate. The console is a shortcut for typing, not a layer that hides what happens:
whoever is watching sees the real command, and if the console misbehaves the same thing can be
done by hand.

## What it changes, and how to undo it

The algorithm and rate limit actions **edit `configs/lb.example.yaml`** and send SIGHUP, because
that is how the balancer reloads. The console snapshots the file when it starts, `r` restores it
at any time, and quitting restores it without asking — leaving demo settings in the repository is
worse than a moment spent putting them back.

The rate limit action restores the previous limit itself: it lowers the limit, drives a burst
through it, reports what the balancer refused and what the backends were therefore spared, waits
for the bucket to refill, and puts the limit back.

Stopping a backend stops its compose service. `r` starts every backend again.

If something is interrupted before the console can tidy up, one command puts the configuration
back:

```bash
git checkout configs/lb.example.yaml
```
