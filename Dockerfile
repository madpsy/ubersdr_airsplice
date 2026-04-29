# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# Stage 1: build ubersdr_airsplice Go binary
# ---------------------------------------------------------------------------
FROM golang:1.24-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/ubersdr_airsplice ./...

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        lame \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -s /bin/false airsplice

COPY --from=go-builder /out/ubersdr_airsplice /usr/local/bin/ubersdr_airsplice

# Copy entrypoint script (translates env vars to ubersdr_airsplice flags)
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Create the default output directory and ensure the airsplice user owns it.
# Users can volume-mount a host directory over /data to persist recordings.
RUN chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /data \
    && chown airsplice:airsplice /data

USER airsplice

VOLUME ["/data"]

# Expose the web UI port (default; override with WEB_PORT env var)
EXPOSE 6095

# Verify the binary can print help
HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD ["/usr/local/bin/ubersdr_airsplice", "-help"] || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
