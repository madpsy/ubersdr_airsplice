#!/usr/bin/env bash
# docker.sh — build the ubersdr_airsplice Docker image
#
# All binaries are built from source inside the Docker image.
# No host binaries are required.
#
# Usage:
#   ./docker.sh [build|push|run]
#
#   build  — build the image (default)
#   push   — build then push to registry (set IMAGE env var)
#   run    — run the image locally (set env vars below)
#
# Environment variables (build):
#   IMAGE      Docker image name/tag   (default: madpsy/ubersdr_airsplice:latest)
#   PLATFORM   Docker --platform flag  (default: linux/amd64)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

IMAGE="${IMAGE:-madpsy/ubersdr_airsplice:latest}"
PLATFORM="${PLATFORM:-linux/amd64}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() { echo "error: $*" >&2; exit 1; }

check_deps() {
    command -v docker >/dev/null || die "docker not found in PATH"
}

build() {
    check_deps

    TMPCTX="$(mktemp -d)"
    trap 'rm -rf "$TMPCTX"' EXIT

    echo "Staging build context in $TMPCTX..."

    rsync -a --exclude='.git' \
              --exclude='recordings' \
              --exclude='data' \
              --exclude='ubersdr_airsplice' \
              "$SCRIPT_DIR/" "$TMPCTX/"

    echo "Building image $IMAGE (platform=$PLATFORM)..."
    docker build \
        --platform "$PLATFORM" \
        --tag "$IMAGE" \
        "$TMPCTX"

    echo "Built: $IMAGE"
}

push() {
    build
    echo "Pushing $IMAGE..."
    docker push "$IMAGE"
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
    push)  push  ;;
    run)   shift; run_image "$@" ;;
    *)
        echo "Usage: $0 [build|push|run [args...]]" >&2
        exit 1
        ;;
esac
