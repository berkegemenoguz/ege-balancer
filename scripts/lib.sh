# Shared by the measurement scripts, which source it: where the stack is, and
# how to switch the balancer's algorithm through its configuration file.

compose=(docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.measure.yml)
config=configs/lb.measure.yaml
status=127.0.0.1:8081
results=measurements

# switch rewrites the algorithm in the measurement configuration, asks the
# balancer to reload, and waits until it reports the new one. Going through the
# file and SIGHUP is how an operator would change it.
switch() {
  local algorithm=$1
  perl -pi -e "s/^algorithm: .*/algorithm: $algorithm              # the measurement script rewrites this/" "$config"
  "${compose[@]}" kill -s HUP loadbalancer >/dev/null

  for _ in $(seq 1 40); do
    if curl -sf "$status/status" | grep -q "\"algorithm\":\"$algorithm\""; then
      return 0
    fi
    sleep 0.5
  done

  echo "the balancer did not switch to $algorithm" >&2
  exit 1
}

# build_loadgen builds the generator once and sets $loadgen to it. go build
# stamps the binary with the revision it was built from, and whether the tree
# had changes not yet committed, which go run does not; every result records
# both, so a campaign can be checked for runs made by different generators.
build_loadgen() {
  loadgen="$(mktemp -d)/loadgen"
  go build -o "$loadgen" ./cmd/loadgen
}

# reloads is how many configurations the balancer has applied since it started.
reloads() {
  curl -sf "$status/status" | python3 -c 'import json, sys; print(json.load(sys.stdin)["reloads"])'
}

# apply writes one of the configurations scripts/configure.py knows, asks the
# balancer to reload, and waits until it has. Waiting on the reload count rather
# than on the algorithm matters here: two configurations can share an algorithm
# and differ only in their bound or in sticky sessions.
apply() {
  local before
  before=$(reloads)
  python3 scripts/configure.py "$@" >/dev/null
  "${compose[@]}" kill -s HUP loadbalancer >/dev/null

  for _ in $(seq 1 40); do
    if [ "$(reloads)" -gt "$before" ]; then
      return 0
    fi
    sleep 0.5
  done

  echo "the balancer did not apply $*" >&2
  exit 1
}

# clear_caches makes every mock backend forget the sessions it remembers, so
# that each run starts from the same empty caches. The admin ports are
# published on the loopback interface at 5781 to 5790.
clear_caches() {
  for port in $(seq 5781 5790); do
    curl -sf -X POST "127.0.0.1:$port/cache/clear" >/dev/null || true
  done
}

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
