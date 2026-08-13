# syntax=docker/dockerfile:1

# ── Build ────────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build

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

# Verify the tests pass inside the build, so a broken image cannot be produced
# from a tree that does not pass its own suite.
RUN CGO_ENABLED=0 go test ./... > /dev/null

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
CMD ["--demo"]
