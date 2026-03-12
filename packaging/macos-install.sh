#!/bin/sh
# Install GopherClaw as a macOS LaunchAgent for the current user.
set -e

PLIST_SRC="$(dirname "$0")/gopherclaw.plist"
PLIST_DST="$HOME/Library/LaunchAgents/com.emsero.gopherclaw.plist"
LOG_DIR="/usr/local/var/log"

if [ ! -f "$PLIST_SRC" ]; then
    echo "Error: gopherclaw.plist not found at $PLIST_SRC" >&2
    exit 1
fi

if ! command -v gopherclaw >/dev/null 2>&1; then
    echo "Warning: gopherclaw binary not found in PATH" >&2
fi

# Ensure log directory exists
mkdir -p "$LOG_DIR"

# Ensure LaunchAgents directory exists
mkdir -p "$HOME/Library/LaunchAgents"

# Stop existing agent if loaded
launchctl bootout "gui/$(id -u)/com.emsero.gopherclaw" 2>/dev/null || true

# Template HOME into the plist
sed "s|/Users/CURRENT_USER|$HOME|g" "$PLIST_SRC" > "$PLIST_DST"

echo "Installed plist to $PLIST_DST"
echo ""
echo "To start GopherClaw now:"
echo "  launchctl bootstrap gui/$(id -u) $PLIST_DST"
echo ""
echo "It will auto-start on login. To stop:"
echo "  launchctl bootout gui/$(id -u)/com.emsero.gopherclaw"
