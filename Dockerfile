# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:1.27.3-bookworm AS build

WORKDIR /src

# No third-party dependencies: the module builds offline.
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/starbound-panel ./cmd/starbound-panel

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive

# steamcmd is a 32-bit binary; the Starbound dedicated server is 64-bit and
# needs libstdc++ and libvorbisfile. lzip unpacks the OpenStarbound release
# archives (.tar.lz).
RUN dpkg --add-architecture i386 \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        tar \
        gzip \
        lzip \
        tzdata \
        lib32gcc-s1 \
        lib32stdc++6 \
        libstdc++6 \
        libvorbisfile3 \
    && rm -rf /var/lib/apt/lists/*

# SteamCMD. It updates itself on first use, so keep it in a writable directory
# owned by the runtime user.
RUN mkdir -p /opt/steamcmd \
    && curl -fsSL https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz \
       | tar -xz -C /opt/steamcmd

# The base image already ships an "ubuntu" user with uid/gid 1000; replace it
# with a dedicated account so the container runs as a named non-root user.
RUN userdel -r ubuntu 2>/dev/null || true; \
    groupdel ubuntu 2>/dev/null || true; \
    groupadd -g 1000 starbound \
    && useradd -m -u 1000 -g starbound starbound \
    && mkdir -p /data /backup /home/starbound \
    && chown -R starbound:starbound /data /backup /opt/steamcmd /home/starbound

COPY --from=build /out/starbound-panel /usr/local/bin/starbound-panel

ENV SB_DATA_DIR=/data \
    SB_BACKUP_DIR=/backup \
    SB_STEAMCMD=/data/steamcmd/steamcmd.sh \
    SB_STEAMCMD_SEED=/opt/steamcmd \
    SB_LISTEN=:8080

USER 1000:1000
EXPOSE 8080 21025

VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s \
    CMD curl -fsS http://127.0.0.1:8080/api/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/starbound-panel"]
