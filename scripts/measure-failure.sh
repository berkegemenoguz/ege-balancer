#!/usr/bin/env bash
#
# One run per algorithm at a fixed load, with a backend taken away part way
# through and started again afterwards, to see what each algorithm does when
# capacity disappears under load.
#
# MODE says how it is taken away. "stop" sends SIGTERM, which the mock backend
# handles: it finishes the requests it has and closes its listener, so this
# measures a backend drained on purpose. "kill" sends SIGKILL, which drops every
# open connection at once and measures a crash.
#
#   docker compose -f deploy/docker-compose.yml \
#                  -f deploy/docker-compose.measure.yml up -d
#   scripts/measure-failure.sh
#
# VICTIM, CONNECTIONS, DURATION, WARMUP, KILL_AFTER and REPEATS can be
# overridden from the environment. Results are written to ./measurements, where
# scripts/summarise.py can be pointed at them.
set -euo pipefail

cd "$(dirname "$0")/.."

compose=(docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.measure.yml)
config=configs/lb.measure.yaml
status=127.0.0.1:8081
results=measurements

victim=${VICTIM:-backend-1}
connections=${CONNECTIONS:-600}
duration=${DURATION:-30s}
warmup=${WARMUP:-5s}
kill_after=${KILL_AFTER:-15}
mode=${MODE:-stop}
repeats=${REPEATS:-1}

# taken is how the run is labelled: SIGTERM lets the mock finish what it holds,
# SIGKILL does not.
case "$mode" in
  stop) taken="drained" ;;
  kill) taken="crashed" ;;
  *) echo "MODE must be stop or kill" >&2; exit 1 ;;
esac

algorithms=("$@")
if [ ${#algorithms[@]} -eq 0 ]; then
  algorithms=(round_robin weighted_round_robin least_connections)
fi

mkdir -p "$results"

switch() {
  perl -pi -e "s/^algorithm: .*/algorithm: $1              # the measurement script rewrites this/" "$config"
  "${compose[@]}" kill -s HUP loadbalancer >/dev/null

  for _ in $(seq 1 40); do
    if curl -sf "$status/status" | grep -q "\"algorithm\":\"$1\""; then
      return 0
    fi
    sleep 0.5
  done

  echo "the balancer did not switch to $1" >&2
  exit 1
}

# whole waits until the balancer sees every backend healthy again, so the next
# algorithm does not start a backend short.
whole() {
  for _ in $(seq 1 60); do
    if curl -sf "$status/status" | grep -q '"healthy_backends":10'; then
      return 0
    fi
    sleep 1
  done

  echo "the pool did not come back to ten healthy backends" >&2
  exit 1
}

for algorithm in "${algorithms[@]}"; do
  switch "$algorithm"

  for repeat in $(seq 1 "$repeats"); do
    whole

      echo "== $algorithm: $victim $taken ${kill_after}s in (run $repeat)"
    go run ./cmd/loadgen \
      -connections "$connections" \
      -duration "$duration" \
      -warmup "$warmup" \
      -label "$algorithm, $victim $taken" \
      -json "$results/failure-$mode-$algorithm-$repeat.json" &
    generator=$!

    sleep "$kill_after"
    "${compose[@]}" "$mode" "$victim" >/dev/null 2>&1

    wait "$generator"

    "${compose[@]}" start "$victim" >/dev/null 2>&1
  done
done

echo "results in $results/"
