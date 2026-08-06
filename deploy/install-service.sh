#!/usr/bin/env bash
# Install the Iron Forest daemon as a systemd --user service.
# Re-running this script updates the unit after a path or unit change.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"
unit="$HOME/.config/systemd/user/forest.service"
legacy_unit="$HOME/.config/systemd/user/forest-chew.service" # retired 2026-08-06; stop and remove so one machine never runs both.

mkdir -p "$(dirname "$unit")"
systemctl --user stop forest-chew.service >/dev/null 2>&1 || true
systemctl --user disable forest-chew.service >/dev/null 2>&1 || true
rm -f "$legacy_unit"
sed "s|@REPO_DIR@|$repo|g" "$here/forest.service" > "$unit"
systemctl --user daemon-reload
systemctl --user enable forest.service >/dev/null
systemctl --user restart forest.service
echo "$(basename "$0"): installed $unit (repo: $repo)"
echo "  status:  systemctl --user status forest.service"
echo "  logs:    journalctl --user -u forest.service -f"
