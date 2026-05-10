#!/usr/bin/env bash
# docker.sh — build the ubersdr_airsplice Docker image
#
# All binaries are built from source inside the Docker image.
# No host binaries are required.
#
# Usage:
#   ./docker.sh [build|push|run|arm64]
#
#   build  — build the image for linux/amd64 (default, local load)
#   arm64  — build the image for linux/arm64 (Raspberry Pi, Apple Silicon, etc., local load)
#   push   — build multi-arch (amd64 + arm64) with buildx and push manifest to registry
#   run    — run the image locally (set env vars below)
#
# Environment variables (build):
#   IMAGE      Docker image name/tag   (default: madpsy/ubersdr_airsplice:latest)
#   PLATFORM   Docker --platform flag  (default: linux/amd64)
#   BUILDER    buildx builder name     (default: ubersdr-multiarch)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

IMAGE="${IMAGE:-madpsy/ubersdr_airsplice:latest}"
PLATFORM="${PLATFORM:-linux/amd64}"
BUILDER="${BUILDER:-ubersdr-multiarch}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() { echo "error: $*" >&2; exit 1; }

check_deps() {
    command -v docker >/dev/null || die "docker not found in PATH"
}

# Ensure a buildx builder that supports multi-platform builds exists.
# If it already exists we just use it; we never delete existing builders.
ensure_builder() {
    if ! docker buildx inspect "$BUILDER" &>/dev/null; then
        echo "Creating buildx builder '$BUILDER'..."
        docker buildx create --name "$BUILDER" --driver docker-container --bootstrap
    else
        echo "Using existing buildx builder '$BUILDER'."
    fi
}

stage_context() {
    TMPCTX="$(mktemp -d)"
    trap 'rm -rf "$TMPCTX"' EXIT

    echo "Staging build context in $TMPCTX..."
    rsync -a --exclude='.git' \
              --exclude='recordings' \
              --exclude='data' \
              --exclude='ubersdr_airsplice' \
              "$SCRIPT_DIR/" "$TMPCTX/"
}

# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------

build() {
    check_deps
    stage_context

    echo "Building image $IMAGE (platform=$PLATFORM)..."
    docker build \
        --platform "$PLATFORM" \
        --tag "$IMAGE" \
        "$TMPCTX"

    echo "Built: $IMAGE"
}

push() {
    check_deps
    ensure_builder
    stage_context

    local platforms="linux/amd64,linux/arm64"
    echo "Building and pushing multi-arch image $IMAGE (platforms=$platforms)..."
    docker buildx build \
        --builder "$BUILDER" \
        --platform "$platforms" \
        --tag "$IMAGE" \
        --push \
        "$TMPCTX"

    echo "Pushed multi-arch manifest: $IMAGE"

    echo "Committing and pushing git repository..."
    git add -A
    git diff --cached --quiet || git commit -m "Release $IMAGE"
    git push
}

run_image() {
    local args=()

    [[ -n "${UBERSDR_URL:-}"      ]] && args+=(-e "UBERSDR_URL=$UBERSDR_URL")
    [[ -n "${UBERSDR_CHANNELS:-}" ]] && args+=(-e "UBERSDR_CHANNELS=$UBERSDR_CHANNELS")
    [[ -n "${UBERSDR_PASS:-}"     ]] && args+=(-e "UBERSDR_PASS=$UBERSDR_PASS")
    [[ -n "${OUTPUT_DIR:-}"       ]] && args+=(-e "OUTPUT_DIR=$OUTPUT_DIR")
    [[ -n "${WEB_PORT:-}"         ]] && args+=(-e "WEB_PORT=$WEB_PORT")
    [[ -n "${SEGMENT_SECS:-}"     ]] && args+=(-e "SEGMENT_SECS=$SEGMENT_SECS")
    [[ -n "${CLEANUP_ALL_DAYS:-}" ]] && args+=(-e "CLEANUP_ALL_DAYS=$CLEANUP_ALL_DAYS")
    [[ -n "${UI_PASSWORD:-}"      ]] && args+=(-e "UI_PASSWORD=$UI_PASSWORD")

    docker run --rm -it \
        --platform "$PLATFORM" \
        -p "${WEB_PORT:-6095}:${WEB_PORT:-6095}" \
        "${args[@]}" \
        "$IMAGE" \
        "$@"
}

# ---------------------------------------------------------------------------
# Environment variable reference (for docker run -e ...)
# ---------------------------------------------------------------------------
#
#   UBERSDR_URL       UberSDR WebSocket URL (default: ws://ubersdr:8080/ws)
#   UBERSDR_CHANNELS  Comma-separated freq:mode pairs, e.g. 7880000:usb,14300000:usb
#   UBERSDR_PASS      UberSDR bypass password (optional)
#   OUTPUT_DIR        Output directory for recordings (default: /data)
#   WEB_PORT          Web UI port (default: 6095)
#   SEGMENT_SECS      WAV segment length in seconds; 0 = continuous (default: 300)
#   CLEANUP_ALL_DAYS  Delete recordings older than N days; 0 = disabled (default: 30)
#   UI_PASSWORD       Password for write actions in the web UI (optional)

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

case "${1:-build}" in
    build) build ;;
    arm64) PLATFORM=linux/arm64 build ;;
    push)  push  ;;
    run)   shift; run_image "$@" ;;
    *)
        echo "Usage: $0 [build|arm64|push|run [args...]]" >&2
        exit 1
        ;;
esac
