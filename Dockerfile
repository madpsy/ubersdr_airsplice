# syntax=docker/dockerfile:1
#
# Build arguments:
#   WITH_WHISPER=1   also build UberSDR's whisper-helper into the image, for
#                    TRANSCRIBE=true (default 0: the image is unchanged, and
#                    the helper can be mounted in instead; see the README).

ARG WITH_WHISPER=0

# ---------------------------------------------------------------------------
# Optional: UberSDR's whisper-helper (clients/electron/whisper), built as its
# own build-inside.sh does: whisper.cpp at the commit UberSDR pins, every
# binary fully static. Only the stage WITH_WHISPER names is built.
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS whisper-0
RUN mkdir -p /out

# Built on the build machine's own platform and cross-compiled for the target,
# as UberSDR's build does: compiling whisper.cpp under emulation for arm64
# would take far longer.
FROM --platform=$BUILDPLATFORM debian:trixie AS whisper-1
ARG UBERSDR_REPO=https://github.com/madpsy/ka9q_ubersdr
ARG UBERSDR_COMMIT=a7d99bdfb94823988009314e4234c8b74307c5ec
ARG WHISPER_CPP_REPO=https://github.com/ggml-org/whisper.cpp
ARG WHISPER_CPP_COMMIT=0c7be696371069e0baa2515ed7aa08d5a5473aa6
# Parallel compile jobs; lower it on a busy build host.
ARG BUILD_JOBS=4
ARG TARGETARCH
ARG BUILDARCH
RUN apt-get update -qq \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      cmake make gcc g++ git ca-certificates \
 && if [ "$TARGETARCH" = arm64 ] && [ "$BUILDARCH" != arm64 ]; then \
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
        gcc-aarch64-linux-gnu g++-aarch64-linux-gnu libc6-dev-arm64-cross; \
    fi \
 && rm -rf /var/lib/apt/lists/*
# Fetch only the helper's directory, and whisper.cpp, each by commit hash.
RUN git init -q /w && cd /w \
 && git remote add origin "$UBERSDR_REPO" \
 && git sparse-checkout set clients/electron/whisper \
 && git fetch -q --depth 1 --filter=blob:none origin "$UBERSDR_COMMIT" \
 && git checkout -q FETCH_HEAD \
 && git init -q /wcpp && cd /wcpp \
 && git fetch -q --depth 1 "$WHISPER_CPP_REPO" "$WHISPER_CPP_COMMIT" \
 && git checkout -q FETCH_HEAD
RUN set -e; H=/w/clients/electron/whisper; mkdir -p /out; \
    STRIP=strip; CROSS=""; \
    if [ "$TARGETARCH" = arm64 ] && [ "$BUILDARCH" != arm64 ]; then \
      CROSS="-DCMAKE_SYSTEM_NAME=Linux -DCMAKE_SYSTEM_PROCESSOR=aarch64 -DCMAKE_C_COMPILER=aarch64-linux-gnu-gcc -DCMAKE_CXX_COMPILER=aarch64-linux-gnu-g++"; \
      STRIP=aarch64-linux-gnu-strip; \
    elif [ "$TARGETARCH" != "$BUILDARCH" ]; then \
      echo "whisper-helper: no cross build from $BUILDARCH to $TARGETARCH" >&2; exit 1; \
    fi; \
    build() { \
      cmake -S "$H" -B "/b/$1" -DCMAKE_BUILD_TYPE=Release -DWHISPER_CPP_DIR=/wcpp \
        -DWHISPER_CPP_COMMIT="$WHISPER_CPP_COMMIT" -DWHISPER_HELPER_VARIANT="$1" $CROSS $2 >/dev/null; \
      cmake --build "/b/$1" --target whisper-helper -j"$BUILD_JOBS" >/dev/null; \
    }; \
    case "$TARGETARCH" in \
      amd64) \
        build baseline "-DGGML_SSE42=OFF -DGGML_AVX=OFF -DGGML_AVX2=OFF -DGGML_FMA=OFF -DGGML_F16C=OFF -DGGML_BMI2=OFF -DGGML_AVX512=OFF -DGGML_AVX_VNNI=OFF -DWHISPER_HELPER_DISPATCH=ON"; \
        build avx2 "-DGGML_SSE42=ON -DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON -DGGML_BMI2=ON -DGGML_AVX512=OFF -DGGML_AVX_VNNI=OFF"; \
        cp /b/baseline/whisper-helper /out/whisper-helper; \
        cp /b/avx2/whisper-helper /out/whisper-helper-avx2 ;; \
      arm64) \
        build baseline "-DGGML_CPU_ARM_ARCH=armv8-a"; \
        cp /b/baseline/whisper-helper /out/whisper-helper ;; \
      *) echo "whisper-helper: no build for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    $STRIP --strip-unneeded /out/*; \
    if [ -z "$CROSS" ]; then /out/whisper-helper --version; fi

FROM whisper-${WITH_WHISPER} AS whisper

# ---------------------------------------------------------------------------
# Stage 1: build ubersdr_airsplice Go binary
# ---------------------------------------------------------------------------
FROM golang:1.24-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The tree now holds two packages -- main and internal/pcmv4 -- and
# `go build -o <file> ./...` refuses to write more than one. Build the
# main package; its dependency is compiled with it either way.
ARG VERSION=dev
RUN go build -ldflags "-X main.version=${VERSION}" -o /out/ubersdr_airsplice .

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        lame \
        wget \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -s /bin/false airsplice

COPY --from=go-builder /out/ubersdr_airsplice /usr/local/bin/ubersdr_airsplice

# whisper-helper when built WITH_WHISPER=1 (the directory is empty otherwise).
# On PATH, so WHISPER_HELPER's default finds it; the baseline x86-64 build
# hands over to whisper-helper-avx2 beside it when the CPU has AVX2.
COPY --from=whisper /out/ /opt/whisper/
ENV PATH="/opt/whisper:${PATH}"

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

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/bin/wget", "-q", "-O", "/dev/null", "http://localhost:6095/"]

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
