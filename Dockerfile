# syntax=docker/dockerfile:1

# ---- build stage ------------------------------------------------------------
# Builds on the native platform and cross-compiles for the target (BuildKit
# provides TARGETOS/TARGETARCH/TARGETVARIANT), so multi-arch builds don't run
# the compiler under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

# CA roots, copied into the runtime image so outbound HTTPS webhooks work.
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
      -o /out/statusserver . \
 && mkdir -p /out/data

# ---- runtime stage ----------------------------------------------------------
FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/statusserver /statusserver

# Persisted agent-uptime history lives here. The directory must exist in the
# image owned by the runtime uid: docker seeds a new named volume from it, so
# the volume is writable too (otherwise it would be root-owned).
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]
ENV UPTIME_FILE=/data/uptime.json

EXPOSE 8090

# Run as a non-root uid. scratch has no /etc/passwd, but a numeric uid works.
USER 65532:65532

# Use the binary's built-in probe (no shell/curl needed in scratch).
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/statusserver", "-healthcheck"]

ENTRYPOINT ["/statusserver"]
