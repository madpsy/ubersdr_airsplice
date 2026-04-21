#!/usr/bin/env bash
# get-password.sh — display the current UI_PASSWORD for the ubersdr_airsplice web UI
#
# Usage:
#   ./get-password.sh

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/airsplice"
CONFIG_PASS_FILE="${INSTALL_DIR}/.config_pass"

if [[ ! -f "${CONFIG_PASS_FILE}" ]]; then
    echo "error: password file not found at ${CONFIG_PASS_FILE}" >&2
    echo "       Has install.sh been run yet?" >&2
    exit 1
fi

CONFIG_PASS="$(cat "${CONFIG_PASS_FILE}")"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  UI PASSWORD"
echo ""
echo "  ${CONFIG_PASS}"
echo ""
echo "  This password protects write actions in the web UI (delete recordings)."
echo "  Stored at: ${CONFIG_PASS_FILE}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
