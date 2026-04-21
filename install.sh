#!/usr/bin/env bash
# install.sh — fetch the docker-compose.yml from the ubersdr_airsplice repo and start the service
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_airsplice/main/install.sh | bash
#   — or —
#   ./install.sh [--force-update]
#
# Options:
#   --force-update   Overwrite an existing docker-compose.yml (default: skip if present)
#
# When piping through bash, pass the flag via env var instead:
#   curl -fsSL ... | FORCE_UPDATE=1 bash

set -euo pipefail

REPO_RAW="https://raw.githubusercontent.com/madpsy/ubersdr_airsplice/main"
INSTALL_DIR="${HOME}/ubersdr/airsplice"
COMPOSE_FILE="docker-compose.yml"
FORCE_UPDATE="${FORCE_UPDATE:-0}"
CONFIG_PASS_FILE=".config_pass"

# Parse flags when run directly (not piped)
for arg in "$@"; do
    case "$arg" in
        --force-update) FORCE_UPDATE=1 ;;
        *) echo "Unknown argument: $arg" >&2; exit 1 ;;
    esac
done

die() { echo "error: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Dependency checks
# ---------------------------------------------------------------------------

command -v docker >/dev/null || die "docker not found in PATH — please install Docker first"
docker compose version >/dev/null 2>&1 || die "docker compose plugin not found — please install Docker Compose v2"

# ---------------------------------------------------------------------------
# Prepare install directory
# ---------------------------------------------------------------------------

mkdir -p "${INSTALL_DIR}"
cd "${INSTALL_DIR}"

# ---------------------------------------------------------------------------
# Generate or load the UI password
# ---------------------------------------------------------------------------

if [[ -f "${CONFIG_PASS_FILE}" ]]; then
    CONFIG_PASS="$(cat "${CONFIG_PASS_FILE}")"
    PASS_IS_NEW=0
else
    CONFIG_PASS="$(set +o pipefail; LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16)"
    echo "${CONFIG_PASS}" > "${CONFIG_PASS_FILE}"
    chmod 600 "${CONFIG_PASS_FILE}"
    PASS_IS_NEW=1
fi

# ---------------------------------------------------------------------------
# Fetch compose file
# ---------------------------------------------------------------------------

if [[ -f "${COMPOSE_FILE}" && "${FORCE_UPDATE}" != "1" ]]; then
    echo "${COMPOSE_FILE} already exists — skipping download (use --force-update to overwrite)"
else
    echo "Fetching ${COMPOSE_FILE} from GitHub..."
    curl -fsSL "${REPO_RAW}/${COMPOSE_FILE}" -o "${COMPOSE_FILE}"
    echo "Saved ${COMPOSE_FILE}"
fi

# ---------------------------------------------------------------------------
# Fetch helper scripts
# ---------------------------------------------------------------------------

for script in update.sh start.sh stop.sh restart.sh get-password.sh; do
    echo "Fetching ${script}..."
    curl -fsSL "${REPO_RAW}/${script}" -o "${script}"
    chmod +x "${script}"
    echo "Saved ${script}"
done

# ---------------------------------------------------------------------------
# Inject UI_PASSWORD into compose file
# ---------------------------------------------------------------------------

if grep -q "# UI_PASSWORD:" "${COMPOSE_FILE}"; then
    sed -i "s|# UI_PASSWORD:.*|UI_PASSWORD: \"${CONFIG_PASS}\"|" "${COMPOSE_FILE}"
elif grep -q "UI_PASSWORD:" "${COMPOSE_FILE}"; then
    sed -i "s|UI_PASSWORD:.*|UI_PASSWORD: \"${CONFIG_PASS}\"|" "${COMPOSE_FILE}"
else
    sed -i "s|    volumes:|      UI_PASSWORD: \"${CONFIG_PASS}\"\n    volumes:|" "${COMPOSE_FILE}"
fi
echo "UI_PASSWORD set in ${COMPOSE_FILE}"

# ---------------------------------------------------------------------------
# Create recordings directory on the host
# ---------------------------------------------------------------------------

RECORDINGS_DIR="recordings"
mkdir -p "${INSTALL_DIR}/${RECORDINGS_DIR}"
chmod 777 "${INSTALL_DIR}/${RECORDINGS_DIR}"
echo "Recordings directory ready: ${INSTALL_DIR}/${RECORDINGS_DIR}"

# ---------------------------------------------------------------------------
# Pull image and start service
# ---------------------------------------------------------------------------

echo "Pulling latest Docker image..."
docker compose pull

echo "Starting ubersdr_airsplice..."
docker compose up -d --remove-orphans --force-recreate

echo ""
echo "Done. ubersdr_airsplice is running."
echo "  View logs  : docker compose logs -f"
echo "  Stop       : ./stop.sh"
echo "  Start      : ./start.sh"
echo "  Restart    : ./restart.sh"
echo "  Update     : ./update.sh"
echo "  Password   : ./get-password.sh"
echo ""
echo "Edit ${INSTALL_DIR}/${COMPOSE_FILE} to configure UBERSDR_URL, UBERSDR_CHANNELS, etc."
echo "Then run ./restart.sh to apply changes."
echo ""
if [[ "${PASS_IS_NEW}" == "1" ]]; then
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  UI PASSWORD (auto-generated)"
    echo ""
    echo "  ${CONFIG_PASS}"
    echo ""
    echo "  This password protects write actions in the web UI (delete recordings)."
    echo "  It has been saved to: ${INSTALL_DIR}/${CONFIG_PASS_FILE}"
    echo ""
    echo "  To change it, edit UI_PASSWORD in ${INSTALL_DIR}/${COMPOSE_FILE}"
    echo "  and run ./restart.sh  (also update ${CONFIG_PASS_FILE} to match)."
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
else
    echo "  UI password loaded from ${INSTALL_DIR}/${CONFIG_PASS_FILE}"
fi
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  UBERSDR PROXY CONFIGURATION"
echo ""
echo "  This addon can be added as an UberSDR Proxy via the Admin interface:"
echo ""
echo "    Name              : airsplice"
echo "    Host              : airsplice"
echo "    Port              : 6095"
echo "    Enabled           : true"
echo "    Strip prefix      : true"
echo "    Rewrite websocket : false"
echo "    Rate Limit        : 250"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
