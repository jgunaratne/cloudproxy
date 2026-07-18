#!/usr/bin/env bash
# Install a macOS launchd agent that runs push-token-to-pi.sh every 45
# minutes (tokens last 60), so the Pi always has a valid IAP token.
# Also runs once immediately on install.
#
# Override the target host at install time:  PI_HOST=mypi.local ./install-token-pusher.sh
#
# Uninstall:
#   launchctl bootout gui/$(id -u)/com.cloudproxy.token-pusher
#   rm ~/Library/LaunchAgents/com.cloudproxy.token-pusher.plist
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LABEL="com.cloudproxy.token-pusher"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME/Library/Logs/cloudproxy-token-pusher.log"

mkdir -p "$(dirname "$PLIST")"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>$SCRIPT_DIR/push-token-to-pi.sh</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PI_HOST</key><string>${PI_HOST:-pidesk.local}</string>
  </dict>
  <key>StartInterval</key><integer>2700</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>$LOG</string>
  <key>StandardErrorPath</key><string>$LOG</string>
</dict>
</plist>
EOF

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
echo "installed launchd agent $LABEL (pushes every 45 min; logs: $LOG)"
