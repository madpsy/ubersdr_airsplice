#!/usr/bin/env bash
# stop.sh — stop the ubersdr_airsplice service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/airsplice"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_airsplice..."
docker compose down
echo "Done."
