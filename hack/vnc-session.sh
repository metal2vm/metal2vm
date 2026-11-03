#!/usr/bin/env bash
# Runs vnc-type.py actions against a VM console through a one-shot
# `virtctl vnc --proxy-only` (it serves a single client), then screenshots.
# Usage: hack/vnc-session.sh <vm> <screenshot.png> <action>...   (namespace: $NS)
set -euo pipefail
NS="${NS:-metal2vm-test}"; VM="$1"; SHOT="$2"; shift 2
VIRTCTL="$(command -v virtctl || echo bin/virtctl)"
LOG="$(mktemp)"; PORT=$((5900 + RANDOM % 1000))
"$VIRTCTL" -n "$NS" vnc --proxy-only --port "$PORT" "$VM" >"$LOG" 2>&1 &
PROXY=$!
trap 'kill $PROXY 2>/dev/null || true; rm -f "$LOG"' EXIT
for _ in $(seq 1 50); do grep -q "\"port\"" "$LOG" && break; sleep 0.2; done
python3 "$(dirname "$0")/vnc-type.py" "$PORT" "$@"
sleep 1
"$VIRTCTL" -n "$NS" vnc screenshot "$VM" --file="$SHOT" >/dev/null 2>&1
