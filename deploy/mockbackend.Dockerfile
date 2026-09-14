# The mock backend that stands in for real services in the demo environment.
# Built from the repository root, like the balancer's own image.
FROM golang:1.27-alpine AS build

WORKDIR /src

# The mock uses the standard library only, so the module file and its own
# package are all the build needs; mockbackend.Dockerfile.dockerignore keeps
# everything else out of the context.
COPY go.mod ./
COPY cmd/mockbackend ./cmd/mockbackend

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockbackend ./cmd/mockbackend

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/mockbackend /usr/local/bin/mockbackend

EXPOSE 5678

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mockbackend"]
