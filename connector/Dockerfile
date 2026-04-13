# Dockerfile — ZECURITY connector multi-stage image
#
# IMPORTANT: build from the repo root, NOT from connector/:
#   docker build -f connector/Dockerfile -t zecurity-connector:latest .
#
# The Rust build needs access to controller/proto/connector/connector.proto
# which lives OUTSIDE the connector/ directory. Building from the repo root
# gives Docker access to both trees.

# ── Stage 1: Build the Rust binary ──────────────────────────────────────────
FROM rust:1.83-slim AS builder

WORKDIR /build

# System build dependencies for protobuf + TLS crates.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        pkg-config \
        libssl-dev \
        protobuf-compiler \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Copy proto first so builds cache even when only Rust source changes.
COPY controller/proto ./controller/proto

# Copy connector sources.
COPY connector/Cargo.toml connector/Cargo.lock connector/build.rs ./connector/
COPY connector/src ./connector/src

WORKDIR /build/connector

# Build release binary.
RUN cargo build --release

# ── Stage 2: Runtime image ──────────────────────────────────────────────────
FROM debian:bookworm-slim

# Runtime dependencies: TLS roots + curl (for /ca.crt fetch during enrollment).
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl && \
    rm -rf /var/lib/apt/lists/*

# Create unprivileged user matching the systemd unit naming.
RUN useradd --system \
        --home-dir /var/lib/zecurity-connector \
        --shell /usr/sbin/nologin \
        zecurity

# Copy the built binary from the builder stage.
COPY --from=builder /build/connector/target/release/zecurity-connector /usr/local/bin/

# State + config directories. Config is expected to be bind-mounted at runtime.
RUN mkdir -p /var/lib/zecurity-connector /etc/zecurity && \
    chown -R zecurity:zecurity /var/lib/zecurity-connector

USER zecurity

ENTRYPOINT ["/usr/local/bin/zecurity-connector"]
