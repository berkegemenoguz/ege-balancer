#!/usr/bin/env python3
"""Turns the runs in ./measurements into the tables the performance report quotes.

Each cell of the matrix — one algorithm at one connection count — may have been
run several times. A single run cannot separate two figures a few per cent
apart, so every number is reported as the median of its runs with the range
beside it, and the report only draws conclusions from gaps wider than that.

    scripts/summarise.py                    # the matrix and the resource table
    scripts/summarise.py --histogram 2000   # the latency distribution at one level
    scripts/summarise.py --affinity memory  # scripts/measure-affinity.sh, one pool
    scripts/summarise.py --pool-change      # scripts/measure-pool-change.py

Only the standard library is used, so it runs wherever python3 does.
"""

import argparse
import glob
import json
import os
import re
import statistics
import sys

ALGORITHMS = ["round_robin", "weighted_round_robin", "least_connections"]

# The configurations scripts/measure-affinity.sh compares, in the order the
# report lists them.
CONFIGURATIONS = ["round_robin", "least_connections", "weighted_round_robin", "consistent_hash",
                  "consistent_hash_150", "consistent_hash_125_by_capacity", "sticky_least_connections"]

# Results of the affinity scripts, which the original matrix leaves alone.
AFFINITY_PREFIXES = ("affinity-", "change-")


def to_ms(value):
    """Reads a Go duration as milliseconds."""
    match = re.fullmatch(r"([\d.]+)(ns|µs|ms|s|m)", value)
    if not match:
        raise ValueError(f"not a duration: {value}")
    size, unit = float(match.group(1)), match.group(2)
    return size * {"ns": 1e-6, "µs": 1e-3, "ms": 1, "s": 1000, "m": 60000}[unit]


def seconds_of(run):
    return to_ms(run["duration"]) / 1000


def drift():
    """How much the machine slowed down over the campaign.

    Each cell is run once per repeat, and the repeats are spread across the
    whole campaign, so comparing a cell's later runs with its first one
    measures the drift rather than the algorithm."""
    per_repeat = {}
    for path in sorted(glob.glob("measurements/*.json")):
        name = os.path.basename(path)
        if name.startswith("failure-") or name.startswith(AFFINITY_PREFIXES):
            continue
        parts = name[: -len(".json")].rsplit("-", 2)
        if len(parts) != 3:
            continue
        algorithm, connections, repeat = parts
        run = json.load(open(path))
        per_repeat.setdefault((algorithm, int(connections)), {})[int(repeat)] = answered(run)

    ratios = {}
    for cell, runs in per_repeat.items():
        if 1 not in runs or runs[1] == 0:
            continue
        for repeat, rate in runs.items():
            ratios.setdefault(repeat, []).append(rate / runs[1])

    if len(ratios) < 2:
        return

    print()
    print("| Repeat | Cells | Throughput against the same cell's first run |")
    print("| --- | --- | --- |")
    for repeat in sorted(ratios):
        values = [100 * r for r in ratios[repeat]]
        print(f"| {repeat} | {len(values)} | {spread(values, 1, '%')} |")


def load(pattern="measurements/*.json", failures=False):
    """Groups every run by the cell it belongs to: the load level and the label
    the generator was given."""
    cells = {}
    for path in sorted(glob.glob(pattern)):
        name = os.path.basename(path)
        if name.startswith("failure-") != failures or name.startswith(AFFINITY_PREFIXES):
            continue
        run = json.load(open(path))
        key = run.get("algorithm", "") if failures else (run["connections"], run.get("algorithm", ""))
        cells.setdefault(key, []).append(run)
    return cells


def failure_table():
    """The runs with a backend taken away part way through."""
    cells = load(failures=True)
    if not cells:
        return

    print()
    print("| Run | Runs | Answered | p95 | p99 | Refused | Retries |")
    print("| --- | --- | --- | --- | --- | --- | --- |")
    for label in sorted(cells):
        runs = cells[label]
        retries = [r.get("balancer_counters", {}).get("retries", 0) for r in runs]
        print(
            f"| {label} | {len(runs)} "
            f"| {spread([answered(r) for r in runs], 0, '/s')} "
            f"| {spread([to_ms(r['p95']) for r in runs], 1, ' ms')} "
            f"| {spread([to_ms(r['p99']) for r in runs], 1, ' ms')} "
            f"| {spread([refused(r) for r in runs], 1, '%')} "
            f"| {spread(retries, 0)} |"
        )


def answered(run):
    return run["statuses"].get("200", 0) / seconds_of(run)


def refused(run):
    total = run["requests"]
    return 100 * (total - run["statuses"].get("200", 0)) / total if total else 0.0


