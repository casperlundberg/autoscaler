# Build stage. Pinned to the Go version the module declares so a CI image
# upgrade cannot silently change what is compiled.
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off and a static build, because the runtime image has no libc.
# Symbols and DWARF stripped: this binary is deployed, not debugged in place.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/autoscaler ./cmd/autoscaler

# Runtime stage. Static distroless: no shell, no package manager, nothing to
# pivot to. This process holds Kubernetes tokens, ColonyOS private keys and
# Docker client certificates, so the smaller the reachable surface the better.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/autoscaler /usr/local/bin/autoscaler

# The API and probes.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/autoscaler"]
