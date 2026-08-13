# syntax=docker/dockerfile:1

# ── Build ────────────────────────────────────────────────────────────────────
FROM golang:1.26.6-alpine AS build

WORKDIR /src

# Sluice has no third-party dependencies, so there is no module download step
# and nothing to cache between the manifest and the source. Copying go.mod
# first still keeps the layer stable when only source changes.
COPY go.mod ./
COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO is off and the binary is fully static, so the runtime image needs no libc.
# Trimpath and the omitted symbol table keep the image small and the build
# reproducible from the same source tree.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/sluiced ./cmd/sluiced && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" \
      -o /out/sluicectl ./cmd/sluicectl && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" \
      -o /out/sluice-upstream ./cmd/sluice-upstream

# Deliberately no `go test` here. The suite starts ten HTTP listeners and
# depends on timing, which is fine on a workstation and flaky inside a
# constrained build container — and a test failure at image-build time is
# discovered later and read less carefully than one in CI. The pipeline runs
# the suite with the race detector before this image is ever built.

# ── Runtime ──────────────────────────────────────────────────────────────────
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -g 65532 -S sluice && \
    adduser -u 65532 -S -G sluice -H -s /sbin/nologin sluice

COPY --from=build /out/sluiced /usr/local/bin/sluiced
COPY --from=build /out/sluicectl /usr/local/bin/sluicectl
COPY --from=build /out/sluice-upstream /usr/local/bin/sluice-upstream
COPY configs/policies.sluice /etc/sluice/policies.sluice

USER 65532:65532

# 8080 dashboard, REST API and Prometheus; 8081 Envoy ext_authz;
# 9443 the optional native data plane.
EXPOSE 8080 8081 9443

# readyz reports healthy only once a traffic distribution has been computed;
# before that the router would deny everything for want of a plan.
HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/readyz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/sluiced"]

# A container has to bind every interface to be reachable, which is exactly the
# configuration that refuses to start without an API token. For the zero-config
# `docker run sluice` demo we opt in explicitly, and the process logs a standing
# warning that the write API is open. A real deployment overrides this CMD and
# sets SLUICE_API_TOKEN.
CMD ["--demo", "--listen", "0.0.0.0:8080", "--dev-insecure"]
