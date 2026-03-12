#!/bin/sh
# Uninstall GopherClaw LaunchAgent for the current user.
set -e

PLIST_DST="$HOME/Library/LaunchAgents/com.emsero.gopherclaw.plist"

# Stop the agent if running
launchctl bootout "gui/$(id -u)/com.emsero.gopherclaw" 2>/dev/null || true

# Remove the plist
if [ -f "$PLIST_DST" ]; then
    rm "$PLIST_DST"
    echo "Removed $PLIST_DST"
else
    echo "No plist found at $PLIST_DST"
fi

echo ""
echo "GopherClaw LaunchAgent uninstalled."
echo "Logs may remain at /usr/local/var/log/gopherclaw.{log,err}"
