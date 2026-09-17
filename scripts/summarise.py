#!/usr/bin/env python3
"""Turns the runs in ./measurements into the tables the performance report quotes.

Each cell of the matrix — one algorithm at one connection count — may have been
run several times. A single run cannot separate two figures a few per cent
apart, so every number is reported as the median of its runs with the range
beside it, and the report only draws conclusions from gaps wider than that.

    scripts/summarise.py                  # the matrix and the resource table
    scripts/summarise.py --histogram 2000 # the latency distribution at one level

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
        if name.startswith("failure-"):
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
        if name.startswith("failure-") != failures:
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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--histogram", type=int, metavar="CONNECTIONS")
    parser.add_argument("--resources", type=int, metavar="CONNECTIONS")
    parser.add_argument("--failures", action="store_true")
    arguments = parser.parse_args()

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