def share(run, backend="backend-9"):
    total = run["requests"]
    return 100 * run["backends"].get(backend, 0) / total if total else 0.0


def spread(values, digits=0, unit=""):
    """The median of a cell, with its range when the runs disagree."""
    median = statistics.median(values)
    low, high = min(values), max(values)
    shown = f"{median:,.{digits}f}{unit}"
    if round(low, digits) == round(high, digits):
        return shown
    return f"{shown} ({low:,.{digits}f}–{high:,.{digits}f})"


def matrix(cells):
    print("| Connections | Algorithm | Runs | Answered | p95 | p99 | Refused | backend-9 |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- |")
    for connections in sorted({c for c, _ in cells}):
        for algorithm in ALGORITHMS:
            runs = cells.get((connections, algorithm))
            if not runs:
                continue
            print(
                f"| {connections:,} | {algorithm.replace('_', ' ')} | {len(runs)} "
                f"| {spread([answered(r) for r in runs], 0, '/s')} "
                f"| {spread([to_ms(r['p95']) for r in runs], 1, ' ms')} "
                f"| {spread([to_ms(r['p99']) for r in runs], 1, ' ms')} "
                f"| {spread([refused(r) for r in runs], 1, '%')} "
                f"| {spread([share(r) for r in runs], 1, '%')} |"
            )


def container_cpu(path):
    """Mean CPU per container over the samples of one run."""
    per_container = {}
    for line in open(path):
        row = line.strip().split(",")
        if len(row) < 2:
            continue
        try:
            per_container.setdefault(row[0], []).append(float(row[1].rstrip("%")))
        except ValueError:
            continue
    return {name: statistics.mean(values) for name, values in per_container.items()}


def service_of(container):
    """The Compose service behind a container name, such as deploy-backend-10-1."""
    match = re.fullmatch(r"(?:[^-]+-)?(.+)-\d+", container)
    return match.group(1) if match else container


def resources(level=None):
    """What each container was doing while the load ran, from the docker stats
    samples. The generator runs on the host and is not in this table; it
    competes for the same cores, which is the campaign's main caveat."""
    cells = {}
    for path in sorted(glob.glob("measurements/*.stats.csv")):
        name = os.path.basename(path)[: -len(".stats.csv")]
        parts = name.rsplit("-", 2)
        if len(parts) != 3:
            continue
        algorithm, connections, _ = parts
        if level is not None and int(connections) != level:
            continue
        cells.setdefault((int(connections), algorithm), []).append(container_cpu(path))

    if not cells:
        return

    print()
    print("| Connections | Algorithm | Runs | Balancer CPU | Ten backends, together | Busiest backend |")
    print("| --- | --- | --- | --- | --- | --- |")

    for connections in sorted({c for c, _ in cells}):
        for algorithm in ALGORITHMS:
            runs = cells.get((connections, algorithm))
            if not runs:
                continue

            def totals(predicate):
                return [
                    sum(cpu for name, cpu in run.items() if predicate(service_of(name)))
                    for run in runs
                ]

            backends = {}
            for run in runs:
                for name, cpu in run.items():
                    service = service_of(name)
                    if service.startswith("backend"):
                        backends.setdefault(service, []).append(cpu)

            busiest = max(backends, key=lambda b: statistics.mean(backends[b])) if backends else "—"
            busiest_cpu = statistics.mean(backends[busiest]) if backends else 0.0

            print(
                f"| {connections:,} | {algorithm.replace('_', ' ')} | {len(runs)} "
                f"| {spread(totals(lambda s: s == 'loadbalancer'), 0, '%')} "
                f"| {spread(totals(lambda s: s.startswith('backend')), 0, '%')} "
                f"| {busiest} at {busiest_cpu:.0f}% |"
            )


def histogram(level):
    """The latency distribution at one connection count, one row per band."""
    for algorithm in ALGORITHMS:
        runs = [
            json.load(open(p))
            for p in sorted(glob.glob(f"measurements/{algorithm}-{level}-*.json"))
        ]
        if not runs:
            continue
        run = runs[0]
        print()
        print(f"{algorithm} at {level} connections, {run['requests']:,} requests")
        print("| Answered within | Requests | Share |")
        print("| --- | --- | --- |")
        for band in run.get("histogram", []):
            print(
                f"| {band['under']} | {band['requests']:,} "
                f"| {100 * band['requests'] / run['requests']:.1f}% |"
            )


