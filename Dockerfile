# syntax=docker/dockerfile:1

# ---- build stage ------------------------------------------------------------
# Pinned to the toolchain declared in go.mod (go 1.24). BuildKit provides
# TARGETOS/TARGETARCH/TARGETVARIANT automatically for multi-arch builds.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

# git is only needed if modules are fetched from VCS; certs let `go` talk to
# proxy.golang.org over TLS during the build.
RUN apk add --no-cache ca-certificates

WORKDIR /src

# Cache dependencies separately from source for faster rebuilds.
COPY source/go.mod source/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY source/ .

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# Build metadata injected into the binary via -ldflags.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# CGO disabled -> fully static binary that runs in scratch. GOARM is derived
# from the arm variant (e.g. v7 -> 7) so linux/arm/v7 builds correctly.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    GOARM=$(printf '%s' "${TARGETVARIANT}" | tr -d 'v') \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/statusserver .

# ---- runtime stage ----------------------------------------------------------
FROM scratch

# CA roots so outbound HTTPS webhooks (alerts) work.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/statusserver /statusserver

# Persisted agent-uptime history lives here; declared as a volume so it
# survives container recreation. Configure via UPTIME_FILE.
VOLUME ["/data"]
ENV UPTIME_FILE=/data/uptime.json

EXPOSE 8090

# Run as a non-root uid. scratch has no /etc/passwd, but a numeric uid works.
USER 65532:65532

# Use the binary's built-in probe (no shell/curl needed in scratch).
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/statusserver", "-healthcheck"]

ENTRYPOINT ["/statusserver"]
