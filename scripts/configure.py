#!/usr/bin/env python3
"""Writes the balancer's configuration for one of the configurations the
affinity measurements compare.

    scripts/configure.py consistent_hash_150
    scripts/configure.py sticky_least_connections --without backend-6

Each is configs/lb.measure.yaml with its algorithm, its consistent hashing
bound, sticky sessions and weights set, written to measurements/lb.affinity.yaml,
where deploy/docker-compose.affinity.yml mounts it. --without leaves a backend
out of the pool, for a run in which it joins part way through. The file is
rewritten in place, so the bind mount keeps seeing it.

Only the standard library is used, so it runs wherever python3 does.
"""

import argparse
import os
import re
import sys

SOURCE = "configs/lb.measure.yaml"
TARGET = "measurements/lb.affinity.yaml"

# The configurations compared. The measurement configuration's weights follow
# each backend's capacity; "equal" sets them all to one, as the shipped
# configurations have them.
CONFIGURATIONS = {
    "round_robin": {"algorithm": "round_robin"},
    "least_connections": {"algorithm": "least_connections"},
    "weighted_round_robin": {"algorithm": "weighted_round_robin"},
    "consistent_hash": {"algorithm": "consistent_hash", "balance_factor": 0, "weights": "equal"},
    "consistent_hash_150": {"algorithm": "consistent_hash", "balance_factor": 150, "weights": "equal"},
    "consistent_hash_125_by_capacity": {"algorithm": "consistent_hash", "balance_factor": 125},
    "sticky_least_connections": {"algorithm": "least_connections", "sticky": "cookie"},
}


def render(name, without=()):
    """The configuration text for a named configuration."""
    settings = CONFIGURATIONS[name]
    text = open(SOURCE).read()

    text, found = re.subn(r"(?m)^algorithm: .*$", f"algorithm: {settings['algorithm']}", text)
    assert found == 1, "no algorithm line"
    text, found = re.subn(r"(?m)^(  balance_factor: )\d+",
                          lambda m: m.group(1) + str(settings.get("balance_factor", 150)), text)
    assert found == 1, "no balance_factor line"
    text, found = re.subn(r"(?m)^(  mode: )\w+", lambda m: m.group(1) + settings.get("sticky", "none"), text)
    assert found == 1, "no sticky mode line"
    if settings.get("weights") == "equal":
        text = re.sub(r"(?m)^(    weight: )\d+", r"\g<1>1", text)
    for backend in without:
        text, found = re.subn(rf'(?m)^  - addr: "{re.escape(backend)}:5678"\n    weight: \d+\n', "", text)
        assert found == 1, f"no backend {backend}"
    return text


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("configuration", choices=sorted(CONFIGURATIONS))
    parser.add_argument("--without", action="append", default=[], metavar="BACKEND",
                        help="leave this backend out of the pool, such as backend-6")
    args = parser.parse_args()

    os.makedirs(os.path.dirname(TARGET), exist_ok=True)
    with open(TARGET, "w") as out:
        out.write(render(args.configuration, args.without))
    settings = CONFIGURATIONS[args.configuration]
    # What /status will report once the balancer has reloaded it.
    print(settings["algorithm"], settings.get("sticky", "none"))


if __name__ == "__main__":
    sys.exit(main())
