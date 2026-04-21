#!/bin/sh
# entrypoint.sh — translate environment variables into ubersdr_airsplice flags
#
# Environment variables:
#   UBERSDR_URL       UberSDR WebSocket URL (default: ws://ubersdr:8080/ws)
#   UBERSDR_PASS      UberSDR bypass password (optional)
#   OUTPUT_DIR        Output directory for recordings (default: /data)
#   WEB_PORT          Port for the web UI server (default: 6095)
#   SEGMENT_SECS      Rotate to a new WAV file every N seconds; 0 = continuous (default: 300)
#   CLEANUP_ALL_DAYS  Delete ALL recordings older than N days; 0 = disabled (default: 30)
#   UI_PASSWORD       Password for write actions in the web UI (optional)
#
# Channels are persisted in $OUTPUT_DIR/channels.json and managed via the web UI.
# No UBERSDR_CHANNELS env var is needed.

set -e

args=""

[ -n "$UBERSDR_URL"  ] && args="$args -url $UBERSDR_URL"
[ -n "$UBERSDR_PASS" ] && args="$args -password $UBERSDR_PASS"

OUTPUT_DIR="${OUTPUT_DIR:-/data}"
args="$args -output $OUTPUT_DIR"

[ -n "$SEGMENT_SECS"     ] && args="$args -segment-secs $SEGMENT_SECS"
[ -n "$CLEANUP_ALL_DAYS" ] && args="$args -cleanup-all-days $CLEANUP_ALL_DAYS"
[ -n "$UI_PASSWORD"      ] && args="$args -ui-password $UI_PASSWORD"

# WEB_PORT → -listen :<port>
if [ -n "$WEB_PORT" ]; then
    args="$args -listen :$WEB_PORT"
else
    args="$args -listen :6095"
fi

# Append any CLI args passed directly to the container
# shellcheck disable=SC2086
exec /usr/local/bin/ubersdr_airsplice $args "$@"
