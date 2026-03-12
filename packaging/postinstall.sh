#!/bin/sh
# Reload systemd to pick up the new service file.
# Runs as root; target the invoking user's session if possible.
if [ -n "$SUDO_USER" ]; then
    SUDO_UID=$(id -u "$SUDO_USER" 2>/dev/null) || true
    if [ -n "$SUDO_UID" ]; then
        export XDG_RUNTIME_DIR="/run/user/$SUDO_UID"
        su - "$SUDO_USER" -c "systemctl --user daemon-reload" 2>/dev/null || true
    fi
fi

echo ""
echo "GopherClaw installed. To enable auto-start:"
echo "  systemctl --user enable --now gopherclaw"
echo ""
