#!/usr/bin/env bash
#
# Measures the configurations that keep a client on one backend against those
# that do not, on one pool: the ten mock backends either as they are
# (POOL=stateless) or remembering their clients' sessions (POOL=memory, the
# default). Every request names one of 30,000 sessions in X-Session, and every
# session keeps the cookies it is given, as a browser would, so that consistent
# hashing and sticky sessions have what they need and the other algorithms see
# exactly the same traffic.
#
#   scripts/measure-affinity.sh                          # every configuration
#   POOL=stateless scripts/measure-affinity.sh           # the backends without memory
#   REPEATS=3 CONNECTIONS="50 300" scripts/measure-affinity.sh round_robin consistent_hash
#
# The script brings the stack up itself, with the overlays the pool needs, and
# writes each configuration to measurements/lb.affinity.yaml through
# scripts/configure.py; nothing in the repository is changed. Results are
# written to ./measurements as affinity-<pool>-<configuration>-<connections>-<repeat>.json,
# and scripts/summarise.py --affinity <pool> turns them into the report's table.
# DURATION, WARMUP, CONNECTIONS, REPEATS, KEYS and COOLDOWN can be overridden.
set -euo pipefail

cd "$(dirname "$0")/.."

# shellcheck source=scripts/lib.sh
source scripts/lib.sh

pool=${POOL:-memory}
case "$pool" in
  memory)
    compose=(docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.memory.yml -f deploy/docker-compose.affinity.yml)
    # The caches start empty and have to fill before the window opens: at 50
    # connections a minute sees all but a few per cent of 30,000 sessions.
    warmup=${WARMUP:-60s}
    ;;
  stateless)
    compose=(docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.affinity.yml)
    warmup=${WARMUP:-4s}
    ;;
  *) echo "POOL must be memory or stateless" >&2; exit 1 ;;
esac

target=127.0.0.1:8080
duration=${DURATION:-20s}
repeats=${REPEATS:-1}
keys=${KEYS:-30000}
cooldown=${COOLDOWN:-8}
read -r -a connections <<< "${CONNECTIONS:-50}"

configurations=("$@")
if [ ${#configurations[@]} -eq 0 ]; then
  configurations=(round_robin least_connections weighted_round_robin consistent_hash
    consistent_hash_150 consistent_hash_125_by_capacity sticky_least_connections)
fi

mkdir -p "$results"
build_loadgen

# The configuration file has to exist before the stack comes up, or Docker
# mounts a directory in its place.
python3 scripts/configure.py "${configurations[0]}" >/dev/null
"${compose[@]}" up -d --build >/dev/null
ready

# The pool must be the one asked for: a stack left running with the other
# overlay would measure the wrong backends.
cache_size=$(curl -sf 127.0.0.1:5781/faults | python3 -c 'import json, sys; print(json.load(sys.stdin)["cache_size"])')
if { [ "$pool" = memory ] && [ "$cache_size" -eq 0 ]; } || { [ "$pool" = stateless ] && [ "$cache_size" -ne 0 ]; }; then
  echo "the mock backends remember $cache_size sessions, which is not the $pool pool" >&2
  exit 1
fi

# As in measure.sh, the configurations are interleaved and their order rotated
# each repeat, so that the machine's drift through a long campaign is shared
# out rather than credited to whichever went first.
for repeat in $(seq 1 "$repeats"); do
  rotated=()
  for i in "${!configurations[@]}"; do
    rotated+=("${configurations[$(( (i + repeat - 1) % ${#configurations[@]} ))]}")
  done

  for count in "${connections[@]}"; do
    for configuration in "${rotated[@]}"; do
      apply "$configuration"
      clear_caches
      ready

      "$loadgen" \
        -addr "http://$target" \
        -connections "$count" \
        -duration "$duration" \
        -warmup "$warmup" \
        -keys "$keys" \
        -cookies \
        -label "$configuration" \
        -json "$results/affinity-$pool-$configuration-$count-$repeat.json" | tee -a "$results/affinity-log.txt"

      sleep "$cooldown"
    done
  done
done

echo "results in $results/"
