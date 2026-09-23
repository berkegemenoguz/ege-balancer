#!/usr/bin/env bash
#
# Runs the measurements behind docs/performance-report.md: one closed-loop load
# run per algorithm and connection count, against the ten mock backends.
#
#   docker compose -f deploy/docker-compose.yml \
#                  -f deploy/docker-compose.measure.yml up -d
#   scripts/measure.sh                       # every algorithm
#   scripts/measure.sh least_connections     # just one
#   REPEATS=3 scripts/measure.sh             # three runs per cell
#
# Results are written to ./measurements as JSON, one file per run, and printed
# as they finish; scripts/summarise.py turns them into the report's tables.
# Alongside each run it samples docker stats, so the report can say which
# container was busy rather than infer it. DURATION, WARMUP, CONNECTIONS and
# REPEATS can be overridden from the environment.
set -euo pipefail

cd "$(dirname "$0")/.."

# shellcheck source=scripts/lib.sh
source scripts/lib.sh

target=127.0.0.1:8080

duration=${DURATION:-15s}
warmup=${WARMUP:-4s}
repeats=${REPEATS:-1}
# Seconds between runs: long enough for the queues to drain, and the lever to
# pull if the machine needs longer to shed heat.
cooldown=${COOLDOWN:-8}
# The sampler starts after the warmup, so the CPU figures belong to the
# measured window rather than to connections being opened.
warmup_seconds=${WARMUP_SECONDS:-4}
read -r -a connections <<< "${CONNECTIONS:-50 100 300 600 1000 2000}"

algorithms=("$@")
if [ ${#algorithms[@]} -eq 0 ]; then
  algorithms=(round_robin weighted_round_robin least_connections)
fi

mkdir -p "$results"

# ready waits until the balancer has a healthy backend to send traffic to, so a
# run never starts while the pool is still recovering from the one before it.
ready() {
  for _ in $(seq 1 60); do
    if curl -sf "$status/readyz" >/dev/null; then
      return 0
    fi
    sleep 0.5
  done

  echo "the balancer never reported itself ready" >&2
  exit 1
}

# sample records what each container is doing, every two seconds, until it is
# killed. The balancer reads its own counters, so this is only about resources.
sample() {
  sleep "$warmup_seconds"
  while true; do
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' >> "$1"
    sleep 2
  done
}

# The algorithms are interleaved rather than run in blocks: a long campaign
# heats the machine and its throughput drifts downwards, so measuring one
# algorithm's eighteen runs before the next one's would credit whichever went
# first. Rotating them inside each level and each repeat spreads that drift
# evenly, and the repeats show what is left of it.
for repeat in $(seq 1 "$repeats"); do
  # The order of the three is rotated each repeat, so that no algorithm is
  # always the first of a triplet and therefore always on the freshest machine.
  rotated=()
  for i in "${!algorithms[@]}"; do
    rotated+=("${algorithms[$(( (i + repeat - 1) % ${#algorithms[@]} ))]}")
  done

  for count in "${connections[@]}"; do
    for algorithm in "${rotated[@]}"; do
      switch "$algorithm"
      run="$results/$algorithm-$count-$repeat"
      ready

      sample "$run.stats.csv" &
      sampler=$!

      go run ./cmd/loadgen \
        -addr "http://$target" \
        -connections "$count" \
        -duration "$duration" \
        -warmup "$warmup" \
        -label "$algorithm" \
        -json "$run.json" | tee -a "$results/log.txt"

      kill "$sampler" 2>/dev/null || true
      wait "$sampler" 2>/dev/null || true

      # Let the queues drain, so the next run does not start behind this one.
      sleep "$cooldown"
    done
  done
done

echo "results in $results/"
