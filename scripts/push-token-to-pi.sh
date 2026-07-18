#!/usr/bin/env bash
# Mint a fresh IAP token on this machine (the Mac) and push it to the Pi.
# The pi-client re-reads the file on every reconnect when started with
# GCP_IDENTITY_TOKEN_FILE, so pushing a fresh token before the old one
# expires keeps the stream up with no manual copy/paste.
#
# Run once by hand to test, then install the schedule with
# scripts/install-token-pusher.sh.
#
# Requirements:
#   - same gcloud setup as mint-iap-token.sh (ADC + token creator role)
#   - key-based SSH to the Pi that works without a password prompt
#
# Config (env vars):
#   PI_HOST     ssh destination           (default: pidesk.local)
#   TOKEN_PATH  token file path on the Pi (default: .cloudproxy/iap-token,
#               relative to the Pi user's home)
set -euo pipefail

# launchd runs jobs with a minimal PATH; make sure gcloud is findable.
export PATH="$PATH:/usr/local/bin:/opt/homebrew/bin:$HOME/google-cloud-sdk/bin"

PI_HOST="${PI_HOST:-pidesk.local}"
TOKEN_PATH="${TOKEN_PATH:-.cloudproxy/iap-token}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TOKEN="$("$SCRIPT_DIR/mint-iap-token.sh")"

# Send the token over stdin (never in argv, where it would show up in ps
# on either machine) and write it atomically so the pi-client can't read
# a half-written file.
printf '%s\n' "$TOKEN" | ssh -o BatchMode=yes "$PI_HOST" \
  "mkdir -p \"\$(dirname '$TOKEN_PATH')\" && umask 077 && cat > '$TOKEN_PATH.tmp' && mv '$TOKEN_PATH.tmp' '$TOKEN_PATH'"

echo "$(date '+%F %T') pushed fresh IAP token to $PI_HOST:$TOKEN_PATH (valid ~1 hour)"
