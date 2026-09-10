# Build the binary in a full toolchain image, then ship only the binary.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so that editing source does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO is off so the result is a static binary the runtime image can run without
# a libc; -trimpath keeps build paths out of it, -s -w drop the debug tables.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/lb ./cmd/lb

# distroless carries no shell, no package manager and no user database beyond
# the nonroot account, so there is very little in the image to attack.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/lb /usr/local/bin/lb
COPY configs/lb.example.yaml /etc/lb/config.yaml

# 8080 carries proxied traffic, 8081 serves /metrics and /status.
EXPOSE 8080 8081

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/lb"]
CMD ["-config", "/etc/lb/config.yaml"]
