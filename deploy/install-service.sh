#!/usr/bin/env bash
# Install the iron-forest factory daemon as a systemd --user service.
# Idempotent: safe to re-run after a path move or unit change.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"
unit="$HOME/.config/systemd/user/forest-chew.service"

mkdir -p "$(dirname "$unit")"
cp "$here/forest-chew.service" "$unit"
systemctl --user daemon-reload
systemctl --user enable forest-chew.service >/dev/null
echo "$(basename "$0"): installed $unit (repo: $repo)"
echo "  start:   systemctl --user start forest-chew"
echo "  status:  systemctl --user status forest-chew"
echo "  logs:    journalctl --user -u forest-chew -f"
echo "  stop:    systemctl --user stop forest-chew"
