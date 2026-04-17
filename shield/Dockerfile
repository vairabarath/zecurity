# Dockerfile — ZECURITY shield multi-stage image
#
# IMPORTANT: build from the repo root, NOT from shield/:
#   docker build -f shield/Dockerfile -t zecurity-shield:latest .

# ── Stage 1: Build the Rust binary ──────────────────────────────────────────
FROM rust:1.83-slim AS builder

WORKDIR /build

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        pkg-config \
        libssl-dev \
        protobuf-compiler \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY proto ./proto

COPY shield/Cargo.toml shield/Cargo.lock shield/build.rs ./shield/
COPY shield/src ./shield/src

WORKDIR /build/shield

RUN cargo build --release

# ── Stage 2: Runtime image ──────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        nftables \
        iproute2 && \
    rm -rf /var/lib/apt/lists/*

RUN useradd --system \
        --home-dir /var/lib/zecurity-shield \
        --shell /usr/sbin/nologin \
        zecurity

COPY --from=builder /build/shield/target/release/zecurity-shield /usr/local/bin/

RUN mkdir -p /var/lib/zecurity-shield /etc/zecurity && \
    chown -R zecurity:zecurity /var/lib/zecurity-shield

USER zecurity

ENTRYPOINT ["/usr/local/bin/zecurity-shield"]
