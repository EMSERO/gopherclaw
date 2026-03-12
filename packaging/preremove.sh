#!/bin/sh
# Runs as root during dpkg -r; attempt to stop the user service for common users.
# If SUDO_USER is set (typical sudo dpkg -i), target that user's session.
if [ -n "$SUDO_USER" ]; then
    SUDO_UID=$(id -u "$SUDO_USER" 2>/dev/null) || true
    if [ -n "$SUDO_UID" ]; then
        export XDG_RUNTIME_DIR="/run/user/$SUDO_UID"
        su - "$SUDO_USER" -c "systemctl --user stop gopherclaw 2>/dev/null; systemctl --user disable gopherclaw 2>/dev/null" || true
        exit 0
    fi
fi
# Fallback: best-effort for current user
systemctl --user stop gopherclaw 2>/dev/null || true
systemctl --user disable gopherclaw 2>/dev/null || true
