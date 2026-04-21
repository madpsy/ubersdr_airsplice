#!/usr/bin/env bash
# update.sh — pull the latest ubersdr_airsplice image and restart the service
#
# Usage:
#   ./update.sh

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/airsplice"

cd "${INSTALL_DIR}"
echo "Pulling latest ubersdr_airsplice image..."
docker compose pull
echo "Restarting service..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
echo "  Web UI    : http://localhost:6095"