def provenance(runs):
    """Says which generator made the runs of a table, and warns when it was not
    one committed generator: a campaign measured with a generator that changed
    part way through compares two instruments, not two configurations."""
    revisions = {run.get("generator", {}).get("revision", "unknown") for run in runs}
    modified = sum(1 for run in runs if run.get("generator", {}).get("modified"))
    names = ", ".join(sorted(revision[:12] for revision in revisions))
    if len(revisions) == 1 and "unknown" not in revisions and not modified:
        print(f"\nAll {len(runs)} runs by the generator at {names}.")
        return
    print(f"\nWarning: {len(runs)} runs by generators {names}; {modified} built from a tree with changes"
          " not committed. Rerun them with one committed generator before quoting them.")


def counter_share(run, prefix, part, whole):
    """One of the balancer's counters as a share of others, from a run's
    counters, or None when none of them moved."""
    counters = run.get("balancer_counters", {})
    total = sum(counters.get(prefix + name, 0) for name in whole)
    return 100 * counters.get(prefix + part, 0) / total if total else None


def maybe(values, digits=1, unit="%"):
    present = [v for v in values if v is not None]
    return spread(present, digits, unit) if present else "—"


def affinity(pool):
    """The runs of scripts/measure-affinity.sh on one pool."""
    cells, every = {}, []
    for path in sorted(glob.glob(f"measurements/affinity-{pool}-*.json")):
        run = json.load(open(path))
        every.append(run)
        cells.setdefault((run["connections"], run.get("algorithm", "")), []).append(run)
    if not cells:
        sys.exit(f"no affinity runs on the {pool} pool in measurements/")

    print("| Connections | Configuration | Runs | Answered | p50 | p99 | Refused | Hit rate "
          "| Moved by the bound | Kept by the cookie |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    placements = ["home", "overloaded", "unavailable"]
    sticky = ["pinned", "new", "repinned", "unknown"]
    for connections in sorted({c for c, _ in cells}):
        for configuration in CONFIGURATIONS:
            runs = cells.get((connections, configuration))
            if not runs:
                continue
            hits = [100 * r["cache"]["hit_rate"] if r.get("cache") else None for r in runs]
            print(
                f"| {connections:,} | {configuration.replace('_', ' ')} | {len(runs)} "
                f"| {spread([answered(r) for r in runs], 0, '/s')} "
                f"| {spread([to_ms(r['p50']) for r in runs], 1, ' ms')} "
                f"| {spread([to_ms(r['p99']) for r in runs], 1, ' ms')} "
                f"| {spread([refused(r) for r in runs], 1, '%')} "
                f"| {maybe(hits)} "
                f"| {maybe([counter_share(r, 'placed ', 'overloaded', placements) for r in runs])} "
                f"| {maybe([counter_share(r, 'sticky ', 'pinned', sticky) for r in runs])} |"
            )
    provenance(every)


def pool_change():
    """The runs of scripts/measure-pool-change.py."""
    cells = {}
    for path in sorted(glob.glob("measurements/change-*.json")):
        if path.endswith(".generator.json"):
            continue
        record = json.load(open(path))
        cells.setdefault((record["event"], record["configuration"]), []).append(record)
    if not cells:
        sys.exit("no pool change runs in measurements/")

    print("| Event | Configuration | Runs | Hit rate before | First 3 s after | Next 22 s "
          "| New backend's share | Failed requests |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- |")
    for (event, configuration), records in sorted(cells.items()):
        def rates(key):
            return [100 * r[key]["hit_rate"] if r[key]["hit_rate"] is not None else None for r in records]
        shares = [100 * r["joiner_share"] if "joiner_share" in r else None for r in records]
        print(
            f"| {event} | {configuration.replace('_', ' ')} | {len(records)} "
            f"| {maybe(rates('before'))} | {maybe(rates('first_3s'))} | {maybe(rates('next_22s'))} "
            f"| {maybe(shares)} | {spread([r['generator']['failures'] for r in records], 0)} |"
        )
    provenance([r["generator"] for records in cells.values() for r in records])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--histogram", type=int, metavar="CONNECTIONS")
    parser.add_argument("--resources", type=int, metavar="CONNECTIONS")
    parser.add_argument("--failures", action="store_true")
    parser.add_argument("--affinity", choices=["memory", "stateless"])
    parser.add_argument("--pool-change", action="store_true")
    arguments = parser.parse_args()

    if arguments.affinity:
        affinity(arguments.affinity)
        return

    if arguments.pool_change:
        pool_change()
        return

    if arguments.histogram:
        histogram(arguments.histogram)
        return

    if arguments.failures:
        failure_table()
        return

    cells = load()
    if not cells:
        sys.exit("no runs in measurements/")
    matrix(cells)
    resources(arguments.resources)
    drift()


if __name__ == "__main__":
    main()
