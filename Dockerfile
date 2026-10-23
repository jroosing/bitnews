# syntax=docker/dockerfile:1

# bitnews container image: a static Go binary and the two static post-processing
# tools (7-Zip, par2cmdline-turbo) on distroless.
#
#   docker build -t bitnews --build-arg VERSION="$(git describe --tags --always --dirty)" \
#                           --build-arg COMMIT="$(git rev-parse --short HEAD)" .
#
# See docs/deploy.md for running it, and deploy/docker-compose.example.yml.

# Build stage. It runs on the build host's own platform and compiles for
# the target platform (linux/amd64 is the supported one; building on an
# Apple Silicon Mac for local testing gives an arm64 image).
# Pin the tag to a digest (golang:1.27.3@sha256:...) for reproducible builds;
# docs/deploy.md describes how.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

# The toolchain in the image is the one go.mod asks for; never download another.
ENV GOTOOLCHAIN=local

WORKDIR /src

# Dependencies first, so this layer is reused until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO_ENABLED=0 gives a fully static binary, which is what distroless/static
# needs (it has no libc). -buildvcs=false because .git is not in the context.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
        -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" \
        -o /out/bitnews ./cmd/bitnews

# The post-processing tools (ADR-0007): 7-Zip (7zz) and par2cmdline-turbo
# (par2), static binaries pinned by version and SHA-256 in
# scripts/get-postproc-tools.sh, fetched for the target platform.
RUN apt-get update \
 && apt-get install -y --no-install-recommends xz-utils unzip \
 && rm -rf /var/lib/apt/lists/* \
 && scripts/get-postproc-tools.sh linux "$TARGETARCH" /out/tools

# /config skeleton, copied into the final stage because distroless has no
# shell to mkdir with. The final stage copies it with mode 0777, so a fresh
# named volume (which copies that directory's owner and mode on first use) can
# be written by whatever uid `user:` / `--user` selects. Bind mounts are not
# affected: they bring their own owner and mode.
RUN mkdir -p /out/config

# Final stage. bitnews runs as whoever starts it (ADR-0006): the default is
# the distroless "nonroot" user (65532), and `user: "uid:gid"` in compose or
# `docker run --user` overrides it. There is no privilege drop and no
# capability to add back.
# Pin to a digest here too for reproducible builds.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="bitnews" \
      org.opencontainers.image.description="Self-hosted Usenet and BitTorrent download daemon" \
      org.opencontainers.image.source="https://gitlab.com/kyura4k/bitnews" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT"

COPY --from=build /out/bitnews /bitnews
COPY --from=build /out/tools/7zz /out/tools/par2 /usr/local/bin/
COPY --from=build --chown=65532:65532 --chmod=0777 /out/config /config

# Bootstrap settings. The master key is generated on first start and stored in
# /config next to the database (set BITNEWS_KEY_FILE to keep it elsewhere).
# UMASK 002 makes new files group-writable, so Sonarr and Radarr running as
# another user in the same group can manage them.
ENV BITNEWS_CONFIG_DIR=/config \
    BITNEWS_LISTEN=0.0.0.0:9494 \
    UMASK=002

EXPOSE 9494
VOLUME /config
USER 65532:65532

# Distroless has no shell, so the exec form is the only form that works.
# "bitnews healthcheck" exits 0 when GET http://127.0.0.1:<port>/api/v1/health
# answers 200 within 3 s (port from BITNEWS_LISTEN), otherwise 1. The 5 s
# timeout leaves room above that 3 s.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/bitnews", "healthcheck"]

ENTRYPOINT ["/bitnews"]
