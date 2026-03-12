#!/bin/sh
systemctl --user stop gopherclaw 2>/dev/null || true
systemctl --user disable gopherclaw 2>/dev/null || true
