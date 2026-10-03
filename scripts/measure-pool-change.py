#!/usr/bin/env python3
"""What happens to the sessions when the pool changes under them.

    scripts/measure-pool-change.py                         # both events, both configurations
    scripts/measure-pool-change.py --repeats 3
    scripts/measure-pool-change.py --event join consistent_hash

Two events, each part way through a run on the memory pool: a backend joins
the pool by a reload (join), or a backend is killed (kill). Two configurations
by default, the two that keep a client on one backend: consistent hashing and
sticky sessions. For every run the script reads each mock backend's cache
counters at four moments — fifteen seconds before the event, at it, three
seconds after and twenty-five seconds after — and writes the hit rate of each
window, the share of requests the new backend took, and the generator's own
result to measurements/change-<event>-<configuration>-<repeat>.json.
scripts/summarise.py --pool-change turns them into the report's table.

It brings the stack up itself with the memory and affinity overlays, and
changes nothing in the repository. Only the standard library is used.
"""

import argparse
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request

COMPOSE = ["docker", "compose", "-f", "deploy/docker-compose.yml",
           "-f", "deploy/docker-compose.memory.yml", "-f", "deploy/docker-compose.affinity.yml"]
STATUS = "http://127.0.0.1:8081/status"
TARGET = "http://127.0.0.1:8080"
BACKENDS = [f"backend-{n}" for n in range(1, 11)]
# A fast backend joins, and another fast one dies, so that neither event is
# about the slow or the large backend.
JOINER, VICTIM = "backend-6", "backend-4"


def log(*parts):
    print(time.strftime("%H:%M:%S"), *parts, flush=True)


def get(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        return response.read().decode()


def status():
    return json.loads(get(STATUS))


def compose(*args):
    subprocess.run(COMPOSE + list(args), check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def apply(configuration, without=()):
    """Writes the configuration and waits until the balancer has reloaded it."""
    before = status()["reloads"]
    command = [sys.executable, "scripts/configure.py", configuration]
    for backend in without:
        command += ["--without", backend]
    subprocess.run(command, check=True, stdout=subprocess.DEVNULL)
    compose("kill", "-s", "HUP", "loadbalancer")
    wait_for(lambda: status()["reloads"] > before, f"the balancer to apply {configuration}")


def wait_for(condition, what, seconds=60):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            if condition():
                return
        except OSError:
            pass
        time.sleep(0.5)
    raise SystemExit(f"timed out waiting for {what}")


def admin(backend):
    return f"http://127.0.0.1:{5780 + int(backend.split('-')[1])}"


def clear_caches():
    for backend in BACKENDS:
        urllib.request.urlopen(urllib.request.Request(admin(backend) + "/cache/clear", method="POST"), timeout=5)


def cache_counts():
    """Each backend's cache hits and misses so far; a backend that cannot be
    reached, such as one just killed, is left out."""
    counts = {}
    for backend in BACKENDS:
        try:
            text = get(admin(backend) + "/metrics")
        except OSError:
            continue
        hits = misses = 0.0
        for line in text.splitlines():
            if line.startswith('mock_cache_requests_total{result="hit"}'):
                hits = float(line.split()[-1])
            elif line.startswith('mock_cache_requests_total{result="miss"}'):
                misses = float(line.split()[-1])
        counts[backend] = (hits, misses)
    return counts


def window(before, after, only=None):
    """The requests and the hit rate between two readings, over the backends
    read both times."""
    names = [b for b in after if b in before and (only is None or b in only)]
    hits = sum(after[b][0] - before[b][0] for b in names)
    misses = sum(after[b][1] - before[b][1] for b in names)
    total = hits + misses
    return {"requests": int(total), "hit_rate": hits / total if total else None}


def one_run(loadgen, event, configuration, repeat, warmup, keys, out):
    """One run: warm up, read the caches, change the pool, read them again."""
    apply(configuration, without=[JOINER] if event == "join" else [])
    clear_caches()
    expected = 9 if event == "join" else 10
    wait_for(lambda: status()["healthy_backends"] == expected, f"{expected} healthy backends")

    label = f"{event}-{configuration}"
    path = os.path.join(out, f"change-{event}-{configuration}-{repeat}.generator.json")
    generator = subprocess.Popen(
        [loadgen, "-addr", TARGET, "-connections", "50", "-warmup", f"{warmup}s", "-duration", "50s",
         "-keys", str(keys), "-cookies", "-label", label, "-json", path],
        stdout=subprocess.DEVNULL)

    started = time.time()
    time.sleep(warmup + 5)
    before = cache_counts()
    time.sleep(15)
    at = cache_counts()
    if event == "join":
        apply(configuration)
    else:
        compose("kill", VICTIM)
    log(label, repeat, f"{event} at {time.strftime('%H:%M:%S')}, {time.time() - started:.0f} s into the run")
    time.sleep(3)
    soon = cache_counts()
    time.sleep(22)
    later = cache_counts()
    generator.wait()

    record = {
        "event": event,
        "configuration": configuration,
        "repeat": repeat,
        "before": window(before, at),
        "first_3s": window(at, soon),
        "next_22s": window(soon, later),
        "generator": json.load(open(path)),
    }
    if event == "join":
        joined = window(soon, later, only={JOINER})
        record["joiner_share"] = joined["requests"] / max(record["next_22s"]["requests"], 1)
        record["joiner_hit_rate"] = joined["hit_rate"]
    else:
        survivors = set(BACKENDS) - {VICTIM}
        record["survivors_first_3s"] = window(at, soon, only=survivors)
        record["survivors_next_22s"] = window(soon, later, only=survivors)
        compose("start", VICTIM)
        wait_for(lambda: status()["healthy_backends"] == 10, "the killed backend to return")

    with open(os.path.join(out, f"change-{event}-{configuration}-{repeat}.json"), "w") as saved:
        json.dump(record, saved, indent=2)
    log(label, repeat, "hit rate before", record["before"]["hit_rate"], "first 3 s", record["first_3s"]["hit_rate"],
        "next 22 s", record["next_22s"]["hit_rate"])


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("configurations", nargs="*", default=["consistent_hash", "sticky_least_connections"])
    parser.add_argument("--event", action="append", choices=["join", "kill"])
    parser.add_argument("--repeats", type=int, default=1)
    parser.add_argument("--warmup", type=int, default=60, help="seconds before the first reading")
    parser.add_argument("--keys", type=int, default=30000)
    parser.add_argument("--cooldown", type=int, default=8)
    args = parser.parse_args()
    events = args.event or ["join", "kill"]

    os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
    os.makedirs("measurements", exist_ok=True)
    loadgen = os.path.join(tempfile.mkdtemp(), "loadgen")
    # Built, not run with go run, so that every result records its revision.
    subprocess.run(["go", "build", "-o", loadgen, "./cmd/loadgen"], check=True)

    subprocess.run([sys.executable, "scripts/configure.py", args.configurations[0]], check=True, stdout=subprocess.DEVNULL)
    compose("up", "-d", "--build")
    wait_for(lambda: get(STATUS) and True, "the balancer")
    if json.loads(get(admin("backend-1") + "/faults"))["cache_size"] == 0:
        raise SystemExit("the mock backends remember nothing: the memory overlay is not in force")

    for repeat in range(1, args.repeats + 1):
        shift = (repeat - 1) % len(args.configurations)
        rotated = args.configurations[shift:] + args.configurations[:shift]
        for event in events:
            for configuration in rotated:
                one_run(loadgen, event, configuration, repeat, args.warmup, args.keys, "measurements")
                time.sleep(args.cooldown)


if __name__ == "__main__":
    sys.exit(main())
