# syntax=docker/dockerfile:1
# ═════════════════════════════════════════════════════════════════════════════
# capydb CLI — local/dev image.
#
#   make docker-build     → capydb:<version>
#   docker build .
#
# Released images are built by GoReleaser from prebuilt binaries using
# Dockerfile.goreleaser; this file is the from-source path.
# ═════════════════════════════════════════════════════════════════════════════

# ── build-time knobs (override with --build-arg) ─────────────────────────────
ARG BUILD_IMAGE=golang:1.27.1-alpine
ARG RUNTIME_IMAGE=alpine:3.24

ARG APP_UID=10001
ARG APP_GID=10001

ARG BUILD_VERSION=dev
ARG BUILD_DATE=unknown
ARG GIT_COMMIT=none

# ─────────────────────────────────────────────────────────────────────────────
# base — Go toolchain, runs natively on the builder (no QEMU)
# ─────────────────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM ${BUILD_IMAGE} AS base
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
WORKDIR /src

# ─────────────────────────────────────────────────────────────────────────────
# deps — module download, warmed into a persistent builder cache
# ─────────────────────────────────────────────────────────────────────────────
FROM base AS deps
RUN --mount=type=bind,source=.,target=.,ro \
    --mount=type=cache,target=/go/pkg/mod,id=gomod \
    go mod download

# ─────────────────────────────────────────────────────────────────────────────
# build — cross-compile for $TARGETPLATFORM, hand off through /out
# ─────────────────────────────────────────────────────────────────────────────
FROM deps AS build
ARG TARGETOS TARGETARCH
ARG BUILD_VERSION BUILD_DATE GIT_COMMIT
RUN --mount=type=bind,source=.,target=.,ro \
    --mount=type=cache,target=/go/pkg/mod,id=gomod \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-${TARGETOS}-${TARGETARCH} \
    mkdir -p /out && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X main.version=${BUILD_VERSION} -X main.date=${BUILD_DATE} -X main.commit=${GIT_COMMIT} -X main.builtBy=docker" \
      -o /out/capydb ./cmd/capydb

# ─────────────────────────────────────────────────────────────────────────────
# runtime — default target
#   alpine, not distroless: this image keeps a shell so the CLI stays usable
#   interactively (`docker run -it --entrypoint sh ...`).
# ─────────────────────────────────────────────────────────────────────────────
FROM ${RUNTIME_IMAGE} AS runtime
ARG APP_UID APP_GID BUILD_VERSION BUILD_DATE GIT_COMMIT

LABEL org.opencontainers.image.title="capydb" \
      org.opencontainers.image.description="CapyDB command-line interface" \
      org.opencontainers.image.version="${BUILD_VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/capydatabase/capydb-cli" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.vendor="CapyDB"

RUN apk add --no-cache ca-certificates tzdata \
  && addgroup -g ${APP_GID} capydb \
  && adduser -D -u ${APP_UID} -G capydb -s /bin/sh capydb

COPY --from=build --chown=${APP_UID}:${APP_GID} /out/capydb /usr/local/bin/capydb

USER ${APP_UID}:${APP_GID}
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/capydb"]
CMD ["--help"]
