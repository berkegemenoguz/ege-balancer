# Day 1 — Project setup and mock environment

**Planned deliverable:** a working repository skeleton, and ten mock backends that come up with
a single `docker compose up`.

## What was built

- Go module `github.com/berkegemenoguz/ege-balancer`, pinned to Go 1.27.
- The package layout from section 3.1 of the design document: `cmd/lb` as the only entry
  point, and one `internal/` package per responsibility.
- `configs/lb.example.yaml` carrying the full configuration schema from section 3.3.
- `deploy/docker-compose.yml` with ten `hashicorp/http-echo` backends, each capped at 32 MB and
  published on loopback ports 5681 to 5690.
- A GitHub Actions pipeline running gofmt, `go vet`, golangci-lint, govulncheck, build and
  `go test -race`.
- `.gitignore`, MIT `LICENSE` and `README.md`.

## Decisions

**The load balancer service was left out of the compose file, and no Dockerfile was written.**
The day's deliverable is the backend environment; the proxy could not forward anything yet. A
container serving nothing, and a Dockerfile to build it, would have been code written for a
feature that did not exist. The image is day 12's work.

As it turned out, the balancer still runs on the host rather than in compose: when the monitoring
stack arrived on day 8, Prometheus was pointed at `host.docker.internal` instead, which kept the
Dockerfile in day 12 where it belongs.

**Empty packages carry a `doc.go` stating their responsibility, not stub functions.** The
package comment is documentation the reader needs; a placeholder function would have been dead
code from the first commit.

**Directory and package names are single lowercase words, file names may use dashes.** A
directory named `health-check` would force the package name `healthcheck`, leaving the import
path and the package identifier out of step. The module path is all lowercase because the Go
module proxy encodes capitals (`Ege` becomes `!ege`), which invites trouble later.

## What went wrong

**The first push was rejected in full.** The personal access token in use lacked the `workflow`
scope, and GitHub refuses any push that creates or updates a file under `.github/workflows`.
Nothing was wrong with the commits. Resolved by switching the remote to SSH, which meant
generating an ed25519 key, loading it into the agent with `--apple-use-keychain`, and adding the
public key to the account.

**The first CI run failed on the lint step.** `golangci-lint-action@v6` only installs
golangci-lint v1, which no longer resolves; the current action major is v9 and it works with
golangci-lint v2. Fixed by upgrading the action. Lesson applied from then on: run
golangci-lint locally before pushing, instead of using CI as the first check.

## Verification

`docker compose up -d` brought all ten backends up; `curl localhost:5681` answered `backend-1`.
