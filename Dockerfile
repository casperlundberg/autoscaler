# Build stage. Pinned to the Go version the module declares so a CI image
# upgrade cannot silently change what is compiled.
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Which code this is. There is no .git in the build context, so CI passes the
# version and commit in, and they are stamped into the binary for
# /v1/version to report. Unstamped, the binary says it does not know.
ARG VERSION=""
ARG COMMIT=""
ARG MODIFIED="false"

# CGO off and a static build, because the runtime image has no libc.
# Symbols and DWARF stripped: this binary is deployed, not debugged in place.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w \
        -X github.com/casperlundberg/autoscaler/internal/buildinfo.version=${VERSION} \
        -X github.com/casperlundberg/autoscaler/internal/buildinfo.commit=${COMMIT} \
        -X github.com/casperlundberg/autoscaler/internal/buildinfo.modified=${MODIFIED}" \
      -o /out/autoscaler ./cmd/autoscaler

# Runtime stage. Static distroless: no shell, no package manager, nothing to
# pivot to. This process holds Kubernetes tokens, ColonyOS private keys and
# Docker client certificates, so the smaller the reachable surface the better.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/autoscaler /usr/local/bin/autoscaler

# The API and probes.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/autoscaler"]
