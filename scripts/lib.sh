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
